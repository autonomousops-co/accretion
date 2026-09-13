package collector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/testutil"
)

// fakeCollector is a Supervised whose Start behavior is driven by onStart, so a
// test can make one collector panic or fail while the others keep running.
type fakeCollector struct {
	onStart func(ctx context.Context, run int) error
	name    string
	starts  atomic.Int32
	stops   atomic.Int32
	stopErr error
}

func (f *fakeCollector) Type() string { return f.name }

func (f *fakeCollector) Start(ctx context.Context) error {
	run := int(f.starts.Add(1))
	if f.onStart != nil {
		return f.onStart(ctx, run)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeCollector) Stop(context.Context) error {
	f.stops.Add(1)
	return f.stopErr
}

// blockUntilDone is the well-behaved collector: it runs until shutdown and
// returns the context error, which the supervisor must not count as a failure.
func blockUntilDone(ctx context.Context, _ int) error {
	<-ctx.Done()
	return ctx.Err()
}

// fastBackoff keeps restart delays short enough for a test while preserving the
// exponential shape.
func fastBackoff() SupervisorConfig {
	return SupervisorConfig{
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
		HealthyRun:     time.Hour,
		UnhealthyAfter: 3,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSupervisorRestartsPanickingCollector is the failure-isolation criterion of
// #91 and #92: consolidating collectors into one process trades process
// isolation for density, so a panicking handler must restart its own collector
// and leave the others collecting.
func TestSupervisorRestartsPanickingCollector(t *testing.T) {
	testutil.SetupTestLogger(t)

	panicky := &fakeCollector{
		name: "panicky",
		onStart: func(ctx context.Context, run int) error {
			if run <= 2 {
				panic("handler exploded")
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	healthy := &fakeCollector{name: "healthy", onStart: blockUntilDone}

	s, err := NewSupervisor(fastBackoff(),
		Unit{Name: "panicky", Worker: panicky},
		Unit{Name: "healthy", Worker: healthy})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// The panicking collector is restarted past its two panics...
	waitFor(t, "panicky collector to be restarted twice", func() bool {
		return panicky.starts.Load() >= 3
	})

	// ...and the healthy one was never restarted along with it: a panic in one
	// handler stayed inside that handler.
	if got := healthy.starts.Load(); got != 1 {
		t.Fatalf("healthy collector started %d times; a sibling's panic must not restart it", got)
	}

	waitFor(t, "supervisor to report healthy again", s.Healthy)

	cancel()
	s.Wait()
}

// TestSupervisorReportsUnhealthyAfterRepeatedFailures covers the other half of
// the criterion: a collector that cannot be restarted successfully must not be
// left dead while the pod goes on reporting Ready.
func TestSupervisorReportsUnhealthyAfterRepeatedFailures(t *testing.T) {
	testutil.SetupTestLogger(t)

	doomed := &fakeCollector{
		name:    "doomed",
		onStart: func(context.Context, int) error { return errors.New("cannot start") },
	}
	healthy := &fakeCollector{name: "healthy", onStart: blockUntilDone}

	s, err := NewSupervisor(fastBackoff(),
		Unit{Name: "doomed", Worker: doomed},
		Unit{Name: "healthy", Worker: healthy})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor(t, "supervisor to report unhealthy", func() bool { return !s.Healthy() })

	status := s.Status()["doomed"]
	if status.Healthy {
		t.Fatal("doomed collector reports healthy after repeated failures")
	}
	if status.LastError != "cannot start" {
		t.Fatalf("last error = %q, want the collector's own error", status.LastError)
	}
	if status.ConsecutiveFailures < 3 {
		t.Fatalf("consecutive failures = %d, want at least 3", status.ConsecutiveFailures)
	}

	// The healthy collector kept running throughout.
	if got := healthy.starts.Load(); got != 1 {
		t.Fatalf("healthy collector started %d times, want 1", got)
	}

	cancel()
	s.Wait()
}

// TestSupervisorTreatsCancellationAsCleanShutdown guards the case that would
// otherwise churn on every rollout: workers return ctx.Err() from their run
// loops, and the supervisor must read that as shutdown rather than as a crash
// to restart.
func TestSupervisorTreatsCancellationAsCleanShutdown(t *testing.T) {
	testutil.SetupTestLogger(t)

	c := &fakeCollector{name: "clean", onStart: blockUntilDone}
	s, err := NewSupervisor(fastBackoff(), Unit{Name: "clean", Worker: c})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	waitFor(t, "collector to start", func() bool { return c.starts.Load() == 1 })

	cancel()
	s.Wait()

	if got := c.starts.Load(); got != 1 {
		t.Fatalf("collector started %d times; cancellation must not trigger a restart", got)
	}
	if !s.Healthy() {
		t.Fatal("supervisor reports unhealthy after a clean shutdown")
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := c.stops.Load(); got != 1 {
		t.Fatalf("Stop called %d times, want 1", got)
	}
}

func TestNewSupervisorRejectsBadUnits(t *testing.T) {
	testutil.SetupTestLogger(t)

	ok := &fakeCollector{name: "ok", onStart: blockUntilDone}

	cases := []struct {
		name  string
		units []Unit
	}{
		{"no units", nil},
		{"empty name", []Unit{{Name: "", Worker: ok}}},
		{"nil worker", []Unit{{Name: "a", Worker: nil}}},
		{"duplicate name", []Unit{{Name: "a", Worker: ok}, {Name: "a", Worker: ok}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSupervisor(fastBackoff(), tc.units...); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
