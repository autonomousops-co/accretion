package testutil

import (
	"os"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// TestingT is an interface that both *testing.T and *testing.B implement
type TestingT interface {
	Fatalf(format string, args ...interface{})
	Cleanup(func())
}

// SetupTestLogger initializes a test logger that outputs to stdout during tests.
// This prevents nil pointer panics when code tries to log during tests.
func SetupTestLogger(t *testing.T) {
	// Create a test logger configuration
	config := zap.NewDevelopmentConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel) // Only show errors during tests
	config.DisableCaller = true
	config.DisableStacktrace = true
	config.OutputPaths = []string{"stdout"}

	// Build the logger
	zapLogger, err := config.Build()
	if err != nil {
		t.Fatalf("Failed to create test logger: %v", err)
	}

	// Set the global logger
	logger.Logger = zapLogger

	// Cleanup function to restore original logger after test
	t.Cleanup(func() {
		zapLogger.Sync()
	})
}

// SetupQuietTestLogger initializes a logger that discards all output.
// Use this when you want to suppress all logging during tests.
func SetupQuietTestLogger(t TestingT) {
	// Create a logger that discards all output
	config := zap.NewDevelopmentConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.FatalLevel) // Only fatal (effectively nothing)
	config.OutputPaths = []string{"/dev/null"}
	if os.Getenv("OS") == "Windows_NT" {
		config.OutputPaths = []string{"nul"}
	}

	zapLogger, err := config.Build()
	if err != nil {
		t.Fatalf("Failed to create quiet test logger: %v", err)
	}

	logger.Logger = zapLogger

	t.Cleanup(func() {
		zapLogger.Sync()
	})
}

// SetupTestEnvironment sets up a complete test environment with logger and other necessities.
func SetupTestEnvironment(t *testing.T) {
	SetupQuietTestLogger(t)
}
