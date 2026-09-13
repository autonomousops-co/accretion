package config

import (
	"reflect"
	"testing"
	"time"
)

func TestNamespaceSelector_Resolve(t *testing.T) {
	tests := []struct {
		name     string
		selector NamespaceSelector
		want     []string
	}{
		{
			name:     "include only",
			selector: NamespaceSelector{Include: []string{"a", "b"}},
			want:     []string{"a", "b"},
		},
		{
			name:     "exclude wins over include",
			selector: NamespaceSelector{Include: []string{"a", "b", "c"}, Exclude: []string{"b"}},
			want:     []string{"a", "c"},
		},
		{
			name:     "empty include resolves to nothing",
			selector: NamespaceSelector{Exclude: []string{"a"}},
			want:     nil,
		},
		{
			name:     "de-dupes and drops blanks preserving order",
			selector: NamespaceSelector{Include: []string{"a", "", "a", "b"}},
			want:     []string{"a", "b"},
		},
		{
			name:     "exclude everything yields empty slice",
			selector: NamespaceSelector{Include: []string{"a"}, Exclude: []string{"a"}},
			want:     []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.selector.Resolve()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNamespaceSelector_Allows(t *testing.T) {
	tests := []struct {
		name     string
		ns       string
		selector NamespaceSelector
		want     bool
	}{
		{
			name:     "empty include allows all",
			selector: NamespaceSelector{},
			ns:       "anything",
			want:     true,
		},
		{
			name:     "empty include allows empty namespace",
			selector: NamespaceSelector{},
			ns:       "",
			want:     true,
		},
		{
			name:     "include allows member",
			selector: NamespaceSelector{Include: []string{"dkjr"}},
			ns:       "dkjr",
			want:     true,
		},
		{
			name:     "include rejects non-member",
			selector: NamespaceSelector{Include: []string{"dkjr"}},
			ns:       "kube-system",
			want:     false,
		},
		{
			name:     "include rejects empty namespace when allowlist configured",
			selector: NamespaceSelector{Include: []string{"dkjr"}},
			ns:       "",
			want:     false,
		},
		{
			name:     "exclude wins over allow-all",
			selector: NamespaceSelector{Exclude: []string{"kube-system"}},
			ns:       "kube-system",
			want:     false,
		},
		{
			name:     "exclude wins over include",
			selector: NamespaceSelector{Include: []string{"kube-system"}, Exclude: []string{"kube-system"}},
			ns:       "kube-system",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.selector.Allows(tt.ns); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.ns, got, tt.want)
			}
		})
	}
}

// TestK8sNamespaceSelector_BackCompat verifies the legacy k8s.namespaces list is
// used when the unified namespaces.include is empty, and superseded when it is set.
func TestK8sNamespaceSelector_BackCompat(t *testing.T) {
	// Legacy-only config: k8s.namespaces set, unified include empty.
	legacy := &CollectorConfig{
		K8s: K8sConfig{Namespaces: []string{"production", "staging"}},
	}
	if got := legacy.K8sNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"production", "staging"}) {
		t.Errorf("legacy fallback Resolve() = %v, want [production staging]", got)
	}

	// Unified config supersedes the legacy list.
	unified := &CollectorConfig{
		K8s: K8sConfig{Namespaces: []string{"production"}},
		Namespaces: NamespaceScopeConfig{
			NamespaceSelector: NamespaceSelector{Include: []string{"team-a", "team-b"}},
		},
	}
	if got := unified.K8sNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"team-a", "team-b"}) {
		t.Errorf("unified Resolve() = %v, want [team-a team-b]", got)
	}
}

// TestK8sNamespaceSelector_Exclude verifies namespaces.exclude removes entries
// from the resolved polled-source list.
func TestK8sNamespaceSelector_Exclude(t *testing.T) {
	cfg := &CollectorConfig{
		Namespaces: NamespaceScopeConfig{
			NamespaceSelector: NamespaceSelector{
				Include: []string{"default", "kube-system"},
				Exclude: []string{"kube-system"},
			},
		},
	}
	if got := cfg.K8sNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("exclude Resolve() = %v, want [default]", got)
	}
}

// TestLogNamespaceSelector_SelfExclusion is the core #145 guardrail: accretion's own
// namespace must be visible to metrics/status (K8sNamespaceSelector) but never
// to the log worker (LogNamespaceSelector), even when it is in the base include.
// This is what prevents the log amplification loop.
func TestLogNamespaceSelector_SelfExclusion(t *testing.T) {
	cfg := &CollectorConfig{
		Namespaces: NamespaceScopeConfig{
			NamespaceSelector: NamespaceSelector{
				Include: []string{"default", "accretion-core", "dkjr"},
			},
			SelfNamespace: "accretion-core",
		},
	}

	// Metrics/status DO see the self namespace.
	k8s := cfg.K8sNamespaceSelector()
	if got := k8s.Resolve(); !reflect.DeepEqual(got, []string{"default", "accretion-core", "dkjr"}) {
		t.Errorf("K8sNamespaceSelector.Resolve() = %v, want self namespace included", got)
	}
	if !k8s.Allows("accretion-core") {
		t.Error("expected metrics selector to allow accretion-core")
	}

	// Logs never see the self namespace.
	logs := cfg.LogNamespaceSelector()
	if got := logs.Resolve(); !reflect.DeepEqual(got, []string{"default", "dkjr"}) {
		t.Errorf("LogNamespaceSelector.Resolve() = %v, want self namespace excluded", got)
	}
	if logs.Allows("accretion-core") {
		t.Error("expected log selector to EXCLUDE accretion-core (loop prevention)")
	}
	if !logs.Allows("default") {
		t.Error("expected log selector to still allow default")
	}
}

// TestLogNamespaceSelector_OptOut verifies an empty self_namespace disables the
// self-exclusion, so logs then see the same namespaces as metrics.
func TestLogNamespaceSelector_OptOut(t *testing.T) {
	cfg := &CollectorConfig{
		Namespaces: NamespaceScopeConfig{
			NamespaceSelector: NamespaceSelector{Include: []string{"default", "accretion-core"}},
			SelfNamespace:     "",
		},
	}
	if got := cfg.LogNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"default", "accretion-core"}) {
		t.Errorf("opt-out LogNamespaceSelector.Resolve() = %v, want accretion-core included", got)
	}
}

// TestLogNamespaceSelector_BackCompatAndExtraExclude verifies logs fall back to
// the legacy k8s.namespaces list for their include, and that namespaces.logs.exclude
// stacks with the self-exclusion.
func TestLogNamespaceSelector_BackCompatAndExtraExclude(t *testing.T) {
	cfg := &CollectorConfig{
		K8s: K8sConfig{Namespaces: []string{"default", "accretion-core", "noisy"}},
		Namespaces: NamespaceScopeConfig{
			Logs:          NamespaceSelector{Exclude: []string{"noisy"}},
			SelfNamespace: "accretion-core",
		},
	}
	if got := cfg.LogNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("LogNamespaceSelector.Resolve() = %v, want [default] (self + logs.exclude removed)", got)
	}
	// Metrics still see accretion-core and noisy (only base exclude applies).
	if got := cfg.K8sNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"default", "accretion-core", "noisy"}) {
		t.Errorf("K8sNamespaceSelector.Resolve() = %v, want all three (no log excludes)", got)
	}
}

// TestLogOnlyExcludes verifies the helper returns exactly the log-specific
// excludes (logs.exclude + non-empty self namespace) the collector subtracts
// when it derives the log scope from the k8s scope (#155).
func TestLogOnlyExcludes(t *testing.T) {
	cfg := &CollectorConfig{
		Namespaces: NamespaceScopeConfig{
			NamespaceSelector: NamespaceSelector{Exclude: []string{"base-ex"}}, // base exclude must NOT appear
			Logs:              NamespaceSelector{Exclude: []string{"noisy", "chatty"}},
			SelfNamespace:     "accretion-core",
		},
	}
	if got, want := cfg.LogOnlyExcludes(), []string{"noisy", "chatty", "accretion-core"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LogOnlyExcludes() = %v, want %v", got, want)
	}

	// Empty self namespace opts out of self-exclusion (#145 opt-out).
	cfg.Namespaces.SelfNamespace = ""
	if got, want := cfg.LogOnlyExcludes(), []string{"noisy", "chatty"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LogOnlyExcludes() with empty self = %v, want %v", got, want)
	}
}

// TestLogOnlyExcludesMatchesLogSelectorExclude is the drift guard for #155: the
// derived log scope reconstructs LogNamespaceSelector's Exclude as
// K8sNamespaceSelector().Exclude ∪ LogOnlyExcludes(). That union must equal
// LogNamespaceSelector().Exclude as a set, or the derived collector scope would
// diverge from the standalone log-worker scope.
func TestLogOnlyExcludesMatchesLogSelectorExclude(t *testing.T) {
	cfg := &CollectorConfig{
		K8s: K8sConfig{Namespaces: []string{"a", "b", "c", "accretion-core", "noisy"}},
		Namespaces: NamespaceScopeConfig{
			NamespaceSelector: NamespaceSelector{Exclude: []string{"base-ex"}},
			Logs:              NamespaceSelector{Exclude: []string{"noisy"}},
			SelfNamespace:     "accretion-core",
		},
	}

	k8sSel := cfg.K8sNamespaceSelector()
	logSel := cfg.LogNamespaceSelector()

	union := append(append([]string{}, k8sSel.Exclude...), cfg.LogOnlyExcludes()...)
	if !equalStringSets(union, logSel.Exclude) {
		t.Errorf("K8sNamespaceSelector().Exclude ∪ LogOnlyExcludes() = %v, want set-equal to LogNamespaceSelector().Exclude = %v",
			union, logSel.Exclude)
	}

	// The collector derives logScope from k8sScope (nsscope.Derived) and only
	// subtracts LogOnlyExcludes. That is equivalent to the standalone log scope
	// ONLY IF the two selectors share every include dimension and differ solely
	// in Exclude. Assert that here so a future change — e.g. wiring the currently
	// unused Namespaces.Logs.Include into LogNamespaceSelector — fails loudly
	// instead of silently diverging the derived log scope from the real one.
	if !equalStringSets(k8sSel.Include, logSel.Include) {
		t.Errorf("Include differs: k8s=%v log=%v — derived logScope would diverge", k8sSel.Include, logSel.Include)
	}
	if !equalStringSets(k8sSel.IncludeLabels, logSel.IncludeLabels) {
		t.Errorf("IncludeLabels differ: k8s=%v log=%v — derived logScope would diverge", k8sSel.IncludeLabels, logSel.IncludeLabels)
	}
	if !equalStringSets(k8sSel.ExcludeLabels, logSel.ExcludeLabels) {
		t.Errorf("ExcludeLabels differ: k8s=%v log=%v — derived logScope would diverge", k8sSel.ExcludeLabels, logSel.ExcludeLabels)
	}
}

// equalStringSets reports whether a and b contain the same elements, ignoring
// order and duplicates.
func equalStringSets(a, b []string) bool {
	set := func(xs []string) map[string]struct{} {
		m := make(map[string]struct{}, len(xs))
		for _, x := range xs {
			m[x] = struct{}{}
		}
		return m
	}
	return reflect.DeepEqual(set(a), set(b))
}

// TestTraceNamespaceSelector verifies the trace worker defaults to the legacy
// dkjr-only allowlist and honors the unified override.
func TestTraceNamespaceSelector(t *testing.T) {
	// Legacy default: trace_worker.namespaces=[dkjr].
	cfg := &CollectorConfig{
		TraceWorker: TraceWorkerConfig{Namespaces: []string{"dkjr"}},
	}
	sel := cfg.TraceNamespaceSelector()
	if !sel.Allows("dkjr") {
		t.Error("expected dkjr to be allowed")
	}
	if sel.Allows("default") {
		t.Error("expected default to be rejected (not in dkjr allowlist)")
	}

	// Unified override supersedes the legacy list.
	cfg.Namespaces.Traces = NamespaceSelector{Include: []string{"team-a"}}
	sel = cfg.TraceNamespaceSelector()
	if !sel.Allows("team-a") {
		t.Error("expected team-a to be allowed via unified override")
	}
	if sel.Allows("dkjr") {
		t.Error("expected legacy dkjr to be superseded by unified override")
	}
}

func TestNamespaceRefreshInterval(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default-style value", value: "5m", want: 5 * time.Minute},
		{name: "seconds", value: "30s", want: 30 * time.Second},
		// Empty and "0" both mean "don't refresh": the startup snapshot stands.
		{name: "empty disables refresh", value: "", want: 0},
		{name: "explicit zero disables refresh", value: "0", want: 0},
		// A typo must surface at startup rather than silently freezing the scope.
		{name: "malformed", value: "5 minutes", wantErr: true},
		{name: "unitless", value: "5", wantErr: true},
		// A positive interval below the floor is rejected: "5ms" parses cleanly
		// but would LIST namespaces ~200x/sec from every worker. It is far more
		// likely a unit slip for "5m" than a real intent, so it must not be
		// silently honored. Use 0 to disable refreshing.
		{name: "at the floor is allowed", value: "10s", want: MinNamespaceRefreshInterval},
		{name: "below the floor", value: "5ms", wantErr: true},
		{name: "sub-second below the floor", value: "100ms", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &CollectorConfig{}
			cfg.Namespaces.RefreshInterval = tt.value

			got, err := cfg.NamespaceRefreshInterval()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NamespaceRefreshInterval(%q) = %v, nil; want an error", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NamespaceRefreshInterval(%q): unexpected error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("NamespaceRefreshInterval(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
