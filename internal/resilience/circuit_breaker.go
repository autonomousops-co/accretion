package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// CircuitState represents the current state of the circuit breaker.
type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

// CircuitBreakerConfig defines circuit breaker behavior.
type CircuitBreakerConfig struct {
	OnStateChange func(from, to CircuitState)
	MaxFailures   int
	Timeout       time.Duration
	ResetTimeout  time.Duration
}

// DefaultCircuitBreakerConfig returns a sensible default configuration.
func DefaultCircuitBreakerConfig() *CircuitBreakerConfig {
	return &CircuitBreakerConfig{
		MaxFailures:  5,
		Timeout:      30 * time.Second,
		ResetTimeout: 60 * time.Second,
	}
}

// CircuitBreaker implements the circuit breaker pattern.
type CircuitBreaker struct {
	lastFailTime time.Time
	config       *CircuitBreakerConfig
	state        CircuitState
	failures     int
	mu           sync.RWMutex
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(config *CircuitBreakerConfig) *CircuitBreaker {
	if config == nil {
		config = DefaultCircuitBreakerConfig()
	}

	return &CircuitBreaker{
		config: config,
		state:  StateClosed,
	}
}

// ErrCircuitOpen is returned when the circuit breaker is open.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Execute runs a function through the circuit breaker.
func (cb *CircuitBreaker) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if !cb.canExecute() {
		return ErrCircuitOpen
	}

	err := fn(ctx)
	cb.recordResult(err == nil)
	return err
}

// canExecute checks if the function can be executed based on circuit state.
func (cb *CircuitBreaker) canExecute() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		return time.Since(cb.lastFailTime) >= cb.config.ResetTimeout
	case StateHalfOpen:
		return true
	default:
		return false
	}
}

// recordResult records the result of an execution.
func (cb *CircuitBreaker) recordResult(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if success {
		cb.onSuccess()
	} else {
		cb.onFailure()
	}
}

// onSuccess handles successful execution.
func (cb *CircuitBreaker) onSuccess() {
	if cb.state == StateHalfOpen {
		cb.setState(StateClosed)
		cb.failures = 0
	}
}

// onFailure handles failed execution.
func (cb *CircuitBreaker) onFailure() {
	cb.failures++
	cb.lastFailTime = time.Now()

	if cb.state == StateHalfOpen || cb.failures >= cb.config.MaxFailures {
		cb.setState(StateOpen)
	}
}

// setState changes the circuit breaker state.
func (cb *CircuitBreaker) setState(newState CircuitState) {
	if cb.state == newState {
		return
	}

	oldState := cb.state
	cb.state = newState

	logger.Logger.Info("Circuit breaker state changed",
		zap.String("from", stateString(oldState)),
		zap.String("to", stateString(newState)),
		zap.Int("failures", cb.failures),
	)

	if cb.config.OnStateChange != nil {
		cb.config.OnStateChange(oldState, newState)
	}
}

// GetState returns the current state of the circuit breaker.
func (cb *CircuitBreaker) GetState() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetFailures returns the current failure count.
func (cb *CircuitBreaker) GetFailures() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.failures
}

// stateString converts CircuitState to string.
func stateString(state CircuitState) string {
	switch state {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// String returns a string representation of the circuit breaker.
func (cb *CircuitBreaker) String() string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return fmt.Sprintf("CircuitBreaker(state=%s, failures=%d)",
		stateString(cb.state), cb.failures)
}
