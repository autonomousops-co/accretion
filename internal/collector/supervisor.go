package collector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// Supervised is the slice of worker.Worker that the supervisor actually needs.
// It is declared here rather than imported so that internal/worker keeps
// depending on nothing in this package.
type Supervised interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Type() string
}

// Unit is one supervised collector: a name used in logs and health output, and
// the worker to run.
type Unit struct {
	Worker Supervised
	Name   string
}

// SupervisorConfig tunes restart behavior. The zero value is not useful; use
// DefaultSupervisorConfig.
type SupervisorConfig struct {
	// InitialBackoff is the delay before the first restart of a unit.
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential growth of the restart delay.
	MaxBackoff time.Duration
	// HealthyRun is how long a unit must stay up before its consecutive-failure
	// count and backoff are considered recovered. Without this a unit that
	// crashes once a day would eventually inherit a minute of backoff.
	HealthyRun time.Duration
	// UnhealthyAfter is the number of consecutive failures at which the unit is
	// reported unhealthy. This is what makes a wedged collector visible: the
	// process keeps the other units running, but stops claiming to be ready.
	UnhealthyAfter int
}

// DefaultSupervisorConfig returns the restart policy used by the collector
// commands.
func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		InitialBackoff: time.Second,
		MaxBackoff:     time.Minute,
		HealthyRun:     5 * time.Minute,
		UnhealthyAfter: 3,
	}
}

// unitState is the supervisor's live view of one unit.
type unitState struct {
	lastError   error
	lastFailure time.Time
	restarts    int
	consecFails int
	running     bool
	everStarted bool
}

// Supervisor runs a set of collectors as goroutines in one process, restarting
// any that panic or return an unexpected error.
//
// This is the failure-isolation half of consolidating separate worker
// deployments into a single pod (#91, #92). Running collectors as goroutines
// trades process isolation for density, so a panicking handler must not be
// allowed to take the process down or -- worse -- die quietly while the pod
// goes on reporting Ready. Every unit therefore runs behind a recover(), and a
// unit that keeps failing is surfaced through Healthy.
type Supervisor struct {
	cfg    SupervisorConfig
	units  []Unit
	states map[string]*unitState
	mu     sync.Mutex
	wg     sync.WaitGroup
}

// NewSupervisor creates a supervisor over units. Names must be unique.
func NewSupervisor(cfg SupervisorConfig, units ...Unit) (*Supervisor, error) {
	if len(units) == 0 {
		return nil, errors.New("supervisor requires at least one unit; every collector is disabled")
	}

	states := make(map[string]*unitState, len(units))
	for _, u := range units {
		if u.Name == "" {
			return nil, errors.New("supervised unit has an empty name")
		}
		if u.Worker == nil {
			return nil, fmt.Errorf("supervised unit %q has a nil worker", u.Name)
		}
		if _, dup := states[u.Name]; dup {
			return nil, fmt.Errorf("duplicate supervised unit name %q", u.Name)
		}
		states[u.Name] = &unitState{}
	}

	return &Supervisor{cfg: cfg, units: units, states: states}, nil
}

// Names returns the supervised unit names, in the order they were configured.
func (s *Supervisor) Names() []string {
	names := make([]string, 0, len(s.units))
	for _, u := range s.units {
		names = append(names, u.Name)
	}
	return names
}

// Start launches every unit and returns immediately. Use Wait to block until
// ctx is canceled and all units have stopped.
func (s *Supervisor) Start(ctx context.Context) {
	for _, u := range s.units {
		s.wg.Add(1)
		go func(u Unit) {
			defer s.wg.Done()
			s.superviseUnit(ctx, u)
		}(u)
	}
}

// Wait blocks until every supervised unit has returned.
func (s *Supervisor) Wait() { s.wg.Wait() }

// Stop calls Stop on every unit. Units are already being torn down by the
// cancellation of the context passed to Start; this is the explicit,
// deadline-bounded half of that shutdown.
func (s *Supervisor) Stop(ctx context.Context) error {
	var errs []error
	for _, u := range s.units {
		if err := u.Worker.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u.Name, err))
		}
	}
	return errors.Join(errs...)
}

// superviseUnit runs one unit until ctx is canceled, restarting it on panic or
// unexpected error with capped exponential backoff.
func (s *Supervisor) superviseUnit(ctx context.Context, u Unit) {
	backoff := s.cfg.InitialBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		s.markRunning(u.Name)
		err := runGuarded(ctx, u.Worker)
		ran := time.Since(started)

		// A canceled context is the normal shutdown path, not a failure: the
		// workers return ctx.Err() from their run loops.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			s.markStopped(u.Name)
			logger.Logger.Info("Collector stopped", zap.String("collector", u.Name))
			return
		}

		// A unit that stayed up long enough has earned a clean slate, so an
		// unrelated failure much later does not start at the backoff ceiling.
		if ran >= s.cfg.HealthyRun {
			backoff = s.cfg.InitialBackoff
			s.resetFailures(u.Name)
		}

		fails := s.recordFailure(u.Name, err)
		logger.Logger.Error("Collector failed; restarting",
			zap.String("collector", u.Name),
			zap.Duration("ran_for", ran),
			zap.Duration("restart_in", backoff),
			zap.Int("consecutive_failures", fails),
			zap.Error(err))

		select {
		case <-ctx.Done():
			s.markStopped(u.Name)
			return
		case <-time.After(backoff):
		}

		if backoff < s.cfg.MaxBackoff {
			backoff *= 2
			if backoff > s.cfg.MaxBackoff {
				backoff = s.cfg.MaxBackoff
			}
		}
	}
}

// runGuarded calls the worker's Start, converting a panic into an error so one
// bad handler restarts its own collector instead of killing the process and
// every other collector sharing it.
func runGuarded(ctx context.Context, w Supervised) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in collector %s: %v", w.Type(), r)
		}
	}()
	return w.Start(ctx)
}

// Healthy reports whether every unit is running or failing only transiently. It
// goes false once any unit has failed UnhealthyAfter times in a row, which is
// what stops the pod reporting Ready while one of its collectors is wedged.
func (s *Supervisor) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, st := range s.states {
		if st.consecFails >= s.cfg.UnhealthyAfter {
			return false
		}
	}
	return true
}

// Status returns a per-unit summary for health endpoints and logs.
func (s *Supervisor) Status() map[string]UnitStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]UnitStatus, len(s.states))
	for name, st := range s.states {
		u := UnitStatus{
			Running:             st.running,
			Restarts:            st.restarts,
			ConsecutiveFailures: st.consecFails,
			Healthy:             st.consecFails < s.cfg.UnhealthyAfter,
		}
		if st.lastError != nil {
			u.LastError = st.lastError.Error()
			u.LastFailure = st.lastFailure
		}
		out[name] = u
	}
	return out
}

// UnitStatus is the exported view of one supervised collector.
type UnitStatus struct {
	LastFailure         time.Time `json:"last_failure,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	Restarts            int       `json:"restarts"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Running             bool      `json:"running"`
	Healthy             bool      `json:"healthy"`
}

func (s *Supervisor) markRunning(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[name]
	if st.everStarted {
		st.restarts++
	}
	st.everStarted = true
	st.running = true
}

func (s *Supervisor) markStopped(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[name].running = false
}

func (s *Supervisor) resetFailures(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[name].consecFails = 0
}

func (s *Supervisor) recordFailure(name string, err error) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[name]
	st.running = false
	st.consecFails++
	st.lastError = err
	st.lastFailure = time.Now()
	return st.consecFails
}
