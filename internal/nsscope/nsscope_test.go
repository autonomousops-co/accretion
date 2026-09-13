package nsscope

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// TestMain installs a no-op global logger once for the whole package. The
// refresh goroutine logs, so the global must be non-nil before any test starts
// and must not be swapped while a goroutine is running (which -race would flag).
func TestMain(m *testing.M) {
	logger.Logger = zap.NewNop()
	os.Exit(m.Run())
}

// testInterval is short enough to keep refresh tests fast; testDeadline is
// generous so they don't flake on a loaded machine.
const (
	testInterval = 10 * time.Millisecond
	testDeadline = 2 * time.Second
)

// fakeLister returns a scripted sequence of results, one per ListNamespaces
// call, repeating the last entry once exhausted. It records its call count so
// tests can assert on the ZERO-call fast path and on goroutine shutdown.
type fakeLister struct {
	mu      sync.Mutex
	results []listResult
	calls   int
}

type listResult struct {
	namespaces []string
	err        error
}

func (f *fakeLister) ListNamespaces(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	i := min(f.calls, len(f.results)-1)
	f.calls++

	r := f.results[i]
	return slices.Clone(r.namespaces), r.err
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// eventually polls read until it reports true, failing the test at testDeadline.
func eventually(t *testing.T, what string, read func() bool) {
	t.Helper()

	deadline := time.Now().Add(testDeadline)
	for time.Now().Before(deadline) {
		if read() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", testDeadline, what)
}

// labelSelector is a selector that requires expansion, used by the refresh tests.
func labelSelector() config.NamespaceSelector {
	return config.NamespaceSelector{IncludeLabels: []string{"accretion=watched"}}
}

func TestNewNameOnlySelectorMakesNoAPICalls(t *testing.T) {
	sel := config.NamespaceSelector{
		Include: []string{"alpha", "beta", "gamma"},
		Exclude: []string{"beta"},
	}
	lister := &fakeLister{results: []listResult{{namespaces: []string{"should-not-be-used"}}}}

	scope, err := New(context.Background(), sel, lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Give any (erroneously started) refresh loop several intervals to call out.
	time.Sleep(20 * testInterval)

	if got := lister.callCount(); got != 0 {
		t.Errorf("lister calls = %d, want 0 — the name-only fast path must never list namespaces", got)
	}
	if got, want := scope.Namespaces(), sel.Resolve(); !slices.Equal(got, want) {
		t.Errorf("Namespaces() = %v, want %v", got, want)
	}
	for _, ns := range []string{"alpha", "beta", "gamma", "delta", ""} {
		if got, want := scope.Allows(ns), sel.Allows(ns); got != want {
			t.Errorf("Allows(%q) = %v, want %v (must match the selector)", ns, got, want)
		}
	}
}

// A nil lister must be safe on the fast path: the trace worker passes one rather
// than building a Kubernetes client it doesn't need.
func TestNewNameOnlySelectorAcceptsNilLister(t *testing.T) {
	sel := config.NamespaceSelector{Include: []string{"dkjr"}}

	scope, err := New(context.Background(), sel, nil, testInterval)
	if err != nil {
		t.Fatalf("New with nil lister: %v", err)
	}
	if got, want := scope.Namespaces(), []string{"dkjr"}; !slices.Equal(got, want) {
		t.Errorf("Namespaces() = %v, want %v", got, want)
	}
}

func TestNewInitialResolveErrorFailsFast(t *testing.T) {
	wantErr := errors.New("api server unreachable")
	lister := &fakeLister{results: []listResult{{err: wantErr}}}

	scope, err := New(context.Background(), labelSelector(), lister, testInterval)
	if err == nil {
		t.Fatal("New = nil error, want the initial resolve error — a misconfigured scope must fail at startup")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("New error = %v, want it to wrap %v", err, wantErr)
	}
	if scope != nil {
		t.Errorf("New scope = %v, want nil on error", scope)
	}
}

func TestRefreshPicksUpNewNamespace(t *testing.T) {
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"a"}},
		{namespaces: []string{"a", "b"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scope, err := New(ctx, labelSelector(), lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got, want := scope.Namespaces(), []string{"a"}; !slices.Equal(got, want) {
		t.Fatalf("Namespaces() before refresh = %v, want %v", got, want)
	}

	eventually(t, `"b" to be scoped in`, func() bool {
		return slices.Contains(scope.Namespaces(), "b")
	})
	if !scope.Allows("b") {
		t.Error(`Allows("b") = false, want true after "b" entered scope`)
	}
}

func TestRefreshDropsRemovedNamespace(t *testing.T) {
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"a", "b"}},
		{namespaces: []string{"a"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scope, err := New(ctx, labelSelector(), lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	eventually(t, `"b" to be dropped`, func() bool {
		return !slices.Contains(scope.Namespaces(), "b")
	})
	if scope.Allows("b") {
		t.Error(`Allows("b") = true, want false after "b" left scope`)
	}
	if got, want := scope.Namespaces(), []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("Namespaces() = %v, want %v", got, want)
	}
}

func TestRefreshErrorKeepsLastGoodSnapshot(t *testing.T) {
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"a", "b"}},
		{err: errors.New("transient API blip")},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scope, err := New(ctx, labelSelector(), lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Let several failing refreshes land.
	eventually(t, "at least two failed refreshes", func() bool {
		return lister.callCount() >= 3
	})

	// The scope must neither empty out (silently stopping ingestion) nor widen.
	if got, want := scope.Namespaces(), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("Namespaces() after failed refreshes = %v, want the last good %v", got, want)
	}
	if !scope.Allows("a") || !scope.Allows("b") {
		t.Error("Allows dropped a previously scoped namespace after a failed refresh")
	}
	if scope.Allows("c") {
		t.Error(`Allows("c") = true, want false — a failed refresh must not widen scope`)
	}
}

func TestRefreshStopsOnContextCancel(t *testing.T) {
	lister := &fakeLister{results: []listResult{{namespaces: []string{"a"}}}}

	ctx, cancel := context.WithCancel(context.Background())

	if _, err := New(ctx, labelSelector(), lister, testInterval); err != nil {
		t.Fatalf("New: %v", err)
	}

	// Let the loop run, then stop it.
	eventually(t, "the refresh loop to run", func() bool {
		return lister.callCount() >= 2
	})
	cancel()

	// Allow an in-flight refresh to finish, then confirm the loop is gone.
	time.Sleep(20 * testInterval)
	settled := lister.callCount()
	time.Sleep(20 * testInterval)

	if got := lister.callCount(); got != settled {
		t.Errorf("lister calls went %d -> %d after cancel; refresh goroutine did not exit", settled, got)
	}
}

// A zero interval means "resolve once at startup and never refresh" — the
// pre-#152 behavior, still reachable via namespaces.refresh_interval: "0".
func TestZeroIntervalDisablesRefresh(t *testing.T) {
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"a"}},
		{namespaces: []string{"a", "b"}},
	}}

	scope, err := New(context.Background(), labelSelector(), lister, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	time.Sleep(20 * testInterval)

	if got := lister.callCount(); got != 1 {
		t.Errorf("lister calls = %d, want exactly 1 (the startup snapshot)", got)
	}
	if got, want := scope.Namespaces(), []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("Namespaces() = %v, want the frozen %v", got, want)
	}
}

// Exclusion is the #145/#146 loop-breaker: it must survive every refresh, even
// when "*" pulls accretion's own namespace back into the include set each time.
func TestExcludeWinsAfterRefresh(t *testing.T) {
	sel := config.NamespaceSelector{
		Include: []string{config.IncludeAllNamespaces},
		Exclude: []string{"accretion-core"},
	}
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"a", "accretion-core"}},
		{namespaces: []string{"a", "accretion-core", "b"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scope, err := New(ctx, sel, lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	eventually(t, `"b" to be scoped in by "*"`, func() bool {
		return slices.Contains(scope.Namespaces(), "b")
	})

	if slices.Contains(scope.Namespaces(), "accretion-core") {
		t.Error(`Namespaces() contains "accretion-core"; the self-exclusion must survive a refresh`)
	}
	if scope.Allows("accretion-core") {
		t.Error(`Allows("accretion-core") = true; exclude must keep winning over "*" after a refresh`)
	}
}

func TestNamespacesReturnsACopy(t *testing.T) {
	lister := &fakeLister{results: []listResult{{namespaces: []string{"a", "b"}}}}

	scopes := map[string]Scope{
		"static": Static(config.NamespaceSelector{Include: []string{"a", "b"}}),
	}

	// A non-zero interval is required: interval <= 0 returns a staticScope, so
	// passing 0 here would silently test staticScope twice and never exercise
	// refreshingScope's clone-on-read — the aliasing hazard this test exists to
	// catch. time.Hour keeps it deterministic (the initial publish happens
	// before the refresh goroutine's first tick).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refreshing, err := New(ctx, labelSelector(), lister, time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := refreshing.(*refreshingScope); !ok {
		t.Fatalf("New returned %T, want *refreshingScope; this test must exercise the refreshing path", refreshing)
	}
	scopes["refreshing"] = refreshing

	for name, scope := range scopes {
		t.Run(name, func(t *testing.T) {
			got := scope.Namespaces()
			if len(got) == 0 {
				t.Fatal("Namespaces() is empty; test needs a non-empty scope")
			}
			got[0] = "mutated"

			if fresh := scope.Namespaces(); fresh[0] == "mutated" {
				t.Error("mutating the returned slice changed the scope; Namespaces() must return a copy")
			}
		})
	}
}

func TestStatic(t *testing.T) {
	sel := config.NamespaceSelector{
		Include: []string{"a", "b"},
		Exclude: []string{"b"},
	}
	scope := Static(sel)

	if got, want := scope.Namespaces(), []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("Namespaces() = %v, want %v", got, want)
	}
	if !scope.Allows("a") {
		t.Error(`Allows("a") = false, want true`)
	}
	if scope.Allows("b") {
		t.Error(`Allows("b") = true, want false (excluded)`)
	}
}

// countingScope wraps a Scope and counts read calls, so a Derived test can
// assert the derived view adds no lister traffic of its own (it has no lister —
// this counts the reads it delegates to the parent).
type countingScope struct {
	Scope
	reads int
}

func (c *countingScope) Namespaces() []string {
	c.reads++
	return c.Scope.Namespaces()
}

func TestDerivedSubtractsExtraExcludes(t *testing.T) {
	parent := Static(config.NamespaceSelector{Include: []string{"a", "b", "self", "c"}})
	derived := Derived(parent, "self", "b")

	if got, want := derived.Namespaces(), []string{"a", "c"}; !slices.Equal(got, want) {
		t.Errorf("Namespaces() = %v, want %v", got, want)
	}
	if !derived.Contains("a") {
		t.Error(`Contains("a") = false, want true`)
	}
	if derived.Contains("self") {
		t.Error(`Contains("self") = true, want false (extra-excluded)`)
	}
	if !derived.Allows("a") {
		t.Error(`Allows("a") = false, want true`)
	}
	if derived.Allows("b") {
		t.Error(`Allows("b") = true, want false (extra-excluded)`)
	}
	if derived.IsEmpty() {
		t.Error("IsEmpty() = true, want false")
	}
}

// Namespaces() must not mutate the parent's set: the parent must still report
// the extra-excluded namespace on its own reads.
func TestDerivedDoesNotMutateParent(t *testing.T) {
	parent := Static(config.NamespaceSelector{Include: []string{"a", "self", "b"}})
	derived := Derived(parent, "self")

	_ = derived.Namespaces()

	if got, want := parent.Namespaces(), []string{"a", "self", "b"}; !slices.Equal(got, want) {
		t.Errorf("parent Namespaces() = %v, want %v — Derived must not mutate parent's set", got, want)
	}
}

// A parent that is non-empty but fully covered by the extra excludes yields an
// empty derived set — IsEmpty must reflect the filtered set, not the parent's.
func TestDerivedFullyExcludedIsEmpty(t *testing.T) {
	parent := Static(config.NamespaceSelector{Include: []string{"a", "b"}})
	derived := Derived(parent, "a", "b")

	if !derived.IsEmpty() {
		t.Error("IsEmpty() = false, want true when every parent namespace is excluded")
	}
	if got := derived.Namespaces(); len(got) != 0 {
		t.Errorf("Namespaces() = %v, want empty", got)
	}
}

// Blank extra excludes carry no intent, so Derived returns the parent itself.
func TestDerivedEmptyExcludesReturnsParent(t *testing.T) {
	parent := Static(config.NamespaceSelector{Include: []string{"a", "b"}})

	if got := Derived(parent); got != parent {
		t.Errorf("Derived with no excludes = %v, want the parent itself", got)
	}
	if got := Derived(parent, "", ""); got != parent {
		t.Errorf("Derived with only blank excludes = %v, want the parent itself", got)
	}
}

// Derived holds no lister of its own; it must track a refreshingScope parent's
// background changes purely by reading through to it.
func TestDerivedTracksParentRefresh(t *testing.T) {
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"a", "self"}},
		{namespaces: []string{"a", "self", "b"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	parent, err := New(ctx, labelSelector(), lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	derived := Derived(parent, "self")

	// self is filtered out from the very first read.
	if slices.Contains(derived.Namespaces(), "self") {
		t.Error(`derived Namespaces() contains "self"; it must be excluded`)
	}

	// A namespace the parent picks up on refresh must appear in the derived view
	// without the derived scope doing any listing of its own.
	eventually(t, `"b" to appear in the derived view via the parent refresh`, func() bool {
		return slices.Contains(derived.Namespaces(), "b")
	})
	if slices.Contains(derived.Namespaces(), "self") {
		t.Error(`derived Namespaces() still excludes "self" after refresh`)
	}
}

// Derived issues no lister calls of its own: reading it many times must not add
// to the parent's lister traffic, and it must read through to its parent.
func TestDerivedMakesNoListerCalls(t *testing.T) {
	lister := &fakeLister{results: []listResult{{namespaces: []string{"a", "b", "self"}}}}

	parent := Static(config.NamespaceSelector{Include: []string{"a", "b", "self"}})
	counting := &countingScope{Scope: parent}
	derived := Derived(counting, "self")

	callsBefore := lister.callCount()
	for i := 0; i < 5; i++ {
		_ = derived.Namespaces()
		_ = derived.Contains("a")
		_ = derived.Allows("a")
		_ = derived.IsEmpty()
	}

	if got := lister.callCount(); got != callsBefore {
		t.Errorf("lister calls = %d, want unchanged %d — Derived must issue no LISTs", got, callsBefore)
	}
	if counting.reads == 0 {
		t.Error("expected the derived view to read through to its parent")
	}
}

// TestRefreshToEmptyDoesNotWidenScope is the #152 regression guard for a
// *successful* refresh that matches nothing. The fail-soft path covers refresh
// ERRORS, but a refresh that succeeds with zero matches used to publish an empty
// Include, which Allows read as "no allowlist => allow everything" — silently
// turning a label-scoped trace worker into a firehose at runtime, with no error
// and no restart. An allowlist that resolves to nothing must go quiet, not open
// up.
func TestRefreshToEmptyDoesNotWidenScope(t *testing.T) {
	lister := &fakeLister{results: []listResult{
		{namespaces: []string{"team-a"}}, // startup: one match
		{namespaces: nil},                // refresh: label removed => no matches, NO error
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scope, err := New(ctx, labelSelector(), lister, testInterval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !scope.Allows("team-a") || scope.Allows("unrelated-ns") {
		t.Fatalf("startup scope wrong: Allows(team-a)=%v Allows(unrelated-ns)=%v, want true/false",
			scope.Allows("team-a"), scope.Allows("unrelated-ns"))
	}

	eventually(t, "the scope to drop team-a after the empty refresh", func() bool {
		return len(scope.Namespaces()) == 0
	})

	if scope.Allows("unrelated-ns") {
		t.Error("Allows(unrelated-ns) = true after a successful empty refresh; " +
			"an allowlist resolving to nothing must deny everything, not widen to all namespaces")
	}
	if scope.Allows("team-a") {
		t.Error("Allows(team-a) = true after it stopped matching the label")
	}
}
