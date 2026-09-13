package config

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fakeNamespaceLister is a test double for NamespaceLister. It records every
// call and answers from a per-selector map (empty label selector => "all").
type fakeNamespaceLister struct {
	// byLabel maps a label-selector string to the namespaces it matches. The
	// empty string "" is the "list all namespaces" query used for the "*" sentinel.
	byLabel map[string][]string
	// err, when non-nil, is returned for every call (to test error propagation).
	err error
	// calls records the label selectors passed to ListNamespaces, in order.
	calls []string
}

func (f *fakeNamespaceLister) ListNamespaces(_ context.Context, labelSelector string) ([]string, error) {
	f.calls = append(f.calls, labelSelector)
	if f.err != nil {
		return nil, f.err
	}
	return f.byLabel[labelSelector], nil
}

func TestNamespaceSelector_Expand_FastPath(t *testing.T) {
	sel := NamespaceSelector{Include: []string{"a", "b"}, Exclude: []string{"c"}}
	lister := &fakeNamespaceLister{}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if !reflect.DeepEqual(got, sel) {
		t.Errorf("Expand() = %+v, want unchanged %+v", got, sel)
	}
	if len(lister.calls) != 0 {
		t.Errorf("fast path made %d lister calls, want 0: %v", len(lister.calls), lister.calls)
	}
}

func TestNamespaceSelector_Expand_AllNamespacesSentinel(t *testing.T) {
	sel := NamespaceSelector{
		Include: []string{IncludeAllNamespaces},
		Exclude: []string{"kube-system"},
	}
	lister := &fakeNamespaceLister{
		byLabel: map[string][]string{"": {"default", "kube-system", "dkjr"}},
	}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if !reflect.DeepEqual(got.Include, []string{"default", "kube-system", "dkjr"}) {
		t.Errorf("Include = %v, want all namespaces", got.Include)
	}
	// Exclude subtracts via Resolve.
	if resolved := got.Resolve(); !reflect.DeepEqual(resolved, []string{"default", "dkjr"}) {
		t.Errorf("Resolve() = %v, want [default dkjr]", resolved)
	}
	if len(got.IncludeLabels) != 0 || len(got.ExcludeLabels) != 0 {
		t.Errorf("expanded selector must be purely name-based, got labels %v / %v", got.IncludeLabels, got.ExcludeLabels)
	}
}

func TestNamespaceSelector_Expand_IncludeLabelsUnion(t *testing.T) {
	sel := NamespaceSelector{
		IncludeLabels: []string{"env=prod", "team=payments"},
	}
	lister := &fakeNamespaceLister{
		byLabel: map[string][]string{
			"env=prod":      {"prod-a", "shared"},
			"team=payments": {"shared", "pay-svc"},
		},
	}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	// Union of both label queries, deduped, order-preserving.
	if !reflect.DeepEqual(got.Include, []string{"prod-a", "shared", "pay-svc"}) {
		t.Errorf("Include = %v, want [prod-a shared pay-svc]", got.Include)
	}
}

func TestNamespaceSelector_Expand_ExcludeLabelsWin(t *testing.T) {
	// "shared" is both label-included and label-excluded => excluded.
	sel := NamespaceSelector{
		IncludeLabels: []string{"env=prod"},
		ExcludeLabels: []string{"tier=infra"},
	}
	lister := &fakeNamespaceLister{
		byLabel: map[string][]string{
			"env=prod":   {"prod-a", "shared"},
			"tier=infra": {"shared"},
		},
	}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if resolved := got.Resolve(); !reflect.DeepEqual(resolved, []string{"prod-a"}) {
		t.Errorf("Resolve() = %v, want [prod-a] (exclude label wins)", resolved)
	}
	if got.Allows("shared") {
		t.Error("Allows(shared) = true, want false (exclude label wins)")
	}
	if !got.Allows("prod-a") {
		t.Error("Allows(prod-a) = false, want true")
	}
}

func TestNamespaceSelector_Expand_Mixed(t *testing.T) {
	sel := NamespaceSelector{
		Include:       []string{"named-a", IncludeAllNamespaces},
		Exclude:       []string{"named-a"},
		IncludeLabels: []string{"env=prod"},
		ExcludeLabels: []string{"tier=infra"},
	}
	lister := &fakeNamespaceLister{
		byLabel: map[string][]string{
			"":           {"kube-system", "prod-b"},
			"env=prod":   {"prod-b", "prod-c"},
			"tier=infra": {"kube-system"},
		},
	}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	// Include union order: name entries (named-a) + all ("" => kube-system, prod-b)
	// + labels (prod-b dup, prod-c). Exclude: named-a + tier=infra (kube-system).
	// Resolve = Include minus Exclude, order-preserving.
	want := []string{"prod-b", "prod-c"}
	if resolved := got.Resolve(); !reflect.DeepEqual(resolved, want) {
		t.Errorf("Resolve() = %v, want %v", resolved, want)
	}
}

func TestNamespaceSelector_Expand_ListerErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	sel := NamespaceSelector{IncludeLabels: []string{"env=prod"}}
	lister := &fakeNamespaceLister{err: sentinel}

	_, err := sel.Expand(context.Background(), lister)
	if err == nil {
		t.Fatal("Expand() error = nil, want wrapped lister error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Expand() error = %v, want wrapped %v", err, sentinel)
	}
}

func TestNamespaceSelector_NeedsExpansion(t *testing.T) {
	cases := []struct {
		name string
		sel  NamespaceSelector
		want bool
	}{
		{"name-only", NamespaceSelector{Include: []string{"a"}, Exclude: []string{"b"}}, false},
		{"star", NamespaceSelector{Include: []string{IncludeAllNamespaces}}, true},
		{"include-label", NamespaceSelector{IncludeLabels: []string{"env=prod"}}, true},
		{"exclude-label", NamespaceSelector{ExcludeLabels: []string{"env=dev"}}, true},
		{"blank-labels-only", NamespaceSelector{IncludeLabels: []string{"", "  "}, ExcludeLabels: []string{""}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sel.NeedsExpansion(); got != tc.want {
				t.Errorf("NeedsExpansion() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNamespaceSelector_Expand_BlankIncludeLabelIgnored guards against a blank
// include-label entry silently ballooning the scope to all namespaces (an empty
// label selector matches everything). The blank entry must be skipped, never
// passed to the lister.
func TestNamespaceSelector_Expand_BlankIncludeLabelIgnored(t *testing.T) {
	sel := NamespaceSelector{
		Include:       []string{"keep"},
		IncludeLabels: []string{"env=prod", "  "},
	}
	lister := &fakeNamespaceLister{byLabel: map[string][]string{
		"env=prod": {"prod-a"},
		"":         {"should-never-appear"},
	}}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	for _, c := range lister.calls {
		if strings.TrimSpace(c) == "" {
			t.Errorf("blank label selector was queried (would match all namespaces): calls=%v", lister.calls)
		}
	}
	want := []string{"keep", "prod-a"}
	if resolved := got.Resolve(); !reflect.DeepEqual(resolved, want) {
		t.Errorf("Resolve() = %v, want %v (blank label must not add all namespaces)", resolved, want)
	}
}

// TestNamespaceSelector_Expand_BlankExcludeLabelIgnored guards against a blank
// exclude-label entry silently excluding ALL namespaces (scoping to nothing).
func TestNamespaceSelector_Expand_BlankExcludeLabelIgnored(t *testing.T) {
	sel := NamespaceSelector{
		Include:       []string{"a", "b"},
		ExcludeLabels: []string{"tier=infra", ""},
	}
	lister := &fakeNamespaceLister{byLabel: map[string][]string{
		"tier=infra": {"b"},
		"":           {"a", "b"},
	}}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	for _, c := range lister.calls {
		if strings.TrimSpace(c) == "" {
			t.Errorf("blank exclude-label selector was queried (would exclude everything): calls=%v", lister.calls)
		}
	}
	// Only "b" is excluded (via tier=infra); the blank entry excludes nothing.
	if resolved := got.Resolve(); !reflect.DeepEqual(resolved, []string{"a"}) {
		t.Errorf("Resolve() = %v, want [a] (blank exclude-label must not drop everything)", resolved)
	}
}

// TestTraceNamespaceSelector_SelfExcludedUnderStar verifies the #146/#145
// loop-safety backstop: a trace selector broadened with "*" still excludes
// accretion's own namespace, so accretion never ingests its own traces.
func TestTraceNamespaceSelector_SelfExcludedUnderStar(t *testing.T) {
	cfg := &CollectorConfig{
		Namespaces: NamespaceScopeConfig{
			SelfNamespace: "accretion-core",
			Traces:        NamespaceSelector{Include: []string{IncludeAllNamespaces}},
		},
	}

	sel := cfg.TraceNamespaceSelector()
	if !slices.Contains(sel.Exclude, "accretion-core") {
		t.Fatalf("trace selector must self-exclude accretion-core, Exclude=%v", sel.Exclude)
	}

	lister := &fakeNamespaceLister{byLabel: map[string][]string{"": {"dkjr", "accretion-core"}}}
	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if got.Allows("accretion-core") {
		t.Error("Allows(accretion-core) = true after '*' expansion; self namespace must stay excluded (loop risk)")
	}
	if !got.Allows("dkjr") {
		t.Error("Allows(dkjr) = false; dkjr should be allowed")
	}
}

// TestNamespaceSelector_Expand_ConfiguredAllowlistMatchingNothingDeniesAll is the
// #152 regression guard: an include allowlist that RESOLVES to zero namespaces
// must allow nothing, not collapse to Allows' historical empty-Include =>
// "allow everything". Otherwise a label-scoped push source whose labels stop
// matching (label removed, namespaces deleted) silently turns into a firehose.
func TestNamespaceSelector_Expand_ConfiguredAllowlistMatchingNothingDeniesAll(t *testing.T) {
	sel := NamespaceSelector{IncludeLabels: []string{"traces=on"}}
	lister := &fakeNamespaceLister{byLabel: map[string][]string{"traces=on": nil}}

	got, err := sel.Expand(context.Background(), lister)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if len(got.Include) != 0 {
		t.Fatalf("Include = %v, want empty (label matched nothing)", got.Include)
	}
	if got.Allows("unrelated-ns") {
		t.Error("Allows(unrelated-ns) = true after a configured allowlist matched nothing; " +
			"an allowlist that resolves empty must deny, not allow everything")
	}
	if got.Resolve() != nil {
		t.Errorf("Resolve() = %v, want nil", got.Resolve())
	}
}

// TestNamespaceSelector_NoAllowlistStillAllowsAll pins the historical
// back-compat behavior the #152 fix must NOT disturb: a selector with no
// include allowlist configured at all still allows every namespace.
func TestNamespaceSelector_NoAllowlistStillAllowsAll(t *testing.T) {
	// Never expanded (no labels, no "*") — the plain empty-include config.
	sel := NamespaceSelector{Exclude: []string{"blocked"}}
	if !sel.Allows("anything") {
		t.Error("Allows(anything) = false; an unconfigured include must still allow all")
	}
	if sel.Allows("blocked") {
		t.Error("Allows(blocked) = true; exclude must still win")
	}

	// Same selector routed through Expand via a "*" that matches nothing would
	// be "configured", so verify the fast path leaves intent unset.
	got, err := sel.Expand(context.Background(), &fakeNamespaceLister{})
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if !got.Allows("anything") {
		t.Error("Allows(anything) = false after fast-path Expand; back-compat broken")
	}
}
