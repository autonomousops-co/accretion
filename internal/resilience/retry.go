package resilience

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// RetryConfig defines retry behavior configuration.
type RetryConfig struct {
	RetryableErrors []error
	MaxAttempts     int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	BackoffFactor   float64
	Jitter          bool
}

// DefaultRetryConfig returns a sensible default retry configuration.
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxAttempts:   3,
		InitialDelay:  100 * time.Millisecond,
		MaxDelay:      5 * time.Second,
		BackoffFactor: 2.0,
		Jitter:        true,
	}
}

// RetryableFunc is a function that can be retried.
type RetryableFunc func(ctx context.Context) error

// WithRetry executes a function with exponential backoff retry logic.
func WithRetry(ctx context.Context, config *RetryConfig, fn RetryableFunc) error {
	if config == nil {
		config = DefaultRetryConfig()
	}

	var lastErr error
	for attempt := 0; attempt < config.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := calculateDelay(config, attempt)
			logger.Logger.Debug("Retrying operation",
				zap.Int("attempt", attempt+1),
				zap.Int("max_attempts", config.MaxAttempts),
				zap.Duration("delay", delay),
			)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				// Continue with retry
			}
		}

		if err := fn(ctx); err != nil {
			lastErr = err
			if !isRetryable(err, config.RetryableErrors) {
				return fmt.Errorf("non-retryable error: %w", err)
			}

			logger.Logger.Debug("Operation failed, will retry",
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
			continue
		}

		// Success
		return nil
	}

	return fmt.Errorf("max retry attempts exceeded: %w", lastErr)
}

// calculateDelay calculates the delay for the given attempt.
func calculateDelay(config *RetryConfig, attempt int) time.Duration {
	delay := float64(config.InitialDelay) * math.Pow(config.BackoffFactor, float64(attempt))

	if delay > float64(config.MaxDelay) {
		delay = float64(config.MaxDelay)
	}

	if config.Jitter {
		// Add jitter to prevent thundering herd. Not a security boundary: this
		// only decorrelates retry timing between replicas, and predicting it
		// gains an attacker nothing.
		// #nosec G404 -- math/rand is correct for backoff jitter
		jitter := rand.Float64() * 0.1 * delay
		delay += jitter
	}

	return time.Duration(delay)
}

// isRetryable checks if an error should be retried.
func isRetryable(err error, retryableErrors []error) bool {
	if len(retryableErrors) == 0 {
		// By default, retry all errors
		return true
	}

	for _, retryableErr := range retryableErrors {
		if err == retryableErr {
			return true
		}
	}
	return false
}
