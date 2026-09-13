package config

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// IncludeAllNamespaces is the sentinel Include entry meaning "every namespace in
// the cluster". When it appears in NamespaceSelector.Include, Expand replaces it
// with the concrete list of all namespace names from the cluster, giving the
// "all-namespaces-except" semantics: all namespaces, then Exclude/ExcludeLabels
// subtract. Resolve/Allows treat a literal "*" as an ordinary name, so callers
// must Expand before resolving.
const IncludeAllNamespaces = "*"

// NamespaceSelector selects which Kubernetes namespaces a data source ingests.
// A namespace can be selected by name (Include/Exclude), by label selector
// (IncludeLabels/ExcludeLabels), or with the "*" sentinel for all namespaces.
// Exclude always wins over any form of include.
//
// It is consumed two ways so a single model serves every source:
//   - Polled sources (events, metrics, logs, *-status, configmaps, endpoints)
//     and the pod-discovery controller call Resolve to get the concrete list of
//     namespaces to iterate (the controller then filters pods against it with
//     slices.Contains). Empty Include means "nothing" for these, so the
//     controller never watches ALL namespaces.
//   - The push-based trace worker calls Allows to filter each incoming record by
//     namespace.
//
// Resolve and Allows are pure and name-only: they operate on concrete names
// AFTER label/"*" expansion. Callers must first call Expand (which lists
// namespaces from the Kubernetes API) to turn IncludeLabels, ExcludeLabels, and
// the "*" sentinel into concrete names. Expand makes ZERO API calls when none of
// those features are used, so name-only deployments keep today's behavior and
// need no extra RBAC.
//
// A single Expand is only a point-in-time snapshot: namespaces created, deleted,
// or relabeled afterwards are not reflected. Rather than call Expand directly,
// production callers go through internal/nsscope, which re-Expands on a ticker
// (namespaces.refresh_interval) and serves the latest resolution to readers, so
// scope tracks namespace lifecycle without a restart. See issue #152.
type NamespaceSelector struct {
	// Include lists namespace names to ingest. The "*" sentinel
	// (IncludeAllNamespaces) means "all namespaces" and is expanded by Expand. An
	// empty Include means "all namespaces" for push sources (Allows); for polled
	// sources (Resolve) it means "nothing", since polling cannot enumerate
	// namespaces without listing them — this preserves today's allowlist-only
	// behavior. After Expand replaces "*" with the concrete namespace list, this
	// empty-Include divergence is moot: "*" yields an explicit "all" list that
	// both Resolve and Allows honor identically.
	Include []string `yaml:"include" mapstructure:"include"`
	// Exclude lists namespace names to never ingest. Exclude always wins over
	// Include, for both Resolve and Allows.
	Exclude []string `yaml:"exclude" mapstructure:"exclude"`
	// IncludeLabels lists standard Kubernetes label-selector strings (e.g.
	// "env=prod,tier!=infra"). A namespace is label-included if it matches ANY
	// entry (OR across entries). Expand resolves each entry to concrete namespace
	// names via the Kubernetes API.
	IncludeLabels []string `yaml:"include_labels" mapstructure:"include_labels"`
	// ExcludeLabels lists standard Kubernetes label-selector strings. A namespace
	// is label-excluded if it matches ANY entry, and exclusion always wins over
	// any include (name, "*", or label). Expand resolves each entry to concrete
	// namespace names via the Kubernetes API.
	ExcludeLabels []string `yaml:"exclude_labels" mapstructure:"exclude_labels"`

	// includeConfigured records that an include allowlist WAS configured (names,
	// label selectors, or "*") on the selector this one was expanded from. It
	// disambiguates the two ways Include can end up empty, which Allows would
	// otherwise conflate:
	//
	//   - No allowlist configured at all  => empty Include means "allow all".
	//   - An allowlist configured that resolved to ZERO namespaces => must allow
	//     NOTHING. Collapsing to "allow all" here would silently invert an
	//     allowlist into a firehose — e.g. traces scoped by include_labels whose
	//     last matching namespace loses the label would begin ingesting every
	//     namespace in the cluster on the next refresh.
	//
	// It is set only by Expand (never deserialized), so a hand-built or
	// name-only selector keeps the historical empty-Include => allow-all
	// behavior. See issue #152.
	includeConfigured bool
}

// NamespaceLister lists namespace names, optionally filtered by a label
// selector (empty string = all namespaces). Implemented by the k8s client.
type NamespaceLister interface {
	ListNamespaces(ctx context.Context, labelSelector string) ([]string, error)
}

// hasNonBlankSelector reports whether sels contains at least one non-blank
// label-selector string. Blank entries are ignored everywhere (they would
// match every namespace), so they never by themselves warrant an API call.
func hasNonBlankSelector(sels []string) bool {
	for _, s := range sels {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// NeedsExpansion reports whether Expand would make any namespace-listing API
// call for this selector — i.e. the "*" all-namespaces sentinel is present, or
// a non-blank include/exclude label selector is set. Callers can use this to
// avoid constructing a Kubernetes client (and its startup failure modes) when
// the selector is purely name-based and needs no resolution.
func (s NamespaceSelector) NeedsExpansion() bool {
	return slices.Contains(s.Include, IncludeAllNamespaces) ||
		hasNonBlankSelector(s.IncludeLabels) ||
		hasNonBlankSelector(s.ExcludeLabels)
}

// nsAccumulator collects namespace names in first-seen order, skipping blanks
// and duplicates. Expand builds an include set and an exclude set the same way,
// so the dedup bookkeeping and the blank-selector guard live here once.
type nsAccumulator struct {
	seen  map[string]struct{}
	names []string
}

func newNSAccumulator(capacity int) *nsAccumulator {
	return &nsAccumulator{
		names: make([]string, 0, capacity),
		seen:  make(map[string]struct{}, capacity),
	}
}

func (a *nsAccumulator) add(ns string) {
	if ns == "" {
		return
	}
	if _, dup := a.seen[ns]; dup {
		return
	}
	a.seen[ns] = struct{}{}
	a.names = append(a.names, ns)
}

func (a *nsAccumulator) addAll(names []string) {
	for _, ns := range names {
		a.add(ns)
	}
}

// addMatching adds every namespace matching each label selector. Blank entries
// are skipped: an empty label selector matches EVERY namespace, so a stray ""
// (a Helm value that templated to empty, say) would silently balloon an include
// to all namespaces, or an exclude to all of them — scoping the source to
// nothing. Mirrors the empty-string guard in add.
func (a *nsAccumulator) addMatching(
	ctx context.Context, lister NamespaceLister, selectors []string, kind string,
) error {
	for _, selector := range selectors {
		if strings.TrimSpace(selector) == "" {
			continue
		}
		matched, err := lister.ListNamespaces(ctx, selector)
		if err != nil {
			return fmt.Errorf("listing namespaces for %s label selector %q: %w", kind, selector, err)
		}
		a.addAll(matched)
	}
	return nil
}

// Expand resolves the label selectors and the "*" sentinel into a purely
// name-based NamespaceSelector, snapshotting the current set of namespaces from
// the cluster. The returned selector has empty IncludeLabels/ExcludeLabels and
// is safe to hand to Resolve/Allows.
//
// FAST PATH: when IncludeLabels and ExcludeLabels are both empty AND Include
// contains no "*" sentinel, the selector is already name-based, so Expand
// returns it unchanged and makes ZERO lister calls. This keeps name-only
// deployments byte-identical and free of any namespace-list RBAC requirement.
//
// Otherwise Expand builds:
//   - Include = union (order-preserving, deduped) of the non-"*" name entries,
//     all namespaces (when "*" is present), and every IncludeLabels match.
//   - Exclude = union (deduped) of the name excludes and every ExcludeLabels
//     match. (Exclusion is applied later by Resolve/Allows, which always let
//     Exclude win.)
//
// Any lister error is wrapped and returned — Expand never silently falls back to
// an empty or all-namespaces scope, so a resolution failure surfaces loudly.
//
// The result reflects the cluster as of this call. Callers that need the scope
// to track namespace lifecycle should use internal/nsscope, which re-Expands
// periodically, rather than holding one Expand result forever.
func (s NamespaceSelector) Expand(ctx context.Context, lister NamespaceLister) (NamespaceSelector, error) {
	if !s.NeedsExpansion() {
		return s, nil
	}

	include := newNSAccumulator(len(s.Include))
	for _, ns := range s.Include {
		if ns == IncludeAllNamespaces {
			continue
		}
		include.add(ns)
	}
	if slices.Contains(s.Include, IncludeAllNamespaces) {
		all, err := lister.ListNamespaces(ctx, "")
		if err != nil {
			return NamespaceSelector{}, fmt.Errorf("listing all namespaces for %q include: %w", IncludeAllNamespaces, err)
		}
		include.addAll(all)
	}
	if err := include.addMatching(ctx, lister, s.IncludeLabels, "include"); err != nil {
		return NamespaceSelector{}, err
	}

	exclude := newNSAccumulator(len(s.Exclude))
	exclude.addAll(s.Exclude)
	if err := exclude.addMatching(ctx, lister, s.ExcludeLabels, "exclude"); err != nil {
		return NamespaceSelector{}, err
	}

	// Record that an allowlist was configured, so Allows can tell "no allowlist"
	// (allow all) from "allowlist that matched nothing" (allow nothing). Blank
	// label entries are ignored here for the same reason addMatching skips them:
	// they select nothing and express no intent.
	return NamespaceSelector{
		Include:           include.names,
		Exclude:           exclude.names,
		includeConfigured: len(s.Include) > 0 || hasNonBlankSelector(s.IncludeLabels),
	}, nil
}

// Resolve returns the concrete namespaces a polled source should iterate:
// Include with every Exclude entry removed, de-duplicated and order-preserving.
// Blank entries are skipped. An empty Include yields nil (poll nothing). Resolve
// is name-only: a literal "*" entry that reaches it unexpanded is treated as an
// ordinary name, so callers must Expand first to honor the all-namespaces
// sentinel.
func (s NamespaceSelector) Resolve() []string {
	if len(s.Include) == 0 {
		return nil
	}
	excluded := make(map[string]struct{}, len(s.Exclude))
	for _, ns := range s.Exclude {
		if ns != "" {
			excluded[ns] = struct{}{}
		}
	}
	resolved := make([]string, 0, len(s.Include))
	seen := make(map[string]struct{}, len(s.Include))
	for _, ns := range s.Include {
		if ns == "" {
			continue
		}
		if _, bad := excluded[ns]; bad {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		resolved = append(resolved, ns)
	}
	return resolved
}

// Allows reports whether a push-based source should ingest a record from
// namespace ns. Exclude always wins. A non-empty Include restricts to its
// members. When an Include allowlist is configured the empty namespace is never
// allowed — an untagged record cannot be confirmed to originate from an included
// namespace.
//
// An empty Include is interpreted by intent (see includeConfigured): if no
// allowlist was ever configured it allows every namespace (minus Exclude, the
// historical behavior); but if an allowlist WAS configured and merely resolved
// to zero namespaces, it allows nothing. Without that distinction a label-scoped
// source whose labels stop matching would silently widen to every namespace
// instead of going quiet. See issue #152.
func (s NamespaceSelector) Allows(ns string) bool {
	for _, ex := range s.Exclude {
		if ns == ex {
			return false
		}
	}
	if len(s.Include) == 0 {
		return !s.includeConfigured
	}
	for _, in := range s.Include {
		if ns == in {
			return true
		}
	}
	return false
}

// NamespaceScopeConfig is the unified namespace-scoping model shared by every
// collector data source. The embedded NamespaceSelector is the base include/
// exclude for polled sources; Traces overrides the base Include for the
// push-based trace worker so trace ingestion stays scoped independently (the
// charts set it to "*" minus the self namespace); Logs adds per-source excludes
// for the log worker on top of the base include.
type NamespaceScopeConfig struct {
	NamespaceSelector `yaml:",inline" mapstructure:",squash"`
	Traces            NamespaceSelector `yaml:"traces" mapstructure:"traces"`
	// Logs carries log-worker-specific overrides. Its Include is currently
	// unused (logs share the base Include so metrics and logs watch the same
	// namespaces); its Exclude is merged into LogNamespaceSelector so operators
	// can drop namespaces from log ingestion only.
	Logs NamespaceSelector `yaml:"logs" mapstructure:"logs"`
	// SelfNamespace is accretion's own namespace (where the collectors and Postgres
	// run, e.g. accretion-core). It is excluded from the LOG selector only — never
	// from the metrics/status (K8s) selector — so accretion can observe its own
	// namespace's metrics/status/events/configmaps/endpoints while the log
	// worker can never ingest its own log lines and feed the amplification loop
	// (log worker emits logs -> ingests them -> emits more). On traces it is not
	// belt-and-suspenders but the control of record: the shipped trace scope is
	// "*", so this exclude is the only thing keeping accretion's own spans out.
	// Empty ("") opts out of self-exclusion. See issue #145.
	SelfNamespace string `yaml:"self_namespace" mapstructure:"self_namespace"`
	// RefreshInterval is how often label selectors and the "*" sentinel are
	// re-resolved against the Kubernetes API, as a duration string (e.g. "5m").
	// This is what lets a namespace created after startup come into scope, and a
	// deleted or relabeled one drop out, without restarting the worker (#152).
	// "0" (or empty) disables refreshing, leaving the startup snapshot in place.
	//
	// It costs nothing on name-only configs: those need no resolution at all, so
	// they never list namespaces and never start a refresh loop, whatever this is
	// set to. Lower values track namespace churn more tightly at the price of one
	// LIST per configured label selector per worker per interval.
	RefreshInterval string `yaml:"refresh_interval" mapstructure:"refresh_interval"`
}

// MinNamespaceRefreshInterval is the shortest accepted refresh cadence. Every
// refresh issues one namespace LIST per configured label selector, per worker
// process; a sub-second interval would turn a typo ("5ms" for "5m") into a
// sustained flood of API-server requests from every worker at once. Refreshing
// namespace membership is not latency-critical, so the floor costs nothing real.
const MinNamespaceRefreshInterval = 10 * time.Second

// NamespaceRefreshInterval returns how often namespace label/"*" scoping should
// be re-resolved. An empty value means "do not refresh" (0), which is also what
// an explicit "0" parses to; the "5m" default comes from viper, so a config
// assembled without it (tests, embedders) opts out rather than silently starting
// a refresh loop. A malformed duration is an error — a typo here must surface at
// startup instead of quietly pinning the scope to its startup snapshot — as is a
// positive interval below MinNamespaceRefreshInterval, which is far more likely
// to be a unit slip than a real intent to LIST namespaces hundreds of times a
// second.
func (c *CollectorConfig) NamespaceRefreshInterval() (time.Duration, error) {
	if c.Namespaces.RefreshInterval == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.Namespaces.RefreshInterval)
	if err != nil {
		return 0, fmt.Errorf("invalid namespaces.refresh_interval %q: %w", c.Namespaces.RefreshInterval, err)
	}
	if d > 0 && d < MinNamespaceRefreshInterval {
		return 0, fmt.Errorf(
			"namespaces.refresh_interval %q is below the %s minimum; use 0 to disable refreshing",
			c.Namespaces.RefreshInterval, MinNamespaceRefreshInterval)
	}
	return d, nil
}

// effectiveExclude combines the base Exclude with any per-source extra excludes
// (e.g. the trace-specific excludes for the trace worker).
func (c *CollectorConfig) effectiveExclude(extra ...string) []string {
	ex := make([]string, 0, len(c.Namespaces.Exclude)+len(extra))
	ex = append(ex, c.Namespaces.Exclude...)
	ex = append(ex, extra...)
	return ex
}

// effectiveExcludeLabels combines the base ExcludeLabels with any per-source
// extra exclude-label selectors, mirroring effectiveExclude for label matching.
func (c *CollectorConfig) effectiveExcludeLabels(extra ...string) []string {
	ex := make([]string, 0, len(c.Namespaces.ExcludeLabels)+len(extra))
	ex = append(ex, c.Namespaces.ExcludeLabels...)
	ex = append(ex, extra...)
	return ex
}

// K8sNamespaceSelector returns the effective selector for polled k8s sources and
// the pod-discovery controller. Include comes from namespaces.include, falling
// back to the legacy k8s.namespaces list for back-compat; Exclude is the base
// namespaces.exclude. The base label selectors (namespaces.include_labels /
// exclude_labels) and the "*" sentinel are carried through for Expand to
// resolve at startup.
func (c *CollectorConfig) K8sNamespaceSelector() NamespaceSelector {
	include := c.Namespaces.Include
	if len(include) == 0 {
		include = c.K8s.Namespaces
	}
	return NamespaceSelector{
		Include:       include,
		Exclude:       c.effectiveExclude(),
		IncludeLabels: c.Namespaces.IncludeLabels,
		ExcludeLabels: c.Namespaces.ExcludeLabels,
	}
}

// LogNamespaceSelector returns the effective selector for the polled log worker.
// Include is the same base include as K8sNamespaceSelector (namespaces.include,
// falling back to the legacy k8s.namespaces list) so logs watch the same
// namespaces as metrics/status by default. Exclude is the base exclude plus any
// log-specific excludes (namespaces.logs.exclude) plus the self namespace
// (namespaces.self_namespace).
//
// This is the structural loop-breaker for #145: even when accretion's own namespace
// is in the base include (so metrics/status observe it), the log worker can
// never ingest it, so the log worker's own output cannot be re-ingested. The
// self-exclusion is applied HERE only — K8sNamespaceSelector deliberately does
// not apply it, so metrics/status still see the self namespace.
//
// The base label selectors (namespaces.include_labels / exclude_labels) and the
// "*" sentinel are carried through for Expand to resolve at startup.
func (c *CollectorConfig) LogNamespaceSelector() NamespaceSelector {
	include := c.Namespaces.Include
	if len(include) == 0 {
		include = c.K8s.Namespaces
	}
	return NamespaceSelector{
		Include:       include,
		Exclude:       c.effectiveExclude(c.LogOnlyExcludes()...),
		IncludeLabels: c.Namespaces.IncludeLabels,
		ExcludeLabels: c.Namespaces.ExcludeLabels,
	}
}

// LogOnlyExcludes returns the name-based excludes the LOG selector adds ON TOP
// of the base namespaces.exclude: the log-specific namespaces.logs.exclude
// entries plus the self namespace (namespaces.self_namespace) when non-empty.
// These are exactly the excludes that make LogNamespaceSelector's set a subset
// of K8sNamespaceSelector's — self-exclusion is applied to logs only (#145).
//
// It exists so the collector can DERIVE the log scope from the k8s scope by
// subtracting these names at read time (nsscope.Derived), instead of resolving
// the identical include/label query a second time. LogNamespaceSelector builds
// its Exclude from this same helper so the two can never drift.
func (c *CollectorConfig) LogOnlyExcludes() []string {
	extra := make([]string, 0, len(c.Namespaces.Logs.Exclude)+1)
	extra = append(extra, c.Namespaces.Logs.Exclude...)
	if c.Namespaces.SelfNamespace != "" {
		extra = append(extra, c.Namespaces.SelfNamespace)
	}
	return extra
}

// TraceNamespaceSelector returns the effective selector for the push-based trace
// worker. Include comes from namespaces.traces.include, falling back to the
// legacy trace_worker.namespaces list (which has no built-in default, so an
// unset Include admits every namespace) for back-compat; Exclude is the base
// exclude plus any
// trace-specific excludes, always including the self namespace — which is what
// keeps the anti-feedback-loop protection intact whatever Include says.
//
// Label selectors follow the same shape as the names: IncludeLabels comes from
// namespaces.traces.include_labels (independent of the base, like include);
// ExcludeLabels is the base exclude_labels plus namespaces.traces.exclude_labels
// (combined, like exclude). Expand resolves them at startup.
//
// Self-exclusion (#146/#145): the self namespace is added to Exclude, same as
// LogNamespaceSelector. With the historical single-namespace allowlist this was
// redundant (accretion's own namespace was never in the allowlist); now that the
// shipped trace scope IS "*", the self-exclude is the load-bearing control that
// keeps accretion from ingesting its OWN traces and re-feeding the amplification
// loop. Exclude wins, so it holds even against "*".
func (c *CollectorConfig) TraceNamespaceSelector() NamespaceSelector {
	include := c.Namespaces.Traces.Include
	if len(include) == 0 {
		include = c.TraceWorker.Namespaces
	}
	extra := make([]string, 0, len(c.Namespaces.Traces.Exclude)+1)
	extra = append(extra, c.Namespaces.Traces.Exclude...)
	if c.Namespaces.SelfNamespace != "" {
		extra = append(extra, c.Namespaces.SelfNamespace)
	}
	return NamespaceSelector{
		Include:       include,
		Exclude:       c.effectiveExclude(extra...),
		IncludeLabels: c.Namespaces.Traces.IncludeLabels,
		ExcludeLabels: c.effectiveExcludeLabels(c.Namespaces.Traces.ExcludeLabels...),
	}
}
