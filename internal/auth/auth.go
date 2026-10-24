package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/sessions"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// Define custom types for context keys to avoid collisions
type contextKey string

const (
	SessionName = "accretion-api-session"
	// UserKey carries the authenticated *User in the request context. It is a
	// distinct contextKey type to avoid collisions with other packages' keys.
	UserKey = contextKey("user")
	// sessionUserKey stores the user in session.Values. It must be a plain string:
	// gorilla/sessions gob-encodes session.Values, and gob cannot encode a map
	// keyed by an unregistered named type such as contextKey (issue #39).
	sessionUserKey = "user"
	StateKey       = "oauth_state"
)

type User struct {
	ID          string   `json:"id"`
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	Picture     string   `json:"picture"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Verified    bool     `json:"verified"`
	// HD is the Google Workspace hosted-domain claim. On the primary Google path
	// it comes from the verified ID token, where it is signed and authoritative
	// for domain-based authorization, unlike the email suffix which the OAuth
	// provider does not vouch for. On the Google userinfo fallback it is instead
	// copied from the unsigned userinfo response (googleUserInfo.toUser); in
	// practice Google's v2 userinfo does not return hd, and domainMatches only
	// consults HD on the Google branch behind a Verified check, so this stays
	// fail-closed.
	HD string `json:"hd"`
}

type AuthManager struct {
	config       *config.AuthConfig
	oauthConfig  *oauth2.Config
	sessionStore *sessions.CookieStore
	enabled      bool
	// apiKeys is the decoded form of config.APIKeyIdentities, built once at
	// construction. It is read-only after NewAuthManager returns, so the
	// per-request scan in authenticateAPIKey needs no locking.
	apiKeys []apiKeyIdentity
}

// apiKeyIdentity is a config.APIKeyIdentity with its hex hash already decoded.
// The digest is a fixed-size array rather than a slice so the whole table is one
// allocation and the per-request scan allocates nothing (issue #47).
type apiKeyIdentity struct {
	name        string
	hash        [sha256.Size]byte
	roles       []string
	permissions []string
}

// newAPIKeyTable decodes every configured identity hash once. Identities whose
// hash is not a 32-byte hex string are dropped here, with the same warning that
// used to be emitted per request — so a typo'd hash is visible at startup rather
// than only in the logs of whichever request happened to scan past it.
func newAPIKeyTable(identities []config.APIKeyIdentity) []apiKeyIdentity {
	table := make([]apiKeyIdentity, 0, len(identities))
	for _, identity := range identities {
		raw, err := hex.DecodeString(identity.Hash)
		if err != nil || len(raw) != sha256.Size {
			logger.Logger.Warn("Skipping API key identity with invalid hash",
				zap.String("key_name", identity.Name))
			continue
		}
		entry := apiKeyIdentity{
			name:        identity.Name,
			roles:       identity.Roles,
			permissions: identity.Permissions,
		}
		copy(entry.hash[:], raw)
		if len(entry.roles) == 0 {
			entry.roles = []string{"api-key"}
		}
		table = append(table, entry)
	}
	return table
}

// sessionKeySource names the variable that carries the session key and where it
// is supposed to come from. Every decodeSessionKey error ends with it.
//
// `openssl rand -hex 32` alone tells an operator how to MAKE a key but not
// where to put it, which is the actual question when a process refuses to
// start (#129). Since `accretion generate config` no longer writes a key into
// the generated file, the environment is the path an operator is expected to
// take, so the error has to say so.
//
// Deliberately phrased as where the value SHOULD come from, not as a claim
// that a config file is refused: auth.session_key is still read from config if
// present, and viper gives the environment precedence over it either way.
const sessionKeySource = "The key comes from the SESSION_KEY environment variable: the gitignored .env " +
	"locally, a Kubernetes Secret via secretKeyRef in-cluster. Generate one with `openssl rand -hex 32`"

// decodeSessionKey validates the configured session signing key and returns its
// decoded bytes.
//
// When auth is enabled we FAIL CLOSED on a missing or weak key: silently
// generating a random per-process key means each replica signs with a
// different key (so cookies from one pod are rejected by another) and every
// restart logs users out. Auto-generation is only acceptable on the
// auth-disabled (dev) path, which returns before this is reached.
func decodeSessionKey(sessionKey string) ([]byte, error) {
	logger.Logger.Debug("Session key configured",
		zap.Bool("present", sessionKey != ""), zap.Int("char_length", len(sessionKey)))

	if sessionKey == "" {
		return nil, fmt.Errorf("auth is enabled but no session key is configured: it must be a hex-encoded "+
			"key of at least 64 hex characters (32 bytes). %s", sessionKeySource)
	}

	// Hex is the ONLY accepted encoding. The former raw-string fallback made the
	// same 32-character input mean either 16 bytes (valid hex) or 32 bytes (not
	// hex), so a low-entropy English placeholder passed the 32-byte check while a
	// cryptographically random `openssl rand -hex 16` was rejected. One encoding,
	// minimum 32 decoded bytes; `openssl rand -hex 32` produces exactly that.
	// LONGER keys stay valid on purpose — rejecting a higher-entropy key would
	// repeat the shape of the bug this replaced, and the derived block key below
	// is only better fed by more input.
	keyBytes, err := hex.DecodeString(sessionKey)
	if err != nil {
		// An odd character count is its OWN failure: every character is valid hex,
		// there is just no whole number of bytes to decode into. Reporting that as
		// "not valid hex" sends the operator hunting for a non-hex character that
		// is not there. Phrasing matches scripts/make-secret.sh (#64).
		if errors.Is(err, hex.ErrLength) {
			return nil, fmt.Errorf("auth is enabled but the session key is %d hexadecimal characters — an odd "+
				"count, so it decodes to no whole number of bytes: it must be an even count, at least 64 hex "+
				"characters (32 bytes). %s", len(sessionKey), sessionKeySource)
		}
		return nil, fmt.Errorf("auth is enabled but the session key is not hex-encoded: it must be a hex "+
			"string of at least 64 characters (32 bytes), got %d characters that are not valid hex. %s",
			len(sessionKey), sessionKeySource)
	}
	logger.Logger.Debug("Decoded hex session key",
		zap.Int("char_length", len(sessionKey)), zap.Int("decoded_byte_length", len(keyBytes)))
	if len(keyBytes) < 32 {
		return nil, fmt.Errorf("auth is enabled but the session key is too short: %d hex characters decode "+
			"to %d bytes, need at least 64 hex characters (32 bytes). %s",
			len(sessionKey), len(keyBytes), sessionKeySource)
	}
	return keyBytes, nil
}

// NewAuthManager builds the auth manager. oauthEnabled reports whether the OAuth
// routes are actually registered — it follows ui.enabled, because /auth/* exists
// only to log a human into the web UI. It gates the whitelist requirement below
// and nothing else: API-key authentication never consults the whitelist.
func NewAuthManager(cfg *config.AuthConfig, enabled, oauthEnabled bool) (*AuthManager, error) {
	logger.Logger.Debug("Initializing AuthManager",
		zap.Bool("enabled", enabled), zap.String("provider", cfg.OAuth.Provider))

	if !enabled {
		logger.Logger.Info("Authentication is disabled")
		return &AuthManager{
			config:  cfg,
			enabled: false,
		}, nil
	}

	// FAIL CLOSED on zero API-key identities. Every machine caller — the Discord
	// bot, each MCP client, CI — authenticates as a named identity, so an
	// authenticated deployment with none configured accepts no machine caller at
	// all. Deleting the shared unscoped key without this check would trade
	// "everyone over-privileged" for "nothing authenticates, silently".
	if len(cfg.APIKeyIdentities) == 0 {
		return nil, fmt.Errorf("auth is enabled but no API key identities are configured: " +
			"declare one entry per client under auth.api_key_identities " +
			"(see charts/accretion-api/values.yaml)")
	}

	// Decode the hashes once. An entry with an unusable hash is dropped, so a
	// config where EVERY entry is malformed lands in exactly the state the check
	// above refuses — identities declared, none usable, no machine caller able to
	// authenticate — and must fail closed for the same reason.
	apiKeys := newAPIKeyTable(cfg.APIKeyIdentities)
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("auth is enabled but none of the %d configured API key identities has a valid "+
			"hash: auth.api_key_identities[].hash must be a hex-encoded SHA-256 digest (64 hex characters)",
			len(cfg.APIKeyIdentities))
	}

	// FAIL CLOSED on an empty authorization whitelist. isUserAllowed treats "no
	// allowed emails and no allowed domains" as allow-all, so with OAuth
	// configured and every list empty, ANY Google account on the internet that
	// completes the flow is granted read:memory + chat:query. That is reachable
	// straight from the documented setup — copy env.example, fill in the three
	// required OAuth/session values, deploy — because the allowlist Secret keys
	// are optional and easy to leave unset. Same reasoning as the zero-identities
	// check above: refuse to start rather than come up silently open.
	// Only when the OAuth routes exist. The whitelist is consulted from exactly
	// one place — HandleCallback -> assignUserPermissions -> isUserAllowed — so
	// with /auth/* unregistered there is no flow to complete and nothing an empty
	// whitelist could admit. Requiring one anyway would force an API-only
	// deployment to name users who can never log in.
	if oauthEnabled &&
		len(cfg.AllowedEmails) == 0 && len(cfg.AllowedDomains) == 0 &&
		len(cfg.AdminEmails) == 0 && len(cfg.AdminDomains) == 0 {
		return nil, fmt.Errorf("auth is enabled but no user whitelist is configured: an empty whitelist " +
			"admits every account that completes the OAuth flow. Set at least one of auth.allowed_emails, " +
			"auth.allowed_domains, auth.admin_emails, auth.admin_domains")
	}

	// Counts only, never addresses: this bug class is "the operator believes the
	// boundary is narrower than it is", and one startup line makes the shape
	// visible without a live probe.
	logger.Logger.Info("Authorization whitelist configured",
		zap.Int("allowed_emails", len(cfg.AllowedEmails)),
		zap.Int("allowed_domains", len(cfg.AllowedDomains)),
		zap.Int("admin_emails", len(cfg.AdminEmails)),
		zap.Int("admin_domains", len(cfg.AdminDomains)))

	keyBytes, err := decodeSessionKey(cfg.SessionKey)
	if err != nil {
		return nil, err
	}

	// Derive a 32-byte block (encryption) key from the signing key so the stored
	// User (email/roles/permissions) is encrypted at rest in the cookie, not just
	// signed. A domain-separation prefix keeps the encryption key distinct from
	// the signing key even though both come from the same secret, and the
	// derivation is deterministic so the key is identical across replicas and
	// cookies remain valid cluster-wide.
	blockKey := sha256.Sum256(append([]byte("accretion-cookie-encryption-key\x00"), keyBytes...))
	store := sessions.NewCookieStore(keyBytes, blockKey[:])
	logger.Logger.Debug("Created encrypted cookie store", zap.Int("key_byte_length", len(keyBytes)))
	store.Options = &sessions.Options{
		Path:     "/",
		Domain:   cfg.CookieDomain,
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		Secure:   cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	logger.Logger.Debug("Cookie options configured",
		zap.String("domain", cfg.CookieDomain), zap.Bool("secure", cfg.CookieSecure),
		zap.Bool("http_only", true), zap.Int("max_age", 86400*7))

	// Setup OAuth config
	var oauthCfg *oauth2.Config
	logger.Logger.Debug("Setting up OAuth provider", zap.String("provider", cfg.OAuth.Provider))
	logger.Logger.Debug("OAuth config",
		zap.String("client_id", cfg.OAuth.ClientID),
		zap.String("redirect_url", cfg.OAuth.RedirectURL), zap.Strings("scopes", cfg.OAuth.Scopes))

	switch strings.ToLower(cfg.OAuth.Provider) {
	case "google":
		oauthCfg = &oauth2.Config{
			ClientID:     cfg.OAuth.ClientID,
			ClientSecret: cfg.OAuth.ClientSecret,
			RedirectURL:  cfg.OAuth.RedirectURL,
			Scopes:       cfg.OAuth.Scopes,
			Endpoint:     google.Endpoint,
		}
		logger.Logger.Debug("Using Google OAuth endpoint")
	default:
		// Custom OAuth provider
		oauthCfg = &oauth2.Config{
			ClientID:     cfg.OAuth.ClientID,
			ClientSecret: cfg.OAuth.ClientSecret,
			RedirectURL:  cfg.OAuth.RedirectURL,
			Scopes:       cfg.OAuth.Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  cfg.OAuth.AuthURL,
				TokenURL: cfg.OAuth.TokenURL,
			},
		}
		logger.Logger.Debug("Using custom OAuth endpoint",
			zap.String("auth_url", cfg.OAuth.AuthURL), zap.String("token_url", cfg.OAuth.TokenURL))
	}

	logger.Logger.Info("Authentication enabled", zap.String("provider", cfg.OAuth.Provider))

	return &AuthManager{
		config:       cfg,
		oauthConfig:  oauthCfg,
		sessionStore: store,
		enabled:      true,
		apiKeys:      apiKeys,
	}, nil
}

// IsEnabled returns whether authentication is enabled
func (am *AuthManager) IsEnabled() bool {
	return am.enabled
}

// RequireAuth is middleware that requires authentication
func (am *AuthManager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Logger.Debug("RequireAuth middleware called", zap.String("method", r.Method), zap.String("path", r.URL.Path))

		if !am.enabled {
			// Authentication disabled, proceed without checks
			logger.Logger.Debug("Authentication disabled, allowing request")
			next.ServeHTTP(w, r)
			return
		}

		// Check for API key authentication (Bearer token)
		if user := am.authenticateAPIKey(r); user != nil {
			ctx := context.WithValue(r.Context(), UserKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Check if user is authenticated via session/cookie
		user := am.GetCurrentUser(r)
		if user == nil {
			// Not authenticated, redirect to login
			logger.Logger.Debug("No authenticated user found for request")
			if isAPIRequest(r) {
				logger.Logger.Debug("API request, returning 401")
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}

			// Redirect to login for web requests
			logger.Logger.Debug("Web request, redirecting to login")
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		logger.Logger.Debug("Authenticated user found", zap.String("name", user.Name), zap.String("email", user.Email))

		// Check if user has any permissions (whitelist check)
		if len(user.Permissions) == 0 {
			// User is authenticated but not authorized (not in whitelist)
			logger.Logger.Warn("Access denied for non-whitelisted user", zap.String("email", user.Email))
			if isAPIRequest(r) {
				http.Error(w, "Access denied - user not authorized", http.StatusForbidden)
				return
			}

			// For web requests, show access denied page. The email is
			// attacker-influenced (a permissive OIDC provider need not constrain
			// the local-part), so it MUST be HTML-escaped before being written
			// into the page to avoid a reflected/stored XSS sink. Set the
			// Content-Type BEFORE WriteHeader, otherwise the header is dropped.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			if _, err := w.Write([]byte(`
				<!DOCTYPE html>
				<html>
				<head><title>Access Denied</title></head>
				<body>
					<h1>Access Denied</h1>
					<p>Your account (` + html.EscapeString(user.Email) + `) is not authorized to access this system.</p>
					<p>Please contact your administrator to request access.</p>
					<a href="/auth/logout">Logout</a>
				</body>
				</html>
			`)); err != nil {
				logger.Logger.Error("Failed to write access denied page", zap.Error(err))
			}
			return
		}

		// Add user to request context
		ctx := context.WithValue(r.Context(), UserKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetCurrentUser returns the current authenticated user from the session
func (am *AuthManager) GetCurrentUser(r *http.Request) *User {
	logger.Logger.Debug("GetCurrentUser called", zap.String("path", r.URL.Path))

	if !am.enabled {
		logger.Logger.Debug("Authentication is disabled, returning nil user")
		return nil
	}

	logger.Logger.Debug("Getting session from request", zap.String("session_name", SessionName))
	session, err := am.sessionStore.Get(r, SessionName)
	if err != nil {
		logger.Logger.Debug("Failed to get session", zap.Error(err),
			zap.Bool("has_cookie", r.Header.Get("Cookie") != ""))

		// If the session is invalid (likely due to key change), we should return nil
		// and let the caller handle it (redirect to login, etc.)
		logger.Logger.Debug("Session is invalid, likely due to session key change")
		return nil
	}
	logger.Logger.Debug("Session retrieved successfully", zap.Bool("is_new", session.IsNew))

	userInterface, ok := session.Values[sessionUserKey]
	if !ok {
		logger.Logger.Debug("No user key found in session values")
		return nil
	}
	logger.Logger.Debug("User data found in session")

	userJSON, ok := userInterface.(string)
	if !ok {
		logger.Logger.Debug("User data in session is not a string", zap.String("type", fmt.Sprintf("%T", userInterface)))
		return nil
	}
	logger.Logger.Debug("User JSON retrieved", zap.Int("length", len(userJSON)))

	var user User
	if err := json.Unmarshal([]byte(userJSON), &user); err != nil {
		logger.Logger.Debug("Failed to unmarshal user from session", zap.Error(err))
		return nil
	}
	logger.Logger.Debug("Successfully retrieved user from session",
		zap.String("name", user.Name), zap.String("email", user.Email))

	return &user
}

// HandleLogin handles the OAuth login initiation
func (am *AuthManager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	logger.Logger.Debug("HandleLogin called", zap.String("path", r.URL.Path))
	logger.Logger.Debug("Request headers",
		zap.String("user_agent", r.Header.Get("User-Agent")),
		zap.Bool("has_cookie", r.Header.Get("Cookie") != ""))

	if !am.enabled {
		logger.Logger.Error("Authentication is disabled but login handler was called")
		http.Error(w, "Authentication is disabled", http.StatusServiceUnavailable)
		return
	}

	// Generate state parameter for CSRF protection
	state, err := generateRandomString(32)
	if err != nil {
		logger.Logger.Error("Failed to generate random string for state", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	logger.Logger.Debug("Generated OAuth state", zap.Int("state_length", len(state)))

	logger.Logger.Debug("Attempting to get session for login")
	session, err := am.sessionStore.Get(r, SessionName)
	if err != nil {
		logger.Logger.Warn("Failed to get existing session (likely due to key change)", zap.Error(err))
		logger.Logger.Debug("Session error details", zap.Bool("has_cookie", r.Header.Get("Cookie") != ""))

		// Create a new session if the old one is invalid
		logger.Logger.Debug("Creating new session due to invalid existing session")
		session, err = am.sessionStore.New(r, SessionName)
		if err != nil {
			logger.Logger.Error("Failed to create new session", zap.Error(err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		logger.Logger.Debug("Successfully created new session")
	}
	logger.Logger.Debug("Session obtained for login", zap.Bool("is_new", session.IsNew))

	session.Values[StateKey] = state
	logger.Logger.Debug("Saving session with state")
	if err := session.Save(r, w); err != nil {
		logger.Logger.Error("Failed to save session", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	logger.Logger.Debug("Session saved successfully")

	// Redirect to OAuth provider. Do not log authURL: it embeds the `state`
	// CSRF nonce in its query string, which must never be written to logs.
	authURL := am.oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)
	logger.Logger.Debug("Redirecting to OAuth provider")
	http.Redirect(w, r, authURL, http.StatusFound)
}

// HandleCallback handles the OAuth callback
func (am *AuthManager) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if !am.enabled {
		http.Error(w, "Authentication is disabled", http.StatusServiceUnavailable)
		return
	}

	// Get session
	session, err := am.sessionStore.Get(r, SessionName)
	if err != nil {
		logger.Logger.Error("Failed to get session", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Verify state parameter
	expectedState, ok := session.Values[StateKey].(string)
	if !ok || expectedState == "" {
		logger.Logger.Error("No state found in session")
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	actualState := r.URL.Query().Get("state")
	if actualState != expectedState {
		logger.Logger.Error("State mismatch",
			zap.Int("expected_length", len(expectedState)), zap.Int("actual_length", len(actualState)))
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	// Exchange code for token
	code := r.URL.Query().Get("code")
	if code == "" {
		logger.Logger.Error("No code in callback")
		http.Error(w, "No authorization code", http.StatusBadRequest)
		return
	}

	token, err := am.oauthConfig.Exchange(r.Context(), code)
	if err != nil {
		logger.Logger.Error("Failed to exchange code for token", zap.Error(err))
		http.Error(w, "Failed to exchange code", http.StatusInternalServerError)
		return
	}

	// Resolve the user identity. Prefer verifying the openid ID token returned by
	// the exchange: its claims (email_verified, hd) are signed by the IdP and
	// cannot be forged, unlike the userinfo endpoint response which is only as
	// trustworthy as the access token. Fall back to userinfo only when no ID
	// token is available (non-openid or custom providers).
	var user *User
	if rawIDToken, ok := token.Extra("id_token").(string); ok && rawIDToken != "" && am.isGoogleProvider() {
		user, err = am.userFromIDToken(r.Context(), rawIDToken)
		if err != nil {
			logger.Logger.Error("Failed to verify ID token", zap.Error(err))
			http.Error(w, "Failed to verify identity", http.StatusUnauthorized)
			return
		}
	} else {
		user, err = am.getUserInfo(r.Context(), token)
		if err != nil {
			logger.Logger.Error("Failed to get user info", zap.Error(err))
			http.Error(w, "Failed to get user info", http.StatusInternalServerError)
			return
		}
	}

	// Assign roles and permissions based on user attributes
	am.assignUserPermissions(user)

	// Store user in session
	userJSON, err := json.Marshal(user)
	if err != nil {
		logger.Logger.Error("Failed to marshal user", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	session.Values[sessionUserKey] = string(userJSON)
	delete(session.Values, StateKey) // Clean up state

	if err := session.Save(r, w); err != nil {
		logger.Logger.Error("Failed to save session", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	logger.Logger.Info("User authenticated", zap.String("name", user.Name), zap.String("email", user.Email))

	// Redirect to home page
	http.Redirect(w, r, "/", http.StatusFound)
}

// HandleLogout handles user logout
func (am *AuthManager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if !am.enabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	session, err := am.sessionStore.Get(r, SessionName)
	if err != nil {
		logger.Logger.Error("Failed to get session", zap.Error(err))
	} else {
		// Clear session
		session.Values = make(map[interface{}]interface{})
		session.Options.MaxAge = -1
		if err := session.Save(r, w); err != nil {
			logger.Logger.Error("Failed to save session during logout", zap.Error(err))
		}
	}

	// Redirect to home page
	http.Redirect(w, r, "/", http.StatusFound)
}

// getUserInfo fetches user information from the OAuth provider
func (am *AuthManager) getUserInfo(ctx context.Context, token *oauth2.Token) (*User, error) {
	client := am.oauthConfig.Client(ctx, token)

	userInfoURL := am.config.OAuth.UserInfoURL
	if userInfoURL == "" {
		// Default to Google's userinfo endpoint
		userInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
	}

	resp, err := client.Get(userInfoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get user info: status %d", resp.StatusCode)
	}

	// Bound the read: a hostile or misconfigured IdP must not be able to make us
	// buffer an unbounded userinfo response. 1 MiB is generous for a set of user
	// claims. Mirrors the LimitReader/MaxBytesReader guards in the trace worker.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read user info: %w", err)
	}

	return am.parseUserInfo(body)
}

// isGoogleProvider reports whether the configured OAuth provider is Google, which
// is the only provider whose ID tokens we verify with the idtoken package.
func (am *AuthManager) isGoogleProvider() bool {
	return strings.EqualFold(am.config.OAuth.Provider, "google")
}

// isGoogleIssuer reports whether iss is one of Google's accepted ID token issuer
// values.
func isGoogleIssuer(iss string) bool {
	return iss == "accounts.google.com" || iss == "https://accounts.google.com"
}

// userFromIDToken verifies a Google openid ID token and builds a User from its
// signed claims. idtoken.Validate pins the signature (via Google's JWKS), the
// audience (our OAuth client ID) and the expiry; it does not constrain the
// issuer, so we assert `iss` explicitly here.
func (am *AuthManager) userFromIDToken(ctx context.Context, rawIDToken string) (*User, error) {
	payload, err := idtoken.Validate(ctx, rawIDToken, am.oauthConfig.ClientID)
	if err != nil {
		return nil, fmt.Errorf("failed to validate ID token: %w", err)
	}
	if !isGoogleIssuer(payload.Issuer) {
		return nil, fmt.Errorf("unexpected ID token issuer %q", payload.Issuer)
	}

	user := &User{
		ID:       payload.Subject,
		Email:    claimString(payload.Claims, "email"),
		Name:     claimString(payload.Claims, "name"),
		Picture:  claimString(payload.Claims, "picture"),
		Verified: claimBool(payload.Claims, "email_verified"),
		HD:       claimString(payload.Claims, "hd"),
	}
	logger.Logger.Debug("Resolved user from verified ID token",
		zap.String("email", user.Email), zap.Bool("email_verified", user.Verified),
		zap.Bool("has_hd", user.HD != ""))
	return user, nil
}

// claimString reads a string claim, returning "" when absent or of another type.
func claimString(claims map[string]interface{}, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

// claimBool reads a boolean claim. Google encodes email_verified as a bool, but
// some IdPs use the string "true"; both forms are accepted.
func claimBool(claims map[string]interface{}, key string) bool {
	switch v := claims[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}

// generateRandomString generates a random string of the specified length
func generateRandomString(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		// In case of error, return empty string - calling code should handle appropriately
		return "", fmt.Errorf("failed to generate random string: %w", err)
	}
	return base64.URLEncoding.EncodeToString(bytes)[:length], nil
}

// isAPIRequest reports whether the request targets the JSON API and should
// therefore receive a 401 rather than a browser login redirect. The decision is
// based solely on the "/api/" path prefix: a browser that merely sends
// "Accept: application/json" (or an equivalent Content-Type) must still be
// redirected to the login page rather than getting a bare 401.
func isAPIRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/")
}

// GetUserFromContext extracts the user from the request context
func GetUserFromContext(ctx context.Context) *User {
	user, ok := ctx.Value(UserKey).(*User)
	if !ok {
		return nil
	}
	return user
}

// Permission checking methods
func (u *User) HasPermission(permission string) bool {
	if u == nil || u.Permissions == nil {
		return false
	}
	for _, p := range u.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

// RequirePermission is middleware that requires a specific permission
func (am *AuthManager) RequirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return am.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// When auth is disabled, RequireAuth passes through without a user in
			// context; skip permission enforcement so that path stays consistent.
			if !am.enabled {
				next.ServeHTTP(w, r)
				return
			}
			user := GetUserFromContext(r.Context())
			if user == nil || !user.HasPermission(permission) {
				if isAPIRequest(r) {
					http.Error(w, "Insufficient permissions", http.StatusForbidden)
					return
				}
				http.Error(w, "Access denied", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// assignUserPermissions assigns roles and permissions to a user based on configuration
func (am *AuthManager) assignUserPermissions(user *User) {
	if !am.enabled {
		return
	}

	// Never whitelist an unverified email. Authorization is email/domain keyed,
	// so an unverified (self-asserted) email must not grant any access.
	if !user.Verified {
		user.Permissions = []string{}
		user.Roles = []string{}
		logger.Logger.Warn("Access denied for user with unverified email", zap.String("email", user.Email))
		return
	}

	// First, check if user is allowed access at all (whitelist check)
	if !am.isUserAllowed(user) {
		// User is not in the whitelist - deny access
		user.Permissions = []string{}
		user.Roles = []string{}
		logger.Logger.Warn("Access denied for user not in whitelist", zap.String("email", user.Email))
		return
	}

	// User is whitelisted - assign the permissions the routes actually require.
	// The taxonomy is exactly what RequirePermission is mounted with; granting
	// anything else (write:config, admin:all, manage:users used to be handed to
	// admins here) implies a boundary that does not exist, since no route
	// consults them.
	user.Permissions = []string{"read:memory", "chat:query"}
	user.Roles = []string{"user"}

	// Check if user is also an admin (admins must also be in whitelist). The
	// admin role carries no extra permissions today: add one here only alongside
	// the route that requires it.
	if am.isUserAdmin(user) {
		user.Roles = append(user.Roles, "admin")
		logger.Logger.Info("Granted admin role to whitelisted user", zap.String("email", user.Email))
		return
	}

	logger.Logger.Info("Granted standard user permissions to whitelisted user", zap.String("email", user.Email))
}

// isUserAllowed checks if a user is in the allowed whitelist. Domain matching is
// authoritative-claim based: for Google the verified `hd` claim must equal the
// allowed domain, while non-Google providers fall back to an email-suffix match.
func (am *AuthManager) isUserAllowed(user *User) bool {
	// If no whitelist restrictions are configured, allow all authenticated users.
	//
	// All FOUR lists must be empty, not just the two Allowed* ones. Consulting
	// only those made an admin-only deployment (admin_emails set, allowed_* not)
	// short-circuit to allow-all before ever reaching isUserAdmin below, so it
	// admitted every account that completed the OAuth flow while looking
	// correctly configured. The condition now matches NewAuthManager's
	// fail-closed guard exactly, which also makes this branch provably dead
	// whenever auth is enabled — the constructor rejects all-four-empty.
	if len(am.config.AllowedEmails) == 0 && len(am.config.AllowedDomains) == 0 &&
		len(am.config.AdminEmails) == 0 && len(am.config.AdminDomains) == 0 {
		logger.Logger.Debug("No whitelist restrictions configured, allowing authenticated user",
			zap.String("email", user.Email))
		return true
	}

	// Check allowed emails
	for _, allowedEmail := range am.config.AllowedEmails {
		if user.Email == allowedEmail {
			return true
		}
	}

	// Check allowed domains
	for _, domain := range am.config.AllowedDomains {
		if am.domainMatches(user, domain) {
			return true
		}
	}

	// Also check admin lists (admins are automatically allowed)
	return am.isUserAdmin(user)
}

// domainMatches reports whether the user belongs to the given domain. For Google
// this requires the signed `hd` (hosted-domain) claim to equal the domain, so an
// attacker-settable email suffix cannot satisfy a domain whitelist. Other
// providers, which do not emit `hd`, fall back to matching the email suffix.
func (am *AuthManager) domainMatches(user *User, domain string) bool {
	if am.isGoogleProvider() {
		return user.HD != "" && strings.EqualFold(user.HD, domain)
	}
	return strings.HasSuffix(user.Email, "@"+domain)
}

// authenticateAPIKey checks for a valid Bearer token in the Authorization
// header. Presented tokens are matched by SHA-256 digest using a constant-time
// comparison, never by plaintext equality. Every key is a named identity
// carrying its own scope, so callers (e.g. the Discord bot vs. an MCP client)
// are individually attributable and revocable.
func (am *AuthManager) authenticateAPIKey(r *http.Request) *User {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" {
		return nil
	}
	presented := sha256.Sum256([]byte(token))

	// Scoped, named identities, hashes already decoded by newAPIKeyTable. The
	// scan is by index and against the stored array so no per-request allocation
	// happens; ConstantTimeCompare still runs in full for every identity, and the
	// only early exit remains the one on a match.
	for i := range am.apiKeys {
		identity := &am.apiKeys[i]
		if subtle.ConstantTimeCompare(presented[:], identity.hash[:]) == 1 {
			// Check-then-Write for the same reason as the miss path below.
			if ce := logger.Logger.Check(zapcore.DebugLevel, "Authenticated via named API key"); ce != nil {
				ce.Write(
					zap.String("key_name", identity.name),
					zap.String("key_fingerprint", keyFingerprint(presented[:])))
			}
			return &User{
				ID:          "api-key:" + identity.name,
				Email:       identity.name + "@api-key.accretion",
				Name:        identity.name,
				Roles:       identity.roles,
				Permissions: identity.permissions,
				Verified:    true,
			}
		}
	}

	// Check-then-Write, not Logger.Debug: keyFingerprint hex-encodes on every
	// request when passed as a plain argument, and the default level is Info, so
	// in production that allocation is pure waste. The check is level-only and
	// runs after the scan, so it adds no data-dependent branching.
	if ce := logger.Logger.Check(zapcore.DebugLevel, "Invalid API key provided"); ce != nil {
		ce.Write(zap.String("key_fingerprint", keyFingerprint(presented[:])))
	}
	return nil
}

// keyFingerprint returns a short, non-reversible identifier for an API key from
// its SHA-256 digest, suitable for correlating log lines without revealing the
// key itself.
func keyFingerprint(digest []byte) string {
	return hex.EncodeToString(digest[:4])
}

// isUserAdmin checks if a user has admin privileges. Like isUserAllowed, domain
// matches rely on the signed `hd` claim for Google so admin grants cannot be
// obtained with a forged email suffix.
func (am *AuthManager) isUserAdmin(user *User) bool {
	// Check admin emails
	for _, adminEmail := range am.config.AdminEmails {
		if user.Email == adminEmail {
			return true
		}
	}

	// Check admin domains
	for _, domain := range am.config.AdminDomains {
		if am.domainMatches(user, domain) {
			return true
		}
	}

	return false
}
