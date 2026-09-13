package cmd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// HealthServer represents a health check HTTP server
type HealthServer struct {
	server *http.Server
	name   string
	port   int
}

// NewHealthServer creates a health server that is always ready, which is
// correct for a process whose single worker is the only thing keeping it alive.
func NewHealthServer(port int, serviceName string) *HealthServer {
	return NewHealthServerWithReadiness(port, serviceName, nil)
}

// NewHealthServerWithReadiness creates a health server whose /ready endpoint
// consults ready. A nil ready is always ready.
//
// This exists for the consolidated collector processes (#91, #92). Once several
// collectors share a pod, "the process is up" stops being evidence that
// collection is happening: one collector can be wedged while the others run
// fine. Passing the supervisor's health in here is what makes that visible
// instead of leaving a half-dead pod reporting Ready.
func NewHealthServerWithReadiness(port int, serviceName string, ready func() bool) *HealthServer {
	mux := http.NewServeMux()

	// Health endpoint. Liveness stays unconditional: a wedged collector is a
	// readiness problem, and restarting the whole pod would take the healthy
	// collectors down with it. The supervisor restarts the failing one.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintf(w, `{"status":"ok","service":"%s"}`, serviceName); err != nil {
			logger.Logger.Error("Failed to write health response", zap.Error(err))
		}
	})

	// Ready endpoint
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		status, body := "ready", http.StatusOK
		if ready != nil && !ready() {
			status, body = "degraded", http.StatusServiceUnavailable
		}

		w.WriteHeader(body)
		if _, err := fmt.Fprintf(w, `{"status":"%s","service":"%s"}`, status, serviceName); err != nil {
			logger.Logger.Error("Failed to write ready response", zap.Error(err))
		}
	})

	return &HealthServer{
		server: &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: mux,
			// Without this a client can hold a connection open indefinitely by
			// dribbling headers (Slowloris). The health endpoint is unauthenticated
			// and reachable from anywhere in the cluster, so it is the easiest
			// thing here to tie up.
			ReadHeaderTimeout: 10 * time.Second,
		},
		port: port,
		name: serviceName,
	}
}

// Start starts the health server in a goroutine
func (s *HealthServer) Start() {
	go func() {
		logger.Logger.Info("Starting health check server", zap.String("service", s.name), zap.Int("port", s.port))
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Logger.Error("Health server error", zap.String("service", s.name), zap.Error(err))
		}
	}()
}

// Stop gracefully shuts down the health server
func (s *HealthServer) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger.Logger.Info("Stopping health server", zap.String("service", s.name))
	return s.server.Shutdown(ctx)
}
