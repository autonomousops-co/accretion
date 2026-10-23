package k8s

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// Watch-driven status collection (#96).
//
// The eight status collectors used to poll: every k8s.poll_interval, one LIST
// per watched namespace per resource -- roughly 10N+1 calls per cycle, issued
// whether or not anything had changed, and a change was seen up to a whole
// interval late. Each kind is now driven by client-go shared informers: a LIST
// and a long-lived WATCH per resource, with every object re-delivered from the
// cache each resync.
//
// Which namespaces an informer covers follows the scope. An explicit static
// scope (config.k8s.namespaces, the common case) gets one informer per
// namespace, so the cache holds exactly what the poll loop iterated and
// nothing of the rest of a shared cluster; a label or "*" scope, whose
// membership changes, gets one cluster-wide informer with the scope applied to
// each object as it arrives. Both paths still run the object through
// inScope, so the set written is identical either way. The trade is nine watch
// streams per namespace on the static path (pods, deployments, replicasets,
// HPAs, endpointslices, PVCs, configmaps, jobs, cronjobs; nodes and PVs are
// cluster-scoped and open once) -- served from the apiserver's watch cache and
// multiplexed over HTTP/2, though the apiserver caps streams per connection
// (http2-max-streams-per-connection, default 250), so at twenty-five
// namespaces client-go transparently opens a second TLS connection -- against
// a cache that grows with the cluster rather than the scope on the other.
// Measured at 30 namespaces: 302 informers, ~17 MiB of heap and stacks, ~2,700
// goroutines, ~10 s to sync. Those figures predate Secrets leaving scope: 302
// is 30x10 plus the two cluster-scoped informers, so they were taken while a
// tenth per-namespace stream was still open. Read them as an upper bound on
// the nine listed above, not a fresh measurement (#132). It works; the status
// command warns once at startup past about twenty namespaces so an operator
// knows what a long static list costs. This is the one place those numbers
// live.
//
// What is deliberately preserved: every object is still converted by the same
// function for every path, so the entries -- and therefore the resource_type
// values, the Data JSON, the content hash and the deterministic timestamp --
// are byte-identical to what polling wrote. The resync period is the old poll
// interval: at least once per interval every object is re-delivered from the
// cache, which is the "re-evaluate everything" guarantee a poll gave, and the
// workers' content-hash dedup discards the unchanged ones exactly as it
// discarded unchanged poll results.
//
// What is deliberately different: failure is loud. A Forbidden or
// Unauthorized on any LIST or WATCH is fatal at once -- a 403 is not
// transient, it is a ClusterRole that does not grant the resource -- and any
// other failure that repeats without a single watch event arriving in between
// is fatal after a few attempts. Either way the unit returns an error, the
// supervisor restarts it, and readiness stops claiming Ready, where the poll
// loop logged an error per cycle and carried on.
//
// What is watched for: a shared informer retains what it is given for the
// life of the process, where a poll held it for one cycle. Every informer
// therefore strips managedFields, which nothing here reads and which can be
// most of an object's bytes; the ConfigMap informer goes further and keeps
// name, namespace, uid, resourceVersion, creation time, labels and the
// annotations SafeAnnotations admits -- so no .data is ever resident, and no
// annotation that could carry it either. Secrets are not collected at all.

// NamespaceFilter reports whether a namespaced object is in scope. Workers pass
// their nsscope.Scope's Contains.
type NamespaceFilter func(namespace string) bool

// Tunables. Variables rather than constants so the tests can shrink them.
var (
	// statusSyncTimeout bounds how long a unit's informers may take to complete
	// their initial LIST before one fails the unit. It is fixed, not derived
	// from the resync period: how long a LIST takes has nothing to do with how
	// often the cache is replayed, and tying the two together would let a long
	// poll_interval turn a slow apiserver into an hours-long silent wait. Two
	// minutes is generous for a paginated LIST of a large cluster and, with the
	// supervisor's UnhealthyAfter of 3, takes /ready down within minutes. A
	// Forbidden does not wait for it; that is fatal on sight.
	statusSyncTimeout = 2 * time.Minute
	// maxWatchFailures is how many consecutive list/watch failures, with no
	// watch event delivered in between, fail an informer. A transient blip is
	// one or two attempts; a watch that never delivers is every attempt. The
	// count is reset only by a delivered event, never by a LIST succeeding:
	// the reflector re-LISTs after every failed watch, so LIST progress is what
	// a permanently broken watch looks like, not proof of health.
	maxWatchFailures = 5
)

// statusInformer is one resource's shared informer plus the failure accounting
// the supervisor relies on. Every informer is required: one that cannot sync
// or keeps failing fails its unit, whatever the resource.
type statusInformer struct {
	kind     string
	informer cache.SharedIndexInformer
	handler  func(obj interface{})

	// progress is set by the watch proxy on every delivered event and cleared
	// when a failure is accounted, so the consecutive-failure count resets
	// only when the watch has demonstrably worked since the last failure.
	progress atomic.Bool

	mu       sync.Mutex
	failures int
	fatal    chan<- error
}

func newStatusInformer(
	kind string, lw cache.ListerWatcher, exemplar runtime.Object,
	resync time.Duration, indexers cache.Indexers,
	handler func(obj interface{}),
) *statusInformer {
	if indexers == nil {
		indexers = cache.Indexers{}
	}
	s := &statusInformer{kind: kind, handler: handler}
	tracked := &progressListerWatcher{inner: cache.ToListerWatcherWithContext(lw), mark: func() { s.progress.Store(true) }}
	s.informer = cache.NewSharedIndexInformer(tracked, exemplar, resync, indexers)
	// Neither errors before the informer has started, and it has not.
	_ = s.informer.SetWatchErrorHandler(s.onWatchError)
	_ = s.informer.SetTransform(stripManagedFields)
	return s
}

// setTransform replaces the default managedFields-only transform. It must run
// before the informer starts, and it does: only the constructors call it.
func (s *statusInformer) setTransform(fn cache.TransformFunc) {
	_ = s.informer.SetTransform(fn)
}

// onWatchError is the reflector's failure callback: every failed ListAndWatch
// lands here. A Forbidden or Unauthorized is fatal immediately. Anything else
// counts, and the count resets only when a watch event was delivered since
// the previous failure -- see maxWatchFailures for why LIST progress does not.
func (s *statusInformer) onWatchError(_ *cache.Reflector, err error) {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		logger.Logger.Error("Status informer denied by the API server; "+
			"the ClusterRole must grant list and watch on this resource",
			zap.String("kind", s.kind), zap.Error(err))
		s.reportFatal(fmt.Errorf("%s informer: %w; the ClusterRole must grant list and watch on this resource",
			s.kind, err))
		return
	}

	s.mu.Lock()
	if s.progress.Swap(false) {
		s.failures = 0
	}
	s.failures++
	n := s.failures
	s.mu.Unlock()

	logger.Logger.Warn("Status informer list/watch failed",
		zap.String("kind", s.kind), zap.Int("consecutive_failures", n), zap.Error(err))
	if n >= maxWatchFailures {
		s.reportFatal(fmt.Errorf("%s informer: %d consecutive list/watch failures with no event delivered: %w",
			s.kind, n, err))
	}
}

func (s *statusInformer) reportFatal(err error) {
	if s.fatal == nil {
		return
	}
	select {
	case s.fatal <- err:
	default:
	}
}

func (s *statusInformer) addHandler() {
	_, _ = s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.handler,
		UpdateFunc: func(_, newObj interface{}) { s.handler(newObj) },
		// Deletes are ignored: a status snapshot of an object that is gone is not
		// a record the pollers ever wrote either.
	})
}

// progressListerWatcher wraps a ListerWatcher so every event a watch delivers
// marks progress on its informer. The reflector offers no success callback,
// only a failure one, and a re-LIST after a failed watch looks exactly like
// progress from the outside; the watch stream itself is the only honest
// signal.
type progressListerWatcher struct {
	inner cache.ListerWatcherWithContext
	mark  func()
}

// The four signatures below are cache.ListerWatcher's and pass ListOptions by
// value, so the hugeParam advice cannot be taken.

func (p *progressListerWatcher) List(
	options metav1.ListOptions, //nolint:gocritic // interface signature
) (runtime.Object, error) {
	return p.ListWithContext(context.Background(), options)
}

func (p *progressListerWatcher) Watch(
	options metav1.ListOptions, //nolint:gocritic // interface signature
) (watch.Interface, error) {
	return p.WatchWithContext(context.Background(), options)
}

func (p *progressListerWatcher) ListWithContext(
	ctx context.Context, options metav1.ListOptions, //nolint:gocritic // interface signature
) (runtime.Object, error) {
	return p.inner.ListWithContext(ctx, options)
}

func (p *progressListerWatcher) WatchWithContext(
	ctx context.Context, options metav1.ListOptions, //nolint:gocritic // interface signature
) (watch.Interface, error) {
	w, err := p.inner.WatchWithContext(ctx, options)
	if err != nil {
		return nil, err
	}
	return newProgressWatch(w, p.mark), nil
}

// progressWatch forwards a watch stream, marking every delivered event (a
// Bookmark counts: the stream is alive) and never an Error event.
type progressWatch struct {
	inner    watch.Interface
	out      chan watch.Event
	stopped  chan struct{}
	stopOnce sync.Once
}

func newProgressWatch(inner watch.Interface, mark func()) *progressWatch {
	p := &progressWatch{inner: inner, out: make(chan watch.Event), stopped: make(chan struct{})}
	go func() {
		defer close(p.out)
		for {
			select {
			case ev, ok := <-inner.ResultChan():
				if !ok {
					return
				}
				if ev.Type != watch.Error {
					mark()
				}
				select {
				case p.out <- ev:
				case <-p.stopped:
					return
				}
			case <-p.stopped:
				return
			}
		}
	}()
	return p
}

func (p *progressWatch) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopped)
		p.inner.Stop()
	})
}

func (p *progressWatch) ResultChan() <-chan watch.Event { return p.out }

// runStatusInformers drives one unit's informers until ctx ends or an
// informer fails. It returns ctx.Err() on a clean stop, which is what the
// supervisor treats as shutdown rather than failure.
//
// Event handlers are registered only after every informer in the unit has
// synced. A shared informer replays its whole store to a handler added after
// start, so the effect is one complete pass over the initial state -- and,
// for the kinds that join two resources (a Deployment with its ReplicaSets),
// no entry is ever built from a store that is still filling.
func runStatusInformers(ctx context.Context, infs ...*statusInformer) error {
	fatal := make(chan error, len(infs))
	stop := make(chan struct{})
	defer close(stop)

	for _, s := range infs {
		s.fatal = fatal
		go s.informer.Run(stop)
	}

	timeout := statusSyncTimeout
	deadline := time.After(timeout)
	for _, s := range infs {
		if err := waitSynced(ctx, s, deadline, timeout, fatal); err != nil {
			return err
		}
	}
	for _, s := range infs {
		s.addHandler()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-fatal:
		return err
	}
}

func waitSynced(
	ctx context.Context, s *statusInformer, deadline <-chan time.Time, timeout time.Duration, fatal <-chan error,
) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.informer.HasSynced() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-fatal:
			return err
		case <-deadline:
			return fmt.Errorf("%s informer did not sync within %s; "+
				"is the API server reachable and the ClusterRole granting list and watch on this resource?",
				s.kind, timeout)
		case <-tick.C:
		}
	}
}

// stripManagedFields is every informer's default store transform. Nothing
// here reads managedFields, and on a real object they are commonly the
// largest single field. Mutating in place is what client-go documents as safe
// for a transform: the object is the reflector's own decode, seen by nothing
// else yet. Idempotent, and anything without object metadata (a tombstone)
// passes through.
func stripManagedFields(obj interface{}) (interface{}, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
	}
	return obj, nil
}

// metaOnly builds the ObjectMeta a Secret or ConfigMap store entry keeps:
// what the informer needs to key and version it, what the conversions read,
// labels, and the annotations the kind's allowlist admits. Nothing else is
// copied -- no managedFields, no ownerReferences, no finalizers -- and in
// particular no annotation that could carry the object's body, which is how
// a kubectl-applied Secret's values would otherwise ride its metadata into
// the store, the entry and the row.
func metaOnly(m *metav1.ObjectMeta, annotations func(map[string]string) map[string]string) metav1.ObjectMeta {
	out := metav1.ObjectMeta{
		Name:              m.Name,
		Namespace:         m.Namespace,
		UID:               m.UID,
		ResourceVersion:   m.ResourceVersion,
		CreationTimestamp: m.CreationTimestamp,
		Annotations:       annotations(m.Annotations),
	}
	if len(m.Labels) > 0 {
		out.Labels = make(map[string]string, len(m.Labels))
		for k, v := range m.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

// stripConfigMapForStore is the ConfigMap informer's store transform: every
// ConfigMap's .data and .binaryData cluster-wide would otherwise sit in memory
// for nothing, since configMapEntry reads metadata only.
func stripConfigMapForStore(obj interface{}) (interface{}, error) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return obj, nil
	}
	return &corev1.ConfigMap{
		TypeMeta:   cm.TypeMeta,
		ObjectMeta: metaOnly(&cm.ObjectMeta, SafeAnnotations),
	}, nil
}

func newConfigMapInformer(lw cache.ListerWatcher, resync time.Duration, handler func(obj interface{})) *statusInformer {
	inf := newStatusInformer("configmaps", lw, &corev1.ConfigMap{}, resync, nil, handler)
	inf.setTransform(stripConfigMapForStore)
	return inf
}

// listWatchers is one ListerWatcher per namespace on the static path, or a
// single cluster-wide one under the empty key.
type listWatchers map[string]cache.ListerWatcher

// namespaced builds one T per explicit namespace, or one cluster-wide T under
// the empty key when the list is empty (a dynamic scope). T is a
// ListerWatcher for most kinds and the {deployments, replicaSets} pair for
// the deployment join.
func namespaced[T any](namespaces []string, mk func(namespace string) T) map[string]T {
	if len(namespaces) == 0 {
		return map[string]T{"": mk(metav1.NamespaceAll)}
	}
	out := make(map[string]T, len(namespaces))
	for _, ns := range namespaces {
		out[ns] = mk(ns)
	}
	return out
}

// listWatch builds a ListerWatcher from a typed client's List and Watch.
func listWatch[L runtime.Object](
	list func(context.Context, metav1.ListOptions) (L, error),
	w func(context.Context, metav1.ListOptions) (watch.Interface, error),
) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return list(ctx, o)
		},
		WatchFuncWithContext: w,
	}
}

// sortedKeys returns a map's keys in order, so informer sets start in a
// deterministic order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// WatchPodStatuses delivers a PodStatusEntry for every in-scope pod on the
// initial list, on every change, and again at each resync, until ctx ends.
// namespaces is the explicit static scope, one informer each; empty means a
// dynamic scope, one cluster-wide informer filtered by inScope.
func (c *Client) WatchPodStatuses(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter, emit func(*PodStatusEntry),
) error {
	lws := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		pods := c.clientset.CoreV1().Pods(ns)
		return listWatch(pods.List, pods.Watch)
	})
	return watchPodStatuses(ctx, lws, resync, inScope, emit)
}

func watchPodStatuses(
	ctx context.Context, lws listWatchers, resync time.Duration,
	inScope NamespaceFilter, emit func(*PodStatusEntry),
) error {
	infs := make([]*statusInformer, 0, len(lws))
	for _, k := range sortedKeys(lws) {
		infs = append(infs, newStatusInformer("pods", lws[k], &corev1.Pod{}, resync, nil, func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok || !inScope(pod.Namespace) {
				return
			}
			e := podStatusEntry(pod, time.Now())
			emit(&e)
		}))
	}
	return runStatusInformers(ctx, infs...)
}

// WatchNodeStatuses is the cluster-scoped equivalent for nodes.
func (c *Client) WatchNodeStatuses(ctx context.Context, resync time.Duration, emit func(*NodeStatusEntry)) error {
	nodes := c.clientset.CoreV1().Nodes()
	return watchNodeStatuses(ctx, listWatch(nodes.List, nodes.Watch), resync, emit)
}

func watchNodeStatuses(
	ctx context.Context, lw cache.ListerWatcher, resync time.Duration, emit func(*NodeStatusEntry),
) error {
	inf := newStatusInformer("nodes", lw, &corev1.Node{}, resync, nil, func(obj interface{}) {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return
		}
		e := nodeStatusEntry(node, time.Now())
		emit(&e)
	})
	return runStatusInformers(ctx, inf)
}

// indexReplicaSetOwner indexes ReplicaSets by the UID of the Deployment that
// owns them, which is how the List path grouped them.
const indexReplicaSetOwner = "owner-deployment-uid"

func replicaSetOwnerIndex(obj interface{}) ([]string, error) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		return nil, nil
	}
	var keys []string
	for _, ref := range rs.OwnerReferences {
		if ref.Kind == kindDeployment {
			keys = append(keys, string(ref.UID))
		}
	}
	return keys, nil
}

// deploymentSources is the pair of ListerWatchers one namespace's deployment
// join needs. Built as a pair so the two can never be keyed apart: a
// Deployment's ReplicaSets live in its own namespace, and each pair's two
// informers only ever join each other.
type deploymentSources struct {
	deployments cache.ListerWatcher
	replicaSets cache.ListerWatcher
}

// WatchDeploymentStatuses joins Deployments with their owned ReplicaSets. A
// change to either re-emits the Deployment.
func (c *Client) WatchDeploymentStatuses(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
	emit func(*DeploymentStatusEntry),
) error {
	sources := namespaced(namespaces, func(ns string) deploymentSources {
		d := c.clientset.AppsV1().Deployments(ns)
		r := c.clientset.AppsV1().ReplicaSets(ns)
		return deploymentSources{
			deployments: listWatch(d.List, d.Watch),
			replicaSets: listWatch(r.List, r.Watch),
		}
	})
	return watchDeploymentStatuses(ctx, sources, resync, inScope, emit)
}

func watchDeploymentStatuses(
	ctx context.Context, sources map[string]deploymentSources, resync time.Duration,
	inScope NamespaceFilter, emit func(*DeploymentStatusEntry),
) error {
	deployInfs := make(map[string]*statusInformer, len(sources))
	rsInfs := make(map[string]*statusInformer, len(sources))
	infs := make([]*statusInformer, 0, 2*len(sources))

	for _, k := range sortedKeys(sources) {
		key := k
		src := sources[key]
		emitDeployment := func(deploy *appsv1.Deployment) {
			if !inScope(deploy.Namespace) {
				return
			}
			objs, err := rsInfs[key].informer.GetIndexer().ByIndex(indexReplicaSetOwner, string(deploy.UID))
			if err != nil {
				return
			}
			// nil when there are none, as the map lookup in the List path was, so
			// the JSON is identical; and name order, which is the order a LIST
			// returns and the index does not promise.
			var sets []ReplicaSetInfo
			for _, o := range objs {
				if rs, ok := o.(*appsv1.ReplicaSet); ok {
					sets = append(sets, replicaSetInfo(rs))
				}
			}
			sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
			e := deploymentStatusEntry(deploy, sets, time.Now())
			emit(&e)
		}

		deployInfs[key] = newStatusInformer("deployments", src.deployments, &appsv1.Deployment{}, resync, nil,
			func(obj interface{}) {
				if deploy, ok := obj.(*appsv1.Deployment); ok {
					emitDeployment(deploy)
				}
			})
		rsInfs[key] = newStatusInformer("replicasets", src.replicaSets, &appsv1.ReplicaSet{}, resync,
			cache.Indexers{indexReplicaSetOwner: replicaSetOwnerIndex},
			func(obj interface{}) {
				rs, ok := obj.(*appsv1.ReplicaSet)
				if !ok {
					return
				}
				for _, ref := range rs.OwnerReferences {
					if ref.Kind != kindDeployment {
						continue
					}
					o, exists, err := deployInfs[key].informer.GetStore().GetByKey(rs.Namespace + "/" + ref.Name)
					if err != nil || !exists {
						continue
					}
					if deploy, ok := o.(*appsv1.Deployment); ok && deploy.UID == ref.UID {
						emitDeployment(deploy)
					}
				}
			})
		infs = append(infs, deployInfs[key], rsInfs[key])
	}
	return runStatusInformers(ctx, infs...)
}

// WatchHPAStatuses delivers HPA entries.
func (c *Client) WatchHPAStatuses(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter, emit func(*HPAStatusEntry),
) error {
	lws := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		h := c.clientset.AutoscalingV2().HorizontalPodAutoscalers(ns)
		return listWatch(h.List, h.Watch)
	})
	return watchHPAStatuses(ctx, lws, resync, inScope, emit)
}

func watchHPAStatuses(
	ctx context.Context, lws listWatchers, resync time.Duration,
	inScope NamespaceFilter, emit func(*HPAStatusEntry),
) error {
	infs := make([]*statusInformer, 0, len(lws))
	for _, k := range sortedKeys(lws) {
		infs = append(infs, newStatusInformer("horizontalpodautoscalers", lws[k], &autoscalingv2.HorizontalPodAutoscaler{},
			resync, nil, func(obj interface{}) {
				hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
				if !ok || !inScope(hpa.Namespace) {
					return
				}
				e := buildHPAEntry(hpa, time.Now())
				emit(&e)
			}))
	}
	return runStatusInformers(ctx, infs...)
}

// indexEndpointSliceService indexes EndpointSlices by "namespace/service", the
// grouping the List path applied. Slices without the service label are left
// out of the index, as they were skipped by the List path.
const indexEndpointSliceService = "service"

func endpointSliceServiceIndex(obj interface{}) ([]string, error) {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil, nil
	}
	svc := slice.Labels[discoveryv1.LabelServiceName]
	if svc == "" {
		return nil, nil
	}
	return []string{slice.Namespace + "/" + svc}, nil
}

// WatchEndpointHealth delivers one EndpointHealthEntry per Service, rebuilt
// from every slice of that Service whenever any of them changes.
func (c *Client) WatchEndpointHealth(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
	emit func(*EndpointHealthEntry),
) error {
	lws := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		s := c.clientset.DiscoveryV1().EndpointSlices(ns)
		return listWatch(s.List, s.Watch)
	})
	return watchEndpointHealth(ctx, lws, resync, inScope, emit)
}

func watchEndpointHealth(
	ctx context.Context, lws listWatchers, resync time.Duration,
	inScope NamespaceFilter, emit func(*EndpointHealthEntry),
) error {
	infs := make([]*statusInformer, 0, len(lws))
	for _, k := range sortedKeys(lws) {
		var inf *statusInformer
		inf = newStatusInformer("endpointslices", lws[k], &discoveryv1.EndpointSlice{}, resync,
			cache.Indexers{indexEndpointSliceService: endpointSliceServiceIndex},
			func(obj interface{}) {
				changed, ok := obj.(*discoveryv1.EndpointSlice)
				if !ok || !inScope(changed.Namespace) {
					return
				}
				svc := changed.Labels[discoveryv1.LabelServiceName]
				if svc == "" {
					return
				}
				objs, err := inf.informer.GetIndexer().ByIndex(indexEndpointSliceService, changed.Namespace+"/"+svc)
				if err != nil {
					return
				}
				slices := make([]discoveryv1.EndpointSlice, 0, len(objs))
				for _, o := range objs {
					if s, ok := o.(*discoveryv1.EndpointSlice); ok {
						slices = append(slices, *s)
					}
				}
				// Name order is the order a LIST returns; the endpoint order inside the
				// entry, and so its content hash, depends on it.
				sort.Slice(slices, func(i, j int) bool { return slices[i].Name < slices[j].Name })
				e := buildEndpointHealthEntry(changed.Namespace, svc, slices, time.Now())
				emit(&e)
			})
		infs = append(infs, inf)
	}
	return runStatusInformers(ctx, infs...)
}

// WatchVolumeStatuses delivers PVC entries for in-scope namespaces and PV
// entries for the whole cluster, which is what the pvc-status unit polled.
func (c *Client) WatchVolumeStatuses(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
	emitPVC func(*PVCStatusEntry), emitPV func(*PVStatusEntry),
) error {
	pvcs := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		p := c.clientset.CoreV1().PersistentVolumeClaims(ns)
		return listWatch(p.List, p.Watch)
	})
	pvs := c.clientset.CoreV1().PersistentVolumes()
	return watchVolumeStatuses(ctx, pvcs, listWatch(pvs.List, pvs.Watch), resync, inScope, emitPVC, emitPV)
}

func watchVolumeStatuses(
	ctx context.Context, pvcLWs listWatchers, pvLW cache.ListerWatcher, resync time.Duration, inScope NamespaceFilter,
	emitPVC func(*PVCStatusEntry), emitPV func(*PVStatusEntry),
) error {
	infs := make([]*statusInformer, 0, len(pvcLWs)+1)
	for _, k := range sortedKeys(pvcLWs) {
		infs = append(infs, newStatusInformer("persistentvolumeclaims", pvcLWs[k], &corev1.PersistentVolumeClaim{},
			resync, nil, func(obj interface{}) {
				pvc, ok := obj.(*corev1.PersistentVolumeClaim)
				if !ok || !inScope(pvc.Namespace) {
					return
				}
				e := pvcStatusEntry(pvc, time.Now())
				emitPVC(&e)
			}))
	}
	infs = append(infs, newStatusInformer("persistentvolumes", pvLW, &corev1.PersistentVolume{}, resync, nil,
		func(obj interface{}) {
			pv, ok := obj.(*corev1.PersistentVolume)
			if !ok {
				return
			}
			e := pvStatusEntry(pv, time.Now())
			emitPV(&e)
		}))
	return runStatusInformers(ctx, infs...)
}

// WatchConfigMetadata delivers ConfigMap metadata entries.
//
// Kubernetes Secrets are deliberately NOT collected and the chart grants no
// verb on them: cluster secrets are out of scope for monitoring, so the widest
// grant a status role could carry does not exist. Accretion's own components
// still authenticate with credentials the kubelet resolves from Secrets via
// secretKeyRef, which needs no RBAC at all.
//
// SECURITY: only the metadata metaOnly keeps is read, and the store transform
// keeps it that way -- a kubectl-applied ConfigMap's last-applied-configuration
// annotation carries its own data, which SafeAnnotations drops.
func (c *Client) WatchConfigMetadata(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
	emitConfigMap func(*ConfigMapEntry),
) error {
	cms := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		m := c.clientset.CoreV1().ConfigMaps(ns)
		return listWatch(m.List, m.Watch)
	})
	return watchConfigMetadata(ctx, cms, resync, inScope, emitConfigMap)
}

func watchConfigMetadata(
	ctx context.Context, cmLWs listWatchers, resync time.Duration, inScope NamespaceFilter,
	emitConfigMap func(*ConfigMapEntry),
) error {
	var infs []*statusInformer
	for _, k := range sortedKeys(cmLWs) {
		infs = append(infs, newConfigMapInformer(cmLWs[k], resync, func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok || !inScope(cm.Namespace) {
				return
			}
			e := configMapEntry(cm, time.Now())
			emitConfigMap(&e)
		}))
	}
	return runStatusInformers(ctx, infs...)
}

// WatchJobStatuses delivers Job and CronJob entries.
func (c *Client) WatchJobStatuses(
	ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
	emitJob func(*JobStatusEntry), emitCronJob func(*CronJobStatusEntry),
) error {
	jobs := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		j := c.clientset.BatchV1().Jobs(ns)
		return listWatch(j.List, j.Watch)
	})
	cronJobs := namespaced(namespaces, func(ns string) cache.ListerWatcher {
		cj := c.clientset.BatchV1().CronJobs(ns)
		return listWatch(cj.List, cj.Watch)
	})
	return watchJobStatuses(ctx, jobs, cronJobs, resync, inScope, emitJob, emitCronJob)
}

func watchJobStatuses(
	ctx context.Context, jobLWs, cronJobLWs listWatchers, resync time.Duration, inScope NamespaceFilter,
	emitJob func(*JobStatusEntry), emitCronJob func(*CronJobStatusEntry),
) error {
	var infs []*statusInformer
	for _, k := range sortedKeys(jobLWs) {
		infs = append(infs, newStatusInformer("jobs", jobLWs[k], &batchv1.Job{}, resync, nil, func(obj interface{}) {
			job, ok := obj.(*batchv1.Job)
			if !ok || !inScope(job.Namespace) {
				return
			}
			e := buildJobStatusEntry(job, time.Now())
			emitJob(&e)
		}))
	}
	for _, k := range sortedKeys(cronJobLWs) {
		infs = append(infs, newStatusInformer("cronjobs", cronJobLWs[k], &batchv1.CronJob{}, resync, nil,
			func(obj interface{}) {
				cj, ok := obj.(*batchv1.CronJob)
				if !ok || !inScope(cj.Namespace) {
					return
				}
				e := buildCronJobStatusEntry(cj, time.Now())
				emitCronJob(&e)
			}))
	}
	return runStatusInformers(ctx, infs...)
}
