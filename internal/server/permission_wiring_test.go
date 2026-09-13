package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autonomousops-co/accretion/internal/auth"
	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newWiredServer builds a Server whose /api routes are mounted exactly the way
// production wires them (setupRoutes), with auth ENABLED and two scoped API-key
// identities so we can exercise the route -> permission wiring end to end.
func newWiredServer(t *testing.T, readerKey, chatKey string) *Server {
	t.Helper()

	cfg := &config.Config{}
	// A valid (>= 32 byte) session key so the enabled AuthManager constructs.
	cfg.Auth.SessionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// A non-empty whitelist, likewise: an enabled AuthManager fails closed on an
	// empty one, since that would admit every account completing the OAuth flow.
	// Irrelevant to these tests, which authenticate by API key.
	cfg.Auth.AllowedDomains = []string{"yourco.com"}
	cfg.Auth.APIKeyIdentities = []config.APIKeyIdentity{
		{Name: "reader", Hash: sha256Hex(readerKey), Permissions: []string{"read:memory"}},
		{Name: "chatter", Hash: sha256Hex(chatKey), Permissions: []string{"chat:query"}},
	}

	am, err := auth.NewAuthManager(&cfg.Auth, true, true)
	if err != nil {
		t.Fatalf("failed to build enabled AuthManager: %v", err)
	}

	// The API listener is what these tests exercise; the UI is irrelevant to
	// permission wiring and stays off.
	cfg.API.Enabled = true
	cfg.API.Port = 41337

	s := &Server{
		config:      cfg,
		authManager: am,
		dbClient:    database.NewMockClient(),
		llmClient:   llm.NewMockClient(),
	}
	s.setupRoutes()
	return s
}

func serve(s *Server, method, path, key, body string) *httptest.ResponseRecorder {
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reqBody)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	s.apiRouter.ServeHTTP(rec, req)
	return rec
}

// TestAPIRoutePermissionWiring is a server-level test for issue #12: it proves
// that RequirePermission is actually mounted on the /api routes with the correct
// permission, not merely that the middleware works in isolation. With auth
// enabled, a principal is admitted to a route only when it holds that route's
// required permission.
func TestAPIRoutePermissionWiring(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	const (
		readerKey = "reader-api-key-secret"
		chatKey   = "chatter-api-key-secret"
	)
	s := newWiredServer(t, readerKey, chatKey)

	t.Run("chat:query principal is denied a read:memory route", func(t *testing.T) {
		rec := serve(s, http.MethodGet, "/api/logs", chatKey, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET /api/logs with chat:query only: expected 403, got %d", rec.Code)
		}
	})

	t.Run("read:memory principal reaches a read:memory route", func(t *testing.T) {
		rec := serve(s, http.MethodGet, "/api/logs", readerKey, "")
		if rec.Code == http.StatusForbidden {
			t.Fatalf("GET /api/logs with read:memory: unexpected 403 (permission gate blocked a permitted principal)")
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/logs with read:memory: expected 200 (handler reached), got %d", rec.Code)
		}
	})

	t.Run("chat:query principal reaches the chat route", func(t *testing.T) {
		// Invalid JSON body: chatHandler returns 400 before touching the LLM, which
		// proves the request passed the chat:query gate and reached the handler.
		rec := serve(s, http.MethodPost, "/api/chat", chatKey, "{not-json")
		if rec.Code == http.StatusForbidden {
			t.Fatalf("POST /api/chat with chat:query: unexpected 403 (permission gate blocked a permitted principal)")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST /api/chat with chat:query: expected 400 (handler reached, invalid JSON), got %d", rec.Code)
		}
	})

	t.Run("read:memory principal is denied the chat route", func(t *testing.T) {
		rec := serve(s, http.MethodPost, "/api/chat", readerKey, `{"messages":[]}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST /api/chat with read:memory only: expected 403, got %d", rec.Code)
		}
	})
}
