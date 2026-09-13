package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/auth"
	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// NewAuthManager logs during construction, so the package-global logger must be
// non-nil for these tests to run on their own rather than only as part of a
// full-package run that happens to initialize it first.
func TestMain(m *testing.M) {
	logger.Logger = zap.NewNop()
	os.Exit(m.Run())
}

// serverWithUI wires a Server the way production does, with the UI on or off, so
// the assertions below are about setupRoutes rather than a hand-built router.
func serverWithUI(t *testing.T, uiEnabled bool) *Server {
	t.Helper()

	cfg := &config.Config{}
	cfg.UI.Enabled = uiEnabled
	cfg.UI.Port = 8080
	cfg.API.Enabled = true
	cfg.API.Port = 41337
	cfg.Auth.SessionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cfg.Auth.APIKeyIdentities = []config.APIKeyIdentity{
		{Name: "reader", Hash: sha256Hex("k"), Permissions: []string{"read:memory"}},
	}
	// Only supplied when the OAuth routes exist. With the UI off the whitelist
	// gates nothing, which is the point of the case below.
	if uiEnabled {
		cfg.Auth.AllowedDomains = []string{"yourco.com"}
	}

	am, err := auth.NewAuthManager(&cfg.Auth, true, uiEnabled)
	if err != nil {
		t.Fatalf("NewAuthManager(uiEnabled=%v): %v", uiEnabled, err)
	}

	s := &Server{
		config:      cfg,
		authManager: am,
		dbClient:    database.NewMockClient(),
		llmClient:   llm.NewMockClient(),
	}
	s.setupRoutes()
	return s
}

// status drives one router directly, so these assertions are about which routes
// a listener carries rather than about anything network-level.
func status(r *mux.Router, path string) int {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec.Code
}

// With the UI off there is no UI listener at all, and the API listener carries
// none of the browser surface — so exposing the API cannot expose /auth/*, the
// only unauthenticated non-health routes in the system.
func TestUIDisabled_NoUIListenerAndAPIUnaffected(t *testing.T) {
	off := serverWithUI(t, false)

	if off.uiRouter != nil {
		t.Fatal("ui disabled: a UI router was built; nothing should listen for the browser surface")
	}
	if off.apiRouter == nil {
		t.Fatal("api enabled: no API router was built")
	}

	for _, path := range []string{"/", "/static/app.css", "/auth/login", "/auth/callback"} {
		if got := status(off.apiRouter, path); got != http.StatusNotFound {
			t.Errorf("api listener: GET %s = %d, want 404 (browser routes belong to the UI listener)", path, got)
		}
	}

	// Unauthenticated and probed by the kubelet: every listener serves it.
	if got := status(off.apiRouter, "/api/health"); got != http.StatusOK {
		t.Errorf("api listener: GET /api/health = %d, want 200", got)
	}

	// Still mounted, still guarded — 401, not 404.
	if got := status(off.apiRouter, "/api/stats"); got == http.StatusNotFound {
		t.Error("api listener: /api/stats is not registered")
	}
}

// With both on, each listener serves only its own routes. The disjointness is
// the point: neither can be reached through the other's port.
func TestBothEnabled_RoutesAreDisjoint(t *testing.T) {
	on := serverWithUI(t, true)

	if on.uiRouter == nil || on.apiRouter == nil {
		t.Fatal("both enabled: expected two routers")
	}

	for _, path := range []string{"/", "/auth/login"} {
		if got := status(on.uiRouter, path); got == http.StatusNotFound {
			t.Errorf("ui listener: GET %s = 404, want the route registered", path)
		}
		if got := status(on.apiRouter, path); got != http.StatusNotFound {
			t.Errorf("api listener: GET %s = %d, want 404 (browser route leaked onto the API)", path, got)
		}
	}

	if got := status(on.uiRouter, "/api/stats"); got != http.StatusNotFound {
		t.Errorf("ui listener: GET /api/stats = %d, want 404 (API route leaked onto the UI)", got)
	}

	// Both listeners must be independently probeable.
	for name, r := range map[string]*mux.Router{"ui": on.uiRouter, "api": on.apiRouter} {
		if got := status(r, "/api/health"); got != http.StatusOK {
			t.Errorf("%s listener: GET /api/health = %d, want 200", name, got)
		}
	}
}
