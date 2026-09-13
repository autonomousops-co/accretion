package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/auth"
	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/tempo"
)

type Server struct {
	config *config.Config
	// One router per listener. Either may be nil when its listener is disabled;
	// Start refuses to run with both nil rather than idling with nothing bound.
	apiRouter            *mux.Router
	uiRouter             *mux.Router
	authManager          *auth.AuthManager
	dbClient             database.Interface
	llmClient            llm.Client
	tempoClient          *tempo.Client
	systemPromptTemplate string
	timeExtractionPrompt string
}

func New(cfg *config.Config, authEnabled bool) (*Server, error) {
	llmClient, err := llm.NewClient(&cfg.LLM)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	dbClient, err := database.NewClient(&cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client: %w", err)
	}

	authManager, err := auth.NewAuthManager(&cfg.Auth, authEnabled, cfg.UI.Enabled)
	if err != nil {
		return nil, fmt.Errorf("failed to create auth manager: %w", err)
	}

	promptBytes, err := os.ReadFile("prompts/system-prompt.md")
	if err != nil {
		return nil, fmt.Errorf("failed to read prompts/system-prompt.md: %w", err)
	}

	timePromptBytes, err := os.ReadFile("prompts/time-extraction.md")
	if err != nil {
		return nil, fmt.Errorf("failed to read prompts/time-extraction.md: %w", err)
	}

	s := &Server{
		config:               cfg,
		authManager:          authManager,
		llmClient:            llm.NewTracingDecorator(llmClient),
		dbClient:             database.NewTracingDecorator(dbClient),
		systemPromptTemplate: string(promptBytes),
		timeExtractionPrompt: string(timePromptBytes),
	}

	if cfg.Tempo.Enabled {
		s.tempoClient = tempo.NewClient(&cfg.Tempo)
	}

	s.setupRoutes()

	return s, nil
}

// newRouter returns a router carrying the middleware every listener needs:
// tracing and a per-request context logger, both skipping /api/health so a probe
// every few seconds does not fill the logs or the trace store.
func (s *Server) newRouter() *mux.Router {
	r := mux.NewRouter()

	// Add OpenTelemetry middleware, skipping meta/health routes
	r.Use(otelmux.Middleware("accretion-api",
		otelmux.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/api/health"
		}),
	))

	// Add context-logger middleware — creates a per-request logger with trace correlation
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l := logger.WithCtx(r.Context())
			ctx := logger.NewContext(r.Context(), l)
			if r.URL.Path != "/api/health" {
				l.Info("Request", zap.String("method", r.Method), zap.String("path", r.URL.Path))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	return r
}

// setupRoutes builds one router per enabled listener. The two route sets are
// disjoint apart from /api/health, which both serve so each listener can be
// probed independently.
func (s *Server) setupRoutes() {
	if s.config.API.Enabled {
		s.apiRouter = s.newRouter()
		s.registerAPIRoutes(s.apiRouter)
		s.apiRouter.HandleFunc("/api/health", s.healthHandler).Methods("GET")
	}
	if s.config.UI.Enabled {
		s.uiRouter = s.newRouter()
		s.registerUIRoutes(s.uiRouter)
		s.uiRouter.HandleFunc("/api/health", s.healthHandler).Methods("GET")
	}
}

func (s *Server) registerAPIRoutes(r *mux.Router) {
	// API routes. Each is gated on the coarse permission it needs;
	// RequirePermission wraps RequireAuth, so authentication is still enforced
	// (and, when auth is disabled, both pass through). Read endpoints require
	// read:memory; the chat endpoint requires chat:query.
	api := r.PathPrefix("/api").Subrouter()
	readMemory := s.authManager.RequirePermission("read:memory")
	chatQuery := s.authManager.RequirePermission("chat:query")
	api.Handle("/chat", chatQuery(http.HandlerFunc(s.chatHandler))).Methods("POST", "OPTIONS")
	api.Handle("/logs", readMemory(http.HandlerFunc(s.logsHandler))).Methods("GET")
	api.Handle("/resources", readMemory(http.HandlerFunc(s.resourcesHandler))).Methods("GET")
	api.Handle("/search", readMemory(http.HandlerFunc(s.searchHandler))).Methods("GET")
	api.Handle("/stats", readMemory(http.HandlerFunc(s.statsHandler))).Methods("GET")
	api.Handle("/traces", readMemory(http.HandlerFunc(s.tracesHandler))).Methods("GET")
	api.Handle("/traces/{traceID}", readMemory(http.HandlerFunc(s.traceHandler))).Methods("GET")
}

// registerUIRoutes wires the browser-facing surface: the OAuth flow, the static
// assets, and the UI itself. Split out so setupRoutes reads as "API always, UI
// when configured" rather than an if-block spanning forty lines.
func (s *Server) registerUIRoutes(r *mux.Router) {
	// Authentication routes (no auth required)
	authRouter := r.PathPrefix("/auth").Subrouter()
	authRouter.HandleFunc("/login", s.authManager.HandleLogin).Methods("GET")
	authRouter.HandleFunc("/callback", s.authManager.HandleCallback).Methods("GET")
	authRouter.HandleFunc("/logout", s.authManager.HandleLogout).Methods("GET", "POST")
	authRouter.HandleFunc("/user", s.userInfoHandler).Methods("GET")

	// Serve static files (no auth required for CSS/JS) with cache busting
	staticHandler := http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Add cache control headers to prevent aggressive caching during development
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.FileServer(http.Dir("./web/static/")).ServeHTTP(w, r)
	}))
	r.PathPrefix("/static/").Handler(staticHandler)

	// Serve the actual index.html file for root path (require authentication)
	r.Handle("/", s.authManager.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Add cache control headers to prevent caching during development
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		s.serveIndexWithConfig(w, r)
	}))).Methods("GET")
}

// listener pairs a configured http.Server with the name used in logs.
//
// tlsEnabled records whether this listener should serve HTTPS. The keypair
// itself already lives in srv.TLSConfig by the time Start runs, loaded and
// validated up front, so the goroutine only has to pick the right Serve call.
type listener struct {
	name       string
	srv        *http.Server
	tlsEnabled bool
}

// Start runs every enabled listener and blocks until a signal, then shuts them
// all down within one shared budget.
func (s *Server) Start() error {
	var listeners []listener

	if s.apiRouter != nil {
		// Load and validate the keypair HERE, on the calling goroutine, so an
		// unreadable or malformed certificate aborts startup with a returned
		// error. Doing it inside the listener goroutine below would turn it into
		// a Fatal from a background goroutine — and any "fall back to cleartext"
		// handling would silently serve the API in the clear.
		tlsConfig, err := s.config.API.TLS.Validate()
		if err != nil {
			return fmt.Errorf("api listener TLS: %w", err)
		}
		listeners = append(listeners, listener{
			name:       "api",
			tlsEnabled: tlsConfig != nil,
			srv: &http.Server{
				TLSConfig:   tlsConfig,
				Addr:        fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.API.Port),
				Handler:     s.apiRouter,
				ReadTimeout: 15 * time.Second,
				// Bounds the ENTIRE handler, so it must cover /api/chat's
				// non-streaming RAG flow of sequential LLM calls, which can exceed
				// 15s on a cold local LLM. Configurable via api.write_timeout
				// (ACCRETION_API_WRITE_TIMEOUT); the functional test client reads the
				// same variable so client and server timeouts agree.
				WriteTimeout:      s.config.API.GetWriteTimeout(),
				IdleTimeout:       60 * time.Second,
				ReadHeaderTimeout: 10 * time.Second,
			},
		})
	}

	if s.uiRouter != nil {
		listeners = append(listeners, listener{
			name: "ui",
			srv: &http.Server{
				Addr:    fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.UI.Port),
				Handler: s.uiRouter,
				// The UI serves templates and files. It needs none of the API's
				// write budget, and a long deadline on the surface carrying the
				// only unauthenticated non-health routes is worth avoiding.
				ReadTimeout:       15 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       60 * time.Second,
				ReadHeaderTimeout: 10 * time.Second,
			},
		})
	}

	// Nothing bound is a misconfiguration, not a valid state: the process would
	// come up healthy and serve no one.
	if len(listeners) == 0 {
		return fmt.Errorf("no listener enabled: set api.enabled or ui.enabled")
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	for _, l := range listeners {
		go func(l listener) {
			logger.Logger.Info("Listener starting",
				zap.String("listener", l.name), zap.String("addr", l.srv.Addr),
				zap.Bool("tls", l.tlsEnabled))
			// The certificate and key are already in srv.TLSConfig, validated
			// before this goroutine existed, so the empty filename arguments are
			// correct rather than an oversight: ListenAndServeTLS uses
			// TLSConfig.Certificates when both paths are empty.
			serve := l.srv.ListenAndServe
			if l.tlsEnabled {
				serve = func() error { return l.srv.ListenAndServeTLS("", "") }
			}
			if err := serve(); err != nil && err != http.ErrServerClosed {
				logger.Logger.Fatal("Listener failed",
					zap.String("listener", l.name), zap.Error(err))
			}
		}(l)
	}

	<-done
	logger.Logger.Info("Server is shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// One budget across all listeners: Shutdown returns when its own connections
	// have drained, so running them in sequence within a shared deadline is
	// enough and keeps the error handling simple.
	for _, l := range listeners {
		if err := l.srv.Shutdown(shutdownCtx); err != nil {
			logger.Logger.Error("Listener forced to shutdown",
				zap.String("listener", l.name), zap.Error(err))
		}
	}

	if s.dbClient != nil {
		if err := s.dbClient.Close(); err != nil {
			logger.Logger.Warn("Error closing database client", zap.Error(err))
		}
	}

	logger.Logger.Info("Server exited")
	return nil
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"status":"ok","service":"accretion-api"}`)); err != nil {
		logger.Logger.Error("Failed to write health response", zap.Error(err))
	}
}

func (s *Server) serveIndexWithConfig(w http.ResponseWriter, r *http.Request) {
	log := logger.FromCtx(r.Context())
	// Read the index.html file
	indexBytes, err := os.ReadFile("./web/static/index.html")
	if err != nil {
		http.Error(w, "Could not read index.html", http.StatusInternalServerError)
		return
	}

	// Inject logging configuration
	logLevel := s.config.Logging.FrontendLevel
	if logLevel == "" {
		logLevel = s.config.Logging.Level // fallback to server log level
	}
	if logLevel == "" {
		logLevel = "info" // final fallback
	}

	configScript := fmt.Sprintf(`<script>window.ACCRETION_LOG_LEVEL = '%s';</script>`, logLevel)

	// Insert the config script before the closing </head> tag
	indexHTML := string(indexBytes)
	indexHTML = strings.Replace(indexHTML, "</head>", configScript+"\n</head>", 1)

	if _, err := w.Write([]byte(indexHTML)); err != nil {
		log.Error("Failed to write index page", zap.Error(err))
	}
}
