package k8s

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// These tests drive the informer runners through cache.ListWatch fakes and
// the REAL reflector (a fake clientset is not vendored). What they establish:
//
//   - the initial LIST is delivered through the same conversion every path
//     uses, and a fixed expected entry pins that conversion's output;
//   - a change arrives through the WATCH promptly, not at the next resync;
//   - resync re-delivers unchanged objects, as a poll re-listed them;
//   - the namespace filter applies per object, and a static scope opens one
//     namespaced informer per namespace rather than a cluster-wide one;
//   - a Forbidden on LIST or on WATCH is fatal on sight, through the
//     reflector; a watch that fails repeatedly with no event delivered is
//     fatal even while LISTs keep succeeding; a slow LIST hits the sync
//     timeout;
//   - every store strips managedFields, and the Secret and ConfigMap stores
//     hold metadata only: .data never lands in the store or the entry;
//   - the joined kinds (Deployment + ReplicaSets, EndpointSlices per Service)
//     are rebuilt from the whole store in name order.

// quietLogger initializes the global logger exactly once for the package. Per
// test would race: a reflector from an earlier test that is still retrying
// logs through the global while the next test replaces it.
var loggerOnce sync.Once

func quietLogger(t *testing.T) {
	t.Helper()
	loggerOnce.Do(func() {
		if err := logger.Initialize(config.LoggingConfig{Level: "error", Format: "json", DisableStacktrace: true}); err != nil {
			t.Fatalf("logger: %v", err)
		}
	})
}

// fakeLW is a ListerWatcher over a list function and a FakeWatcher the test
// can push events into. watchErr, when set, fails every Watch call.
type fakeLW struct {
	mu       sync.Mutex
	list     func() runtime.Object
	listErr  error
	watchErr error
	watcher  *watch.FakeWatcher
	lists    atomic.Int32
	watches  atomic.Int32
}

func (f *fakeLW) lw() cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(context.Context, metav1.ListOptions) (runtime.Object, error) {
			f.lists.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.listErr != nil {
				return nil, f.listErr
			}
			return f.list(), nil
		},
		WatchFuncWithContext: func(context.Context, metav1.ListOptions) (watch.Interface, error) {
			f.watches.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.watchErr != nil {
				return nil, f.watchErr
			}
			f.watcher = watch.NewFakeWithChanSize(16, false)
			return f.watcher, nil
		},
	}
}

// send waits for the reflector to have opened a watch, then pushes through it.
func (f *fakeLW) send(t *testing.T, fn func(w *watch.FakeWatcher)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		w := f.watcher
		f.mu.Unlock()
		if w != nil {
			fn(w)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reflector never opened a watch")
}

func recv[T any](t *testing.T, ch <-chan T, within time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(within):
		t.Fatalf("no delivery within %s", within)
	}
	var zero T
	return zero
}

func expectNone[T any](t *testing.T, ch <-chan T, within time.Duration) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected delivery: %+v", v)
	case <-time.After(within):
	}
}

func shrinkSyncTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := statusSyncTimeout
	statusSyncTimeout = d
	t.Cleanup(func() { statusSyncTimeout = old })
}

func shrinkWatchFailures(t *testing.T, n int) {
	t.Helper()
	old := maxWatchFailures
	maxWatchFailures = n
	t.Cleanup(func() { maxWatchFailures = old })
}

func allNamespaces(string) bool { return true }

func clusterWide(f *fakeLW) listWatchers { return listWatchers{"": f.lw()} }

var created = metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("no grant"))
}

func pod(ns, name, rv string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name),
			ResourceVersion: rv, CreationTimestamp: created,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}},
		Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "up", LastTransitionTime: created}}},
	}
}

func podList(pods ...*corev1.Pod) func() runtime.Object {
	return func() runtime.Object {
		l := &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
		for _, p := range pods {
			l.Items = append(l.Items, *p)
		}
		return l
	}
}

func TestWatchPodStatuses_ListThenWatchThenFilter(t *testing.T) {
	quietLogger(t)
	in := pod("in", "a", "1", corev1.PodRunning)
	out := pod("out", "b", "1", corev1.PodRunning)
	f := &fakeLW{list: podList(in, out)}

	got := make(chan PodStatusEntry, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- watchPodStatuses(ctx, clusterWide(f), time.Hour, func(ns string) bool { return ns == "in" },
			func(e *PodStatusEntry) { got <- *e })
	}()

	// Initial list: the in-scope pod. The expectation is written out, not
	// computed with the conversion under test, so a slip inside podStatusEntry
	// fails here rather than passing on both sides.
	e := recv(t, got, 5*time.Second)
	want := PodStatusEntry{
		Timestamp: created.Time,
		Namespace: "in",
		PodName:   "a",
		Phase:     "Running",
		Conditions: []PodConditionInfo{{
			Type: "Ready", Status: "True", Reason: "up", LastTransitionTime: created.Time,
		}},
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("initial entry:\n got %+v\nwant %+v", e, want)
	}
	if !reflect.DeepEqual(e, podStatusEntry(in, time.Now())) {
		t.Fatalf("watch path and conversion disagree")
	}
	expectNone(t, got, 200*time.Millisecond) // the out-of-scope pod never arrives

	// A change comes through the watch well inside the (one hour) resync.
	changed := in.DeepCopy()
	changed.ResourceVersion = "2"
	changed.Status.Phase = corev1.PodFailed
	start := time.Now()
	f.send(t, func(w *watch.FakeWatcher) { w.Modify(changed) })
	e = recv(t, got, 5*time.Second)
	if e.Phase != "Failed" {
		t.Fatalf("expected the modified pod, got %+v", e)
	}
	if lag := time.Since(start); lag > 2*time.Second {
		t.Fatalf("change took %s to arrive; it came from a resync, not the watch", lag)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on a clean stop, got %v", err)
	}
}

func TestWatchPodStatuses_StaticScopeOpensOneInformerPerNamespace(t *testing.T) {
	quietLogger(t)
	a := &fakeLW{list: podList(pod("a", "pa", "1", corev1.PodRunning))}
	b := &fakeLW{list: podList(pod("b", "pb", "1", corev1.PodPending))}
	got := make(chan PodStatusEntry, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = watchPodStatuses(ctx, listWatchers{"a": a.lw(), "b": b.lw()}, time.Hour, allNamespaces,
			func(e *PodStatusEntry) { got <- *e })
	}()
	seen := map[string]string{}
	for len(seen) < 2 {
		e := recv(t, got, 5*time.Second)
		seen[e.Namespace] = e.PodName
	}
	if seen["a"] != "pa" || seen["b"] != "pb" {
		t.Fatalf("unexpected deliveries %v", seen)
	}
	if a.lists.Load() != 1 || b.lists.Load() != 1 {
		t.Fatalf("each namespace must be listed exactly once: a=%d b=%d", a.lists.Load(), b.lists.Load())
	}
	// A cluster-wide informer would have needed a single ListerWatcher; the
	// static path opened one per namespace and nothing else.
	if a.watches.Load() != 1 || b.watches.Load() != 1 {
		t.Fatalf("each namespace must hold one watch: a=%d b=%d", a.watches.Load(), b.watches.Load())
	}
}

func TestWatchPodStatuses_ResyncRedelivers(t *testing.T) {
	quietLogger(t)
	f := &fakeLW{list: podList(pod("ns", "a", "1", corev1.PodRunning))}
	got := make(chan PodStatusEntry, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = watchPodStatuses(ctx, clusterWide(f), time.Second, allNamespaces, func(e *PodStatusEntry) { got <- *e })
	}()
	first := recv(t, got, 5*time.Second)
	second := recv(t, got, 3*time.Second)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("resync must re-deliver the unchanged object identically:\n%+v\n%+v", first, second)
	}
}

func TestInformer_StoreStripsManagedFields(t *testing.T) {
	quietLogger(t)
	f := &fakeLW{list: podList(pod("ns", "a", "1", corev1.PodRunning))}
	got := make(chan struct{}, 4)
	inf := newStatusInformer("pods", f.lw(), &corev1.Pod{}, time.Hour, nil, func(interface{}) { got <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runStatusInformers(ctx, inf) }()
	recv(t, got, 5*time.Second)
	items := inf.informer.GetStore().List()
	if len(items) != 1 {
		t.Fatalf("expected one stored pod, got %d", len(items))
	}
	stored, ok := items[0].(*corev1.Pod)
	if !ok {
		t.Fatalf("store holds %T", items[0])
	}
	if stored.ManagedFields != nil {
		t.Fatalf("store retains managedFields: %v", stored.ManagedFields)
	}
	if stored.Name != "a" || stored.Status.Phase != corev1.PodRunning {
		t.Fatalf("store lost the object: %+v", stored)
	}
}

func TestRunStatusInformers_ForbiddenListIsFatalOnSight(t *testing.T) {
	quietLogger(t)
	shrinkSyncTimeout(t, time.Minute) // must not be what ends this
	f := &fakeLW{listErr: forbidden("pods")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := watchPodStatuses(ctx, clusterWide(f), time.Hour, allNamespaces, func(*PodStatusEntry) {})
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("expected the Forbidden to surface, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("a forbidden LIST took %s to fail; it must not wait for the sync timeout", time.Since(start))
	}
}

func TestRunStatusInformers_ForbiddenWatchIsFatalThroughTheReflector(t *testing.T) {
	quietLogger(t)
	f := &fakeLW{list: podList(pod("ns", "a", "1", corev1.PodRunning)), watchErr: forbidden("pods")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err := watchPodStatuses(ctx, clusterWide(f), time.Hour, allNamespaces, func(*PodStatusEntry) {})
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("a forbidden WATCH must fail the unit through the reflector, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s; the 403 was not treated as fatal on sight", time.Since(start))
	}
}

func TestRunStatusInformers_RepeatedWatchFailureIsFatalDespiteListProgress(t *testing.T) {
	quietLogger(t)
	shrinkWatchFailures(t, 2)
	var rv atomic.Int32
	f := &fakeLW{watchErr: errors.New("watch stream broke")}
	// Every re-LIST advances the resource version, exactly what a cluster with
	// churn looks like while a watch keeps failing. The old accounting reset
	// on that and never fired.
	f.list = func() runtime.Object {
		n := rv.Add(1)
		l := &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: fmt.Sprint(n)}}
		l.Items = append(l.Items, *pod("ns", "a", fmt.Sprint(n), corev1.PodRunning))
		return l
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := watchPodStatuses(ctx, clusterWide(f), time.Hour, allNamespaces, func(*PodStatusEntry) {})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a consecutive-failure fatal, got %v", err)
	}
	if !strings.Contains(err.Error(), "consecutive") {
		t.Fatalf("unexpected failure: %v", err)
	}
	if f.lists.Load() < 2 {
		t.Fatalf("the reflector should have re-listed between failures; lists=%d", f.lists.Load())
	}
}

func TestStatusInformer_DeliveredEventResetsTheCount(t *testing.T) {
	quietLogger(t)
	f := &fakeLW{list: podList()}
	s := newStatusInformer("pods", f.lw(), &corev1.Pod{}, time.Hour, nil, func(interface{}) {})
	fatal := make(chan error, 1)
	s.fatal = fatal
	generic := errors.New("boom")
	for i := 0; i < maxWatchFailures-1; i++ {
		s.onWatchError(nil, generic)
	}
	s.progress.Store(true) // a watch event arrived
	s.onWatchError(nil, generic)
	select {
	case err := <-fatal:
		t.Fatalf("a delivered event must reset the count, but fatal fired: %v", err)
	default:
	}
	for i := 0; i < maxWatchFailures-1; i++ {
		s.onWatchError(nil, generic)
	}
	select {
	case <-fatal:
	default:
		t.Fatalf("no fatal after %d failures with no event in between", maxWatchFailures)
	}
}

func TestRunStatusInformers_SlowListHitsTheSyncTimeout(t *testing.T) {
	quietLogger(t)
	shrinkSyncTimeout(t, 300*time.Millisecond)
	f := &fakeLW{listErr: errors.New("apiserver timeout")} // retried with backoff, never Forbidden
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := watchPodStatuses(ctx, clusterWide(f), time.Hour, allNamespaces, func(*PodStatusEntry) {})
	if err == nil || !strings.Contains(err.Error(), "did not sync") {
		t.Fatalf("expected the sync timeout, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("failure took %s; the sync timeout did not bound it", time.Since(start))
	}
}

func TestConfigMapInformer_StoreHoldsMetadataOnly(t *testing.T) {
	quietLogger(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns", ResourceVersion: "9", CreationTimestamp: created,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}},
		Data:       map[string]string{"config.yaml": "big: payload"},
		BinaryData: map[string][]byte{"blob": {1, 2, 3}},
	}
	f := &fakeLW{list: func() runtime.Object {
		return &corev1.ConfigMapList{ListMeta: metav1.ListMeta{ResourceVersion: "9"}, Items: []corev1.ConfigMap{*cm}}
	}}
	got := make(chan ConfigMapEntry, 16)
	inf := newConfigMapInformer(f.lw(), time.Hour, func(obj interface{}) {
		if c, ok := obj.(*corev1.ConfigMap); ok {
			got <- configMapEntry(c, time.Now())
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runStatusInformers(ctx, inf) }()

	e := recv(t, got, 5*time.Second)
	if want := configMapEntry(cm, time.Now()); !reflect.DeepEqual(e, want) {
		t.Fatalf("entry differs from the conversion of the same object:\n got %+v\nwant %+v", e, want)
	}
	items := inf.informer.GetStore().List()
	if len(items) != 1 {
		t.Fatalf("expected one stored object, got %d", len(items))
	}
	stored, ok := items[0].(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("store holds %T", items[0])
	}
	if stored.Data != nil || stored.BinaryData != nil || stored.ManagedFields != nil {
		t.Fatalf("store retains stripped fields: %+v", stored)
	}
	if stored.Name != "app" || stored.ResourceVersion != "9" {
		t.Fatalf("store lost metadata: %+v", stored.ObjectMeta)
	}
}

func replicaSet(ns, name, rv string, owner *appsv1.Deployment, ready int32) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, ResourceVersion: rv, CreationTimestamp: created,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: owner.Name, UID: owner.UID}}},
		Status: appsv1.ReplicaSetStatus{Replicas: 2, ReadyReplicas: ready},
	}
}

func TestWatchDeploymentStatuses_JoinsOwnedReplicaSetsInNameOrder(t *testing.T) {
	quietLogger(t)
	two := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: "uid-web", ResourceVersion: "1", CreationTimestamp: created},
		Spec:       appsv1.DeploymentSpec{Replicas: &two},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2, UpdatedReplicas: 2},
	}
	rsB := replicaSet("ns", "web-b", "1", deploy, 2)
	rsA := replicaSet("ns", "web-a", "1", deploy, 0)
	orphan := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "lonely", Namespace: "ns", UID: "uid-lonely",
		ResourceVersion: "1", CreationTimestamp: created}}

	deploys := &fakeLW{list: func() runtime.Object {
		return &appsv1.DeploymentList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.Deployment{*deploy, *orphan}}
	}}
	sets := &fakeLW{list: func() runtime.Object {
		// Listed b before a: the entry must still carry them in name order.
		return &appsv1.ReplicaSetList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.ReplicaSet{*rsB, *rsA}}
	}}

	got := make(chan DeploymentStatusEntry, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sources := map[string]deploymentSources{"": {deployments: deploys.lw(), replicaSets: sets.lw()}}
		_ = watchDeploymentStatuses(ctx, sources, time.Hour, allNamespaces,
			func(e *DeploymentStatusEntry) { got <- *e })
	}()

	byName := map[string]DeploymentStatusEntry{}
	for len(byName) < 2 {
		e := recv(t, got, 5*time.Second)
		byName[e.DeploymentName] = e
	}
	wantWeb := DeploymentStatusEntry{
		Timestamp: created.Time, Namespace: "ns", DeploymentName: "web",
		ReplicaSets: []ReplicaSetInfo{
			{Name: "web-a", Revision: "1", Replicas: 2, ReadyReplicas: 0},
			{Name: "web-b", Revision: "1", Replicas: 2, ReadyReplicas: 2},
		},
		Replicas: DeploymentReplicaCounts{Desired: 2, Ready: 2, Available: 2, Updated: 2},
	}
	if !reflect.DeepEqual(byName["web"], wantWeb) {
		t.Fatalf("deployment entry:\n got %+v\nwant %+v", byName["web"], wantWeb)
	}
	if byName["lonely"].ReplicaSets != nil {
		t.Fatalf("a deployment with no ReplicaSets must carry nil, not %v (JSON differs)", byName["lonely"].ReplicaSets)
	}

	// A ReplicaSet change re-emits its Deployment with the new counts.
	rsA2 := rsA.DeepCopy()
	rsA2.ResourceVersion = "2"
	rsA2.Status.ReadyReplicas = 2
	sets.send(t, func(w *watch.FakeWatcher) { w.Modify(rsA2) })
	for {
		e := recv(t, got, 5*time.Second)
		if e.DeploymentName == "web" && len(e.ReplicaSets) == 2 && e.ReplicaSets[0].ReadyReplicas == 2 {
			break
		}
	}
}

func slice(ns, name, svc string, ready bool, addr string) *discoveryv1.EndpointSlice {
	labels := map[string]string{}
	if svc != "" {
		labels[discoveryv1.LabelServiceName] = svc
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, ResourceVersion: "1", CreationTimestamp: created, Labels: labels},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{addr}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
}

func TestWatchEndpointHealth_AggregatesSlicesPerServiceInNameOrder(t *testing.T) {
	quietLogger(t)
	sB := slice("ns", "web-b", "web", false, "10.0.0.2")
	sA := slice("ns", "web-a", "web", true, "10.0.0.1")
	unlabeled := slice("ns", "stray", "", true, "10.0.0.9")
	f := &fakeLW{list: func() runtime.Object {
		return &discoveryv1.EndpointSliceList{ListMeta: metav1.ListMeta{ResourceVersion: "1"},
			Items: []discoveryv1.EndpointSlice{*sB, *sA, *unlabeled}}
	}}
	got := make(chan EndpointHealthEntry, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = watchEndpointHealth(ctx, clusterWide(f), time.Hour, allNamespaces, func(e *EndpointHealthEntry) { got <- *e })
	}()
	e := recv(t, got, 5*time.Second)
	want := EndpointHealthEntry{
		Timestamp: created.Time, Namespace: "ns", ServiceName: "web",
		Endpoints: []EndpointInfo{
			{Addresses: []string{"10.0.0.1"}, Conditions: EndpointConditionInfo{Ready: true}},
			{Addresses: []string{"10.0.0.2"}, Conditions: EndpointConditionInfo{Ready: false}},
		},
		ReadyCount: 1, NotReadyCount: 1, TotalCount: 2,
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("endpoint entry:\n got %+v\nwant %+v", e, want)
	}
	// Every slice of the service triggers a rebuild of the same entry; the
	// unlabeled slice triggers nothing. So at most one more, identical, entry.
	for {
		select {
		case more := <-got:
			if !reflect.DeepEqual(more, want) {
				t.Fatalf("unexpected entry %+v", more)
			}
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}
}
