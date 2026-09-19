package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/sessions"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// sha256Hex returns the hex-encoded SHA-256 digest of s, matching the form used
// to configure APIKeyIdentity.Hash.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func bearerRequest(method, path, key string) *http.Request {
	req := httptest.NewRequest(method, path, http.NoBody)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

// TestMain wires up a no-op logger so the package-global logger.Logger is safe
// to call from the code under test (it is otherwise only initialized at runtime).
func TestMain(m *testing.M) {
	logger.Logger = zap.NewNop()
	os.Exit(m.Run())
}

// baseAuthConfig satisfies both of NewAuthManager's fail-closed checks — one
// API-key identity and a non-empty user whitelist (issue #44). Tests that care
// about either override that field.
func baseAuthConfig() *config.AuthConfig {
	return &config.AuthConfig{
		OAuth:         config.OAuthConfig{Provider: "google"},
		AllowedEmails: []string{"user@yourco.com"},
		APIKeyIdentities: []config.APIKeyIdentity{
			{Name: "test", Hash: sha256Hex("test-key"), Permissions: []string{"read:memory"}},
		},
	}
}

// hasRole reports whether the user carries the role. A local test helper since
// User.HasRole was removed with its last production caller (issue #44).
func hasRole(u *User, role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// validHexKey is 64 hex characters, i.e. a 32-byte decoded session key.
const validHexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestNewAuthManager_FailsClosedOnWeakSessionKey covers issue #10 and #65: when
// auth is enabled the constructor must fail (not warn-and-continue) on a missing,
// non-hex, or sub-32-byte session key. Hex is the only accepted encoding, so the
// three failure modes must be distinguishable from their messages alone —
// wantContains asserts the operator is told which one they hit.
//
// It also covers #129: every one of those messages must name SESSION_KEY and
// where it comes from. That part is asserted for the whole table below rather
// than per-case, so a failure mode added later cannot omit it.
func TestNewAuthManager_FailsClosedOnWeakSessionKey(t *testing.T) {
	// A 64-character non-hex string: long enough that calling it "too weak" would
	// be actively misleading, so it must be reported as NOT HEX (issue #65).
	const longNonHexKey = "your-32-character-session-key-here-plus-more-padding-to-64-chars"
	// 32 hex characters decode to 16 bytes — the case the old raw-string fallback
	// rejected while letting the low-entropy placeholder through.
	const shortHexKey = "0123456789abcdef0123456789abcdef"
	// 65 characters, every one of them valid hex. hex.DecodeString fails with
	// ErrLength, not an invalid byte, so calling this "not valid hex" would send
	// the operator hunting for a bad character that is not there.
	const oddLengthHexKey = validHexKey + "a"
	// 96 hex characters = 48 bytes. Longer than the minimum and therefore VALID:
	// rejecting a higher-entropy key would repeat the shape of the original bug.
	const longerHexKey = validHexKey + "0123456789abcdef0123456789abcdef"

	tests := []struct {
		name         string
		sessionKey   string
		wantErr      bool
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "empty key errors",
			sessionKey:   "",
			wantErr:      true,
			wantContains: []string{"no session key is configured", "openssl rand -hex 32"},
		},
		{
			name:         "short raw key errors as non-hex",
			sessionKey:   "too-short",
			wantErr:      true,
			wantContains: []string{"not hex-encoded", "openssl rand -hex 32"},
		},
		{
			name:         "long non-hex key errors as non-hex, not as too weak",
			sessionKey:   longNonHexKey,
			wantErr:      true,
			wantContains: []string{"not hex-encoded", "64 characters", "openssl rand -hex 32"},
			wantAbsent:   []string{"too weak", "too short"},
		},
		{
			name:         "odd-length all-hex key errors as odd count, not as non-hex",
			sessionKey:   oddLengthHexKey,
			wantErr:      true,
			wantContains: []string{"65 hexadecimal characters", "odd count", "openssl rand -hex 32"},
			wantAbsent:   []string{"not hex-encoded", "not valid hex", "too weak"},
		},
		{
			name:         "short hex key errors naming chars and bytes",
			sessionKey:   shortHexKey,
			wantErr:      true,
			wantContains: []string{"too short", "32 hex characters", "16 bytes", "openssl rand -hex 32"},
			wantAbsent:   []string{"not hex-encoded"},
		},
		{
			name:         "6 hex chars errors naming chars and bytes",
			sessionKey:   "aabbcc",
			wantErr:      true,
			wantContains: []string{"too short", "6 hex characters", "3 bytes"},
		},
		{name: "valid 32-byte hex key ok", sessionKey: validHexKey, wantErr: false},
		{name: "longer 48-byte hex key ok", sessionKey: longerHexKey, wantErr: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseAuthConfig()
			cfg.SessionKey = tc.sessionKey

			am, err := NewAuthManager(cfg, true, true)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for session key %q, got nil", tc.sessionKey)
				}
				for _, want := range tc.wantContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error for %q must mention %q, got: %v", tc.sessionKey, want, err)
					}
				}
				for _, absent := range tc.wantAbsent {
					if strings.Contains(err.Error(), absent) {
						t.Errorf("error for %q must NOT say %q, got: %v", tc.sessionKey, absent, err)
					}
				}
				// `openssl rand -hex 32` says how to make a key but not where to
				// put it, which is the question an operator actually has when the
				// process refuses to start. Since `accretion generate config` no
				// longer writes one (#129), the message must name the variable and
				// both places it is sourced from.
				for _, want := range []string{"SESSION_KEY", ".env", "Kubernetes Secret"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error for %q must name %q so the operator knows where the key comes from, got: %v",
							tc.sessionKey, want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for session key %q: %v", tc.sessionKey, err)
			}
			if am == nil || !am.IsEnabled() {
				t.Fatalf("expected an enabled AuthManager for a valid key")
			}
		})
	}
}

// TestNewAuthManager_FailsClosedOnZeroIdentities covers issue #44: removing the
// shared unscoped key must not leave a deployment that authenticates nothing,
// silently. With auth enabled and no identities configured, construction fails.
func TestNewAuthManager_FailsClosedOnZeroIdentities(t *testing.T) {
	cfg := baseAuthConfig()
	cfg.SessionKey = validHexKey
	cfg.APIKeyIdentities = nil

	if _, err := NewAuthManager(cfg, true, true); err == nil {
		t.Fatal("expected an error when auth is enabled with zero API key identities")
	}

	// The auth-disabled (dev) path is unaffected.
	if _, err := NewAuthManager(cfg, false, true); err != nil {
		t.Fatalf("auth-disabled construction should not error: %v", err)
	}
}

// TestNewAuthManager_FailsClosedOnEmptyWhitelist covers issue #44: isUserAllowed
// treats an empty whitelist as allow-all, so an authenticated deployment that
// configures none of the four lists would admit every account that completes
// the OAuth flow. Construction must refuse rather than come up silently open.
func TestNewAuthManager_FailsClosedOnEmptyWhitelist(t *testing.T) {
	empty := func() *config.AuthConfig {
		cfg := baseAuthConfig()
		cfg.SessionKey = validHexKey
		cfg.AllowedEmails = nil
		return cfg
	}

	if _, err := NewAuthManager(empty(), true, true); err == nil {
		t.Fatal("expected an error when auth is enabled with an empty user whitelist")
	}

	// Any one of the four lists is enough to make the boundary explicit — and
	// each must actually BE a boundary. Asserting only that the constructor
	// accepts is what let an admin-only config through review: isUserAllowed
	// short-circuited to allow-all whenever the two Allowed* lists were empty,
	// so admin_emails alone started clean and admitted the whole internet.
	for _, tc := range []struct {
		name  string
		apply func(*config.AuthConfig)
	}{
		{"allowed emails", func(c *config.AuthConfig) { c.AllowedEmails = []string{"u@yourco.com"} }},
		{"allowed domains", func(c *config.AuthConfig) { c.AllowedDomains = []string{"yourco.com"} }},
		{"admin emails", func(c *config.AuthConfig) { c.AdminEmails = []string{"a@yourco.com"} }},
		{"admin domains", func(c *config.AuthConfig) { c.AdminDomains = []string{"yourco.com"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := empty()
			tc.apply(cfg)
			am, err := NewAuthManager(cfg, true, true)
			if err != nil {
				t.Fatalf("unexpected error with %s configured: %v", tc.name, err)
			}

			// A verified stranger from an unrelated domain must be denied by
			// every one of the four configurations.
			stranger := &User{Email: "attacker@gmail.com", Verified: true, HD: "gmail.com"}
			am.assignUserPermissions(stranger)
			if len(stranger.Permissions) > 0 {
				t.Fatalf("%s: stranger admitted with perms %v — this list is not a boundary",
					tc.name, stranger.Permissions)
			}

			// ...and the account the list names must still get in.
			insider := &User{Email: "u@yourco.com", Verified: true, HD: "yourco.com"}
			if tc.name == "admin emails" {
				insider.Email = "a@yourco.com"
			}
			am.assignUserPermissions(insider)
			if len(insider.Permissions) == 0 {
				t.Fatalf("%s: the named account was denied; the guard is too tight", tc.name)
			}
		})
	}

	// The auth-disabled (dev) path is unaffected.
	if _, err := NewAuthManager(empty(), false, true); err != nil {
		t.Fatalf("auth-disabled construction should not error: %v", err)
	}
}

// TestNewAuthManager_DisabledAllowsEmptyKey confirms the dev/auth-disabled path
// still constructs without a session key.
func TestNewAuthManager_DisabledAllowsEmptyKey(t *testing.T) {
	cfg := baseAuthConfig()
	cfg.SessionKey = ""

	am, err := NewAuthManager(cfg, false, true)
	if err != nil {
		t.Fatalf("auth-disabled construction should not error: %v", err)
	}
	if am == nil || am.IsEnabled() {
		t.Fatalf("expected a disabled AuthManager")
	}
}

// TestIsAPIRequest covers issue #13: the 401-vs-redirect decision must be based
// on the /api/ path prefix only, never on a client-supplied Accept header.
func TestIsAPIRequest(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		accept      string
		contentType string
		want        bool
	}{
		{name: "api path is an api request", path: "/api/logs", want: true},
		{name: "web path with json accept redirects", path: "/dashboard", accept: "application/json", want: false},
		{name: "web path with json content-type redirects", path: "/dashboard", contentType: "application/json", want: false},
		{name: "root web path redirects", path: "/", accept: "text/html", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, http.NoBody)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if got := isAPIRequest(req); got != tc.want {
				t.Fatalf("isAPIRequest(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestNewAuthManager_EncryptsSessionCookie covers issue #13: the stored session
// values must be encrypted, not merely signed. The weak substring check is kept,
// but the real proof is that the encrypted store round-trips the value while a
// signed-only store built with the SAME hash key cannot recover it — a
// signed-but-unencrypted cookie WOULD be readable by that signed-only store.
func TestNewAuthManager_EncryptsSessionCookie(t *testing.T) {
	// Hex is the only accepted session-key encoding (issue #65), so the
	// signed-only store below is reconstructed from the same decoded bytes.
	hashKey, err := hex.DecodeString(validHexKey)
	if err != nil {
		t.Fatalf("validHexKey is not hex: %v", err)
	}
	cfg := baseAuthConfig()
	cfg.SessionKey = validHexKey

	am, err := NewAuthManager(cfg, true, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	saveReq := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	session, err := am.sessionStore.New(saveReq, SessionName)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	const secretValue = "secret-user@example.com"
	// StateKey is a plain string key so gob can serialize it.
	session.Values[StateKey] = secretValue
	if saveErr := session.Save(saveReq, rec); saveErr != nil {
		t.Fatalf("failed to save session: %v", saveErr)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected a session cookie to be set")
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), secretValue) {
		t.Fatalf("session cookie leaked plaintext value")
	}

	// Use separate requests and New() (not Get()) so each decode runs the store's
	// own codecs rather than reusing gorilla's per-request registry cache.
	encReq := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	signedReq := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	for _, c := range cookies {
		encReq.AddCookie(c)
		signedReq.AddCookie(c)
	}

	// The encrypted store must be able to decrypt and round-trip the value.
	got, err := am.sessionStore.New(encReq, SessionName)
	if err != nil {
		t.Fatalf("encrypted store failed to read its own cookie: %v", err)
	}
	if got.Values[StateKey] != secretValue {
		t.Fatalf("encrypted store did not round-trip value, got %v", got.Values[StateKey])
	}

	// A signed-only store with the same hash key must NOT recover the value; if
	// the cookie were merely signed (not encrypted) it would decode successfully.
	signedOnly := sessions.NewCookieStore(hashKey)
	leaked, err := signedOnly.New(signedReq, SessionName)
	if err == nil && leaked.Values[StateKey] == secretValue {
		t.Fatalf("signed-only store decoded the value -> cookie is not encrypted")
	}
}

// TestAssignUserPermissions_EmailVerifiedAndHD covers issue #8: authorization
// must require a verified email, and for Google the domain whitelist must match
// the signed `hd` claim rather than an attacker-settable email suffix.
func TestAssignUserPermissions_EmailVerifiedAndHD(t *testing.T) {
	googleManager := func() *AuthManager {
		return &AuthManager{
			enabled: true,
			config: &config.AuthConfig{
				OAuth:          config.OAuthConfig{Provider: "google"},
				AllowedDomains: []string{"yourco.com"},
			},
		}
	}

	tests := []struct {
		name        string
		am          *AuthManager
		user        User
		wantAllowed bool
		wantRole    string
	}{
		{
			name:        "unverified email with matching hd is denied",
			am:          googleManager(),
			user:        User{Email: "user@yourco.com", Verified: false, HD: "yourco.com"},
			wantAllowed: false,
		},
		{
			name:        "verified email with matching hd gets user role",
			am:          googleManager(),
			user:        User{Email: "user@yourco.com", Verified: true, HD: "yourco.com"},
			wantAllowed: true,
			wantRole:    "user",
		},
		{
			name:        "forged suffix without hd is denied even if verified",
			am:          googleManager(),
			user:        User{Email: "attacker@yourco.com", Verified: true, HD: ""},
			wantAllowed: false,
		},
		{
			name:        "wrong hd is denied",
			am:          googleManager(),
			user:        User{Email: "user@yourco.com", Verified: true, HD: "evil.com"},
			wantAllowed: false,
		},
		{
			name: "non-google provider falls back to verified suffix match",
			am: &AuthManager{
				enabled: true,
				config: &config.AuthConfig{
					OAuth:          config.OAuthConfig{Provider: "okta"},
					AllowedDomains: []string{"yourco.com"},
				},
			},
			user:        User{Email: "user@yourco.com", Verified: true},
			wantAllowed: true,
			wantRole:    "user",
		},
		{
			name: "non-google provider denies unverified suffix match",
			am: &AuthManager{
				enabled: true,
				config: &config.AuthConfig{
					OAuth:          config.OAuthConfig{Provider: "okta"},
					AllowedDomains: []string{"yourco.com"},
				},
			},
			user:        User{Email: "user@yourco.com", Verified: false},
			wantAllowed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.user
			tc.am.assignUserPermissions(&u)

			allowed := len(u.Permissions) > 0
			if allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v (perms=%v), want %v", allowed, u.Permissions, tc.wantAllowed)
			}
			if tc.wantRole != "" && !hasRole(&u, tc.wantRole) {
				t.Fatalf("expected role %q, got roles %v", tc.wantRole, u.Roles)
			}
		})
	}
}

// TestClaimBool verifies the email_verified claim is read whether the IdP encodes
// it as a bool or the string "true".
func TestClaimBool(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]interface{}
		want   bool
	}{
		{name: "bool true", claims: map[string]interface{}{"email_verified": true}, want: true},
		{name: "bool false", claims: map[string]interface{}{"email_verified": false}, want: false},
		{name: "string true", claims: map[string]interface{}{"email_verified": "true"}, want: true},
		{name: "string false", claims: map[string]interface{}{"email_verified": "false"}, want: false},
		{name: "missing", claims: map[string]interface{}{}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimBool(tc.claims, "email_verified"); got != tc.want {
				t.Fatalf("claimBool = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuthenticateAPIKey covers issues #11 and #44: keys are matched by SHA-256
// digest, and every key is a named identity yielding its own scoped principal —
// there is no unscoped fallback left.
func TestAuthenticateAPIKey(t *testing.T) {
	const (
		botKey = "discord-bot-secret-key"
		mcpKey = "mcp-secret-key"
	)
	identities := []config.APIKeyIdentity{
		{Name: "discord-bot", Hash: sha256Hex(botKey), Permissions: []string{"read:memory", "chat:query"}},
		{Name: "mcp", Hash: sha256Hex(mcpKey), Permissions: []string{"read:memory"}},
	}
	am := &AuthManager{
		enabled: true,
		config:  &config.AuthConfig{APIKeyIdentities: identities},
		apiKeys: newAPIKeyTable(identities),
	}

	t.Run("missing header returns nil", func(t *testing.T) {
		if u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", "")); u != nil {
			t.Fatalf("expected nil for no Authorization header, got %+v", u)
		}
	})

	t.Run("bad key returns nil", func(t *testing.T) {
		if u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", "not-a-real-key")); u != nil {
			t.Fatalf("expected nil for unknown key, got %+v", u)
		}
	})

	t.Run("named bot key yields its own scoped principal", func(t *testing.T) {
		u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", botKey))
		if u == nil {
			t.Fatal("expected a user for the bot key")
		}
		if u.Name != "discord-bot" || u.ID != "api-key:discord-bot" {
			t.Fatalf("unexpected identity: name=%q id=%q", u.Name, u.ID)
		}
		if !u.HasPermission("read:memory") || !u.HasPermission("chat:query") {
			t.Fatalf("bot key should cover all tool routes, got perms %v", u.Permissions)
		}
	})

	t.Run("named mcp key is scoped down", func(t *testing.T) {
		u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", mcpKey))
		if u == nil || u.Name != "mcp" {
			t.Fatalf("expected mcp identity, got %+v", u)
		}
		if !u.HasPermission("read:memory") {
			t.Fatalf("mcp key should have read:memory, got %v", u.Permissions)
		}
		if u.HasPermission("chat:query") {
			t.Fatalf("mcp key should NOT have chat:query, got %v", u.Permissions)
		}
	})
}

// TestRequirePermission_Enforcement covers issue #12: annotated routes admit a
// principal only when it holds the required permission, and enforcement is
// bypassed entirely when auth is disabled.
func TestRequirePermission_Enforcement(t *testing.T) {
	const botKey = "bot-key"
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	identities := []config.APIKeyIdentity{
		{Name: "bot", Hash: sha256Hex(botKey), Permissions: []string{"chat:query"}},
	}
	enabled := &AuthManager{
		enabled: true,
		config:  &config.AuthConfig{APIKeyIdentities: identities},
		apiKeys: newAPIKeyTable(identities),
	}

	t.Run("permission held -> 200", func(t *testing.T) {
		h := enabled.RequirePermission("chat:query")(okHandler)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, bearerRequest(http.MethodPost, "/api/chat", botKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("permission missing -> 403", func(t *testing.T) {
		h := enabled.RequirePermission("read:memory")(okHandler)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/logs", botKey))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})

	t.Run("auth disabled -> pass through", func(t *testing.T) {
		disabled := &AuthManager{enabled: false, config: &config.AuthConfig{}}
		h := disabled.RequirePermission("read:memory")(okHandler)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/logs", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 pass-through when disabled, got %d", rec.Code)
		}
	})
}

// TestRequireAuth_AccessDeniedEscapesEmail covers issue #37: the web (non-API)
// access-denied page renders the authenticated user's email, which is
// attacker-influenced. The email MUST be HTML-escaped so crafted markup cannot
// be injected into the 403 page (reflected/stored XSS), and the Content-Type
// header must survive (it must be set before WriteHeader).
func TestRequireAuth_AccessDeniedEscapesEmail(t *testing.T) {
	cfg := baseAuthConfig()
	cfg.SessionKey = validHexKey
	am, err := NewAuthManager(cfg, true, true)
	if err != nil {
		t.Fatalf("failed to build auth manager: %v", err)
	}

	// An authenticated user with NO permissions is authenticated-but-unauthorized:
	// exactly the access-denied branch. Its email carries an XSS payload.
	const payloadEmail = `x<script>alert(1)</script>@e.com`
	userJSON, err := json.Marshal(User{Email: payloadEmail})
	if err != nil {
		t.Fatalf("failed to marshal user: %v", err)
	}

	// Persist the user into a session cookie the way HandleCallback does.
	saveRec := httptest.NewRecorder()
	saveReq := httptest.NewRequest(http.MethodGet, "/dashboard", http.NoBody)
	session, err := am.sessionStore.New(saveReq, SessionName)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	session.Values[sessionUserKey] = string(userJSON)
	if err := session.Save(saveReq, saveRec); err != nil {
		t.Fatalf("failed to save session: %v", err)
	}

	// Replay the cookie against RequireAuth on a web (non-/api) path.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", http.NoBody)
	for _, c := range saveRec.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // reaching next means the guard failed
	})
	am.RequireAuth(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unauthorized user, got %d", rec.Code)
	}

	// Result().Header snapshots at WriteHeader, so this also asserts the
	// Content-Type was set BEFORE WriteHeader rather than dropped.
	if ct := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html Content-Type, got %q", ct)
	}

	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("access-denied body contains unescaped <script>:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("access-denied body does not contain the escaped email:\n%s", body)
	}
}

// TestSessionRoundTrip_UserPersistsAndReloads covers issue #39: HandleCallback
// stored the user under the contextKey UserKey, which gorilla/sessions cannot
// gob-encode ("gob: type not registered for interface: auth.contextKey"), so
// session.Save 500'd every OAuth login. This asserts a populated User survives
// Save -> reload -> GetCurrentUser with no gob error. It fails before the fix
// (writing under the contextKey key) and passes after (plain-string session key).
func TestSessionRoundTrip_UserPersistsAndReloads(t *testing.T) {
	cfg := baseAuthConfig()
	cfg.SessionKey = validHexKey
	am, err := NewAuthManager(cfg, true, true)
	if err != nil {
		t.Fatalf("failed to build auth manager: %v", err)
	}

	want := User{
		ID:          "42",
		Email:       "user@example.com",
		Name:        "Test User",
		Roles:       []string{"viewer"},
		Permissions: []string{"chat:query"},
		Verified:    true,
	}
	userJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("failed to marshal user: %v", err)
	}

	// Persist the user into a session cookie exactly the way HandleCallback does.
	saveRec := httptest.NewRecorder()
	saveReq := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	session, err := am.sessionStore.New(saveReq, SessionName)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	session.Values[sessionUserKey] = string(userJSON)
	if err := session.Save(saveReq, saveRec); err != nil {
		t.Fatalf("session.Save failed to persist user (issue #39): %v", err)
	}

	// Replay the cookie and recover the user through the production read path.
	readReq := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	for _, c := range saveRec.Result().Cookies() {
		readReq.AddCookie(c)
	}
	got := am.GetCurrentUser(readReq)
	if got == nil {
		t.Fatalf("GetCurrentUser returned nil after round-trip")
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("round-tripped user mismatch: got %+v, want %+v", *got, want)
	}
}

// TestIsGoogleIssuer verifies only Google's two accepted issuer strings pass the
// explicit issuer check applied after idtoken.Validate.
func TestIsGoogleIssuer(t *testing.T) {
	tests := []struct {
		iss  string
		want bool
	}{
		{iss: "accounts.google.com", want: true},
		{iss: "https://accounts.google.com", want: true},
		{iss: "http://accounts.google.com", want: false},
		{iss: "accounts.evil.com", want: false},
		{iss: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.iss, func(t *testing.T) {
			if got := isGoogleIssuer(tc.iss); got != tc.want {
				t.Fatalf("isGoogleIssuer(%q) = %v, want %v", tc.iss, got, tc.want)
			}
		})
	}
}

// TestNewAPIKeyTable_SkipsInvalidHashesAtLoad covers issue #47: hashes are
// decoded once at construction, and an entry whose hash is not a 32-byte hex
// digest is dropped there (with a warning) instead of being re-rejected on every
// request. Valid neighbors must survive, and the roles default must be applied
// at load time now that the request path no longer applies it.
func TestNewAPIKeyTable_SkipsInvalidHashesAtLoad(t *testing.T) {
	table := newAPIKeyTable([]config.APIKeyIdentity{
		{Name: "not-hex", Hash: "zzzz"},
		{Name: "too-short", Hash: "abcd"},
		{Name: "empty", Hash: ""},
		{Name: "good", Hash: sha256Hex("good-key"), Permissions: []string{"read:memory"}},
		{Name: "with-roles", Hash: sha256Hex("other-key"), Roles: []string{"bot"}},
	})

	if len(table) != 2 {
		t.Fatalf("expected only the 2 valid identities, got %d: %+v", len(table), table)
	}
	if table[0].name != "good" || table[1].name != "with-roles" {
		t.Fatalf("unexpected surviving identities: %q, %q", table[0].name, table[1].name)
	}
	if want := sha256.Sum256([]byte("good-key")); table[0].hash != want {
		t.Fatalf("decoded hash mismatch for %q", table[0].name)
	}
	if !reflect.DeepEqual(table[0].roles, []string{"api-key"}) {
		t.Fatalf("expected default role for identity with no roles, got %v", table[0].roles)
	}
	if !reflect.DeepEqual(table[1].roles, []string{"bot"}) {
		t.Fatalf("configured roles should be preserved, got %v", table[1].roles)
	}
}

// TestAuthenticateAPIKey_InvalidHashIdentityIsUnusable pins the security-relevant
// half of the same change: an identity dropped at load can never authenticate,
// and a valid identity listed after it is still reachable.
func TestAuthenticateAPIKey_InvalidHashIdentityIsUnusable(t *testing.T) {
	const goodKey = "good-key"
	identities := []config.APIKeyIdentity{
		{Name: "broken", Hash: "not-hex", Permissions: []string{"read:memory"}},
		{Name: "good", Hash: sha256Hex(goodKey), Permissions: []string{"read:memory"}},
	}
	am := &AuthManager{
		enabled: true,
		config:  &config.AuthConfig{APIKeyIdentities: identities},
		apiKeys: newAPIKeyTable(identities),
	}

	if u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", "not-hex")); u != nil {
		t.Fatalf("an identity with an invalid hash must not authenticate, got %+v", u)
	}
	u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", goodKey))
	if u == nil || u.Name != "good" {
		t.Fatalf("expected the valid identity after a dropped one, got %+v", u)
	}
}

// TestNewAuthManager_FailsClosedOnAllInvalidHashes: identities declared but none
// usable is the same end state as none declared — no machine caller can
// authenticate — so the constructor must refuse rather than come up silently.
func TestNewAuthManager_FailsClosedOnAllInvalidHashes(t *testing.T) {
	cfg := baseAuthConfig()
	cfg.SessionKey = validHexKey
	cfg.APIKeyIdentities = []config.APIKeyIdentity{
		{Name: "broken", Hash: "not-hex"},
	}

	if _, err := NewAuthManager(cfg, true, true); err == nil {
		t.Fatal("expected NewAuthManager to fail when no identity has a valid hash")
	} else if !strings.Contains(err.Error(), "valid hash") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// benchIdentities builds n distinct identities, each with key "key-<i>". A
// benchmark that wants the matching case can present one of those tokens; the
// benchmark below deliberately does not, measuring only the full-scan miss.
func benchIdentities(n int) []config.APIKeyIdentity {
	identities := make([]config.APIKeyIdentity, n)
	for i := range identities {
		identities[i] = config.APIKeyIdentity{
			Name:        fmt.Sprintf("client-%d", i),
			Hash:        sha256Hex(fmt.Sprintf("key-%d", i)),
			Permissions: []string{"read:memory"},
		}
	}
	return identities
}

// BenchmarkAuthenticateAPIKey measures the worst case (full scan, no match),
// which is what an unauthenticated or wrong-key request costs.
//
// Recorded on Apple M3 Max (go test ./internal/auth/ -run=XXX
// -bench=AuthenticateAPIKey -benchmem, go1.24.5, 2026-08-25). "legacy" is the
// pre-#47 path that hex-decoded every identity's hash per request; it was
// measured with a replica of that code which has since been deleted (#59),
// so those figures are a historical record and cannot be re-derived here.
// Recover the original via `git show main:internal/auth/auth.go` if a
// fresh before/after on new hardware is ever needed.
//
//	N     legacy                 hoisted (pre-#60)    hoisted (post-#60)
//	1       178 ns,   4 allocs     132 ns, 3 allocs     88 ns, 0 allocs
//	3       297 ns,   6 allocs     161 ns, 3 allocs    115 ns, 0 allocs
//	10      667 ns,  13 allocs     251 ns, 3 allocs    208 ns, 0 allocs
//	50     2.77 µs,  53 allocs     741 ns, 3 allocs    685 ns, 0 allocs
//	500    26.4 µs, 503 allocs    6.12 µs, 3 allocs   5.98 µs, 0 allocs
//
// What the #47 hoisting removed is the one-per-identity hex decode, so hoisted
// allocations became constant in N while legacy grew linearly. The 3 that
// remained were all the miss-path debug log, and they account for its 80 B
// exactly: the variadic []zap.Field backing array (64 B — zap.Field is 64 bytes
// wide), the `make([]byte, ~r0)` hex destination (8 B), and the
// `string(hex.dst)` conversion (8 B). #60 put that log behind a level check, so
// at Info — the production default — none of the three happen.
//
// The result is 0, not the 2 that #60 predicted. The ticket assumed the
// sha256 digest was one of the three; it never was. `go build -gcflags=-m`
// reports "digest does not escape" and lists no "moved to heap: presented" in
// either version — the digest was always on the stack. The AC simply miscounted;
// there is no extra escape-analysis win here to look for.
//
// With Debug actually enabled the log is emitted and those allocations come
// back, at no measurable added cost: against a real core writing to io.Discard,
// Debug measures 415.7 ns before and 417.2 ns after, 3 allocs both ways, and the
// Check against a live Info core costs ~3 ns and inlines. This row measures the
// level-disabled path, which is the one production takes; a real Info core gives
// the same 0 allocs as TestMain's Nop one, so the win is not a Nop artifact.
// Numbers are a record, not an assertion — re-run rather than trusting them.
func BenchmarkAuthenticateAPIKey(b *testing.B) {
	for _, n := range []int{1, 3, 10, 50, 500} {
		identities := benchIdentities(n)
		req := bearerRequest(http.MethodGet, "/api/logs", "no-such-key")

		am := &AuthManager{
			enabled: true,
			config:  &config.AuthConfig{APIKeyIdentities: identities},
			apiKeys: newAPIKeyTable(identities),
		}
		b.Run(fmt.Sprintf("hoisted/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if u := am.authenticateAPIKey(req); u != nil {
					b.Fatal("unexpected match")
				}
			}
		})
	}
}

// TestAuthenticateAPIKey_DebugLogFields pins the fields of both API-key log
// lines now that #60 emits them through Check/Write rather than Logger.Debug:
// the level guard must change when the work happens, never what is logged.
func TestAuthenticateAPIKey_DebugLogFields(t *testing.T) {
	const goodKey = "good-key"
	identities := []config.APIKeyIdentity{
		{Name: "good", Hash: sha256Hex(goodKey), Permissions: []string{"read:memory"}},
	}
	am := &AuthManager{
		enabled: true,
		config:  &config.AuthConfig{APIKeyIdentities: identities},
		apiKeys: newAPIKeyTable(identities),
	}

	rec := &recordingCore{level: zapcore.DebugLevel}
	prev := logger.Logger
	logger.Logger = zap.New(rec)
	defer func() { logger.Logger = prev }()

	if u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", goodKey)); u == nil {
		t.Fatal("expected the valid key to authenticate")
	}
	if u := am.authenticateAPIKey(bearerRequest(http.MethodGet, "/api/logs", "wrong-key")); u != nil {
		t.Fatalf("expected no match, got %+v", u)
	}

	entries := rec.entries
	if len(entries) != 2 {
		t.Fatalf("expected 2 debug entries, got %d: %+v", len(entries), entries)
	}

	matchDigest := sha256.Sum256([]byte(goodKey))
	if got, want := entries[0].entry.Message, "Authenticated via named API key"; got != want {
		t.Fatalf("match log message = %q, want %q", got, want)
	}
	wantMatch := map[string]any{
		"key_name":        "good",
		"key_fingerprint": keyFingerprint(matchDigest[:]),
	}
	if got := fieldMap(entries[0].fields); !reflect.DeepEqual(got, wantMatch) {
		t.Fatalf("match log fields = %+v, want %+v", got, wantMatch)
	}

	missDigest := sha256.Sum256([]byte("wrong-key"))
	if got, want := entries[1].entry.Message, "Invalid API key provided"; got != want {
		t.Fatalf("miss log message = %q, want %q", got, want)
	}
	wantMiss := map[string]any{"key_fingerprint": keyFingerprint(missDigest[:])}
	if got := fieldMap(entries[1].fields); !reflect.DeepEqual(got, wantMiss) {
		t.Fatalf("miss log fields = %+v, want %+v", got, wantMiss)
	}
}

// TestAuthenticateAPIKey_NoFingerprintWorkAboveDebug is the other half of #60:
// at the production default level neither call site may do the hex encode. It is
// asserted as zero allocations, which the encode (and the zap field it feeds)
// cannot avoid making.
func TestAuthenticateAPIKey_NoFingerprintWorkAboveDebug(t *testing.T) {
	identities := []config.APIKeyIdentity{
		{Name: "good", Hash: sha256Hex("good-key"), Permissions: []string{"read:memory"}},
	}
	am := &AuthManager{
		enabled: true,
		config:  &config.AuthConfig{APIKeyIdentities: identities},
		apiKeys: newAPIKeyTable(identities),
	}

	// A live core set above Debug, not a Nop core: production runs a real core
	// that merely declines Debug, and that is what the guard must beat.
	prev := logger.Logger
	logger.Logger = zap.New(&recordingCore{level: zapcore.InfoLevel})
	defer func() { logger.Logger = prev }()

	req := bearerRequest(http.MethodGet, "/api/logs", "no-such-key")
	if got := testing.AllocsPerRun(100, func() {
		if u := am.authenticateAPIKey(req); u != nil {
			t.Fatal("unexpected match")
		}
	}); got != 0 {
		t.Fatalf("miss path allocated %v times at Info level; the fingerprint hex is not guarded", got)
	}
}

// recordingCore is a minimal zapcore.Core that captures entries and their
// fields. zaptest/observer would do this, but it is not vendored and vendor/ is
// not this change's to touch.
type recordingCore struct {
	level   zapcore.Level
	entries []recordedEntry
}

type recordedEntry struct {
	entry  zapcore.Entry
	fields []zap.Field
}

func (c *recordingCore) Enabled(l zapcore.Level) bool { return l >= c.level }

// With is deliberately the identity. Returning a derived core would either drop
// its entries (a fresh core has its own nil slice) or need field plumbing no test
// exercises. If a future test needs With semantics it fails loudly — the fields
// just will not appear — rather than silently losing log lines.
func (c *recordingCore) With([]zap.Field) zapcore.Core { return c }

func (c *recordingCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *recordingCore) Write(e zapcore.Entry, fields []zap.Field) error {
	c.entries = append(c.entries, recordedEntry{entry: e, fields: fields})
	return nil
}

func (c *recordingCore) Sync() error { return nil }

// fieldMap renders recorded fields as name -> value for comparison.
func fieldMap(fields []zap.Field) map[string]any {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	return enc.Fields
}

// The whitelist guard exists because an empty whitelist admits every account
// that completes the OAuth flow. With the UI disabled the OAuth routes are never
// registered, so there is no flow to complete — requiring a whitelist there
// would force an API-only deployment to name users who could never log in.
//
// The relaxation is scoped to exactly that: API-key authentication never
// consults the whitelist (HandleCallback is its only caller, via
// assignUserPermissions), so nothing else loosens.
func TestNewAuthManager_WhitelistRequiredOnlyWhenOAuthRoutesExist(t *testing.T) {
	noWhitelist := func() *config.AuthConfig {
		cfg := baseAuthConfig()
		cfg.SessionKey = validHexKey
		cfg.AllowedEmails = nil
		cfg.AllowedDomains = nil
		cfg.AdminEmails = nil
		cfg.AdminDomains = nil
		return cfg
	}

	if _, err := NewAuthManager(noWhitelist(), true, true); err == nil {
		t.Fatal("oauth routes registered and no whitelist: want a fail-closed error, got nil")
	}

	if _, err := NewAuthManager(noWhitelist(), true, false); err != nil {
		t.Fatalf("oauth routes absent and no whitelist: want success, got %v", err)
	}

	// Auth itself is untouched: a weak session key still fails either way.
	weak := noWhitelist()
	weak.SessionKey = "short"
	if _, err := NewAuthManager(weak, true, false); err == nil {
		t.Fatal("weak session key with oauth disabled: want an error, got nil")
	}
}
