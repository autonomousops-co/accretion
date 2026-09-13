package auth

import (
	"testing"

	"github.com/autonomousops-co/accretion/internal/config"
)

// authManagerForProvider builds a minimal AuthManager whose only configured
// field is the OAuth provider, which is all parseUserInfo dispatches on.
func authManagerForProvider(provider string) *AuthManager {
	return &AuthManager{
		config: &config.AuthConfig{
			OAuth: config.OAuthConfig{Provider: provider},
		},
	}
}

// TestParseUserInfo covers the userinfo-fallback decode path for both provider
// shapes. The regression that motivated #36: standards-compliant OIDC providers
// send `email_verified` (not Google's `verified_email`) and key the subject as
// `sub` (not `id`); decoding those into a Google-shaped struct left User.Verified
// false and User.ID empty, so every such user was denied at assignUserPermissions.
func TestParseUserInfo(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		body         string
		wantID       string
		wantEmail    string
		wantName     string
		wantPicture  string
		wantVerified bool
	}{
		{
			name:         "google verified_email true and id",
			provider:     "google",
			body:         `{"id":"google-123","email":"a@example.com","verified_email":true,"name":"Ada","picture":"https://pic/a","hd":"example.com"}`,
			wantID:       "google-123",
			wantEmail:    "a@example.com",
			wantName:     "Ada",
			wantPicture:  "https://pic/a",
			wantVerified: true,
		},
		{
			name:         "oidc email_verified true bool",
			provider:     "okta",
			body:         `{"sub":"oidc-1","email":"b@example.com","email_verified":true,"name":"Bob","picture":"https://pic/b"}`,
			wantID:       "oidc-1",
			wantEmail:    "b@example.com",
			wantName:     "Bob",
			wantPicture:  "https://pic/b",
			wantVerified: true,
		},
		{
			name:         "oidc email_verified true string",
			provider:     "keycloak",
			body:         `{"sub":"oidc-2","email":"c@example.com","email_verified":"true"}`,
			wantID:       "oidc-2",
			wantEmail:    "c@example.com",
			wantVerified: true,
		},
		{
			name:         "oidc email_verified false",
			provider:     "auth0",
			body:         `{"sub":"oidc-3","email":"d@example.com","email_verified":false}`,
			wantID:       "oidc-3",
			wantEmail:    "d@example.com",
			wantVerified: false,
		},
		{
			name:         "oidc email_verified false string",
			provider:     "dex",
			body:         `{"sub":"oidc-4","email":"e@example.com","email_verified":"false"}`,
			wantID:       "oidc-4",
			wantEmail:    "e@example.com",
			wantVerified: false,
		},
		{
			name:         "oidc email_verified absent defaults false",
			provider:     "azuread",
			body:         `{"sub":"oidc-5","email":"f@example.com"}`,
			wantID:       "oidc-5",
			wantEmail:    "f@example.com",
			wantVerified: false,
		},
		{
			name:         "empty provider defaults to oidc shape",
			provider:     "",
			body:         `{"sub":"oidc-6","email":"g@example.com","email_verified":true}`,
			wantID:       "oidc-6",
			wantEmail:    "g@example.com",
			wantVerified: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			am := authManagerForProvider(tc.provider)
			user, err := am.parseUserInfo([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseUserInfo returned error: %v", err)
			}
			if user.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", user.ID, tc.wantID)
			}
			if user.Email != tc.wantEmail {
				t.Errorf("Email = %q, want %q", user.Email, tc.wantEmail)
			}
			if user.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", user.Name, tc.wantName)
			}
			if user.Picture != tc.wantPicture {
				t.Errorf("Picture = %q, want %q", user.Picture, tc.wantPicture)
			}
			if user.Verified != tc.wantVerified {
				t.Errorf("Verified = %v, want %v", user.Verified, tc.wantVerified)
			}
		})
	}
}

// TestParseUserInfo_GoogleHD confirms the Google DTO carries the hosted-domain
// claim through to the neutral User.
func TestParseUserInfo_GoogleHD(t *testing.T) {
	am := authManagerForProvider("google")
	user, err := am.parseUserInfo([]byte(`{"id":"g","email":"h@example.com","verified_email":true,"hd":"example.com"}`))
	if err != nil {
		t.Fatalf("parseUserInfo returned error: %v", err)
	}
	if user.HD != "example.com" {
		t.Errorf("HD = %q, want %q", user.HD, "example.com")
	}
}

// TestParseUserInfo_InvalidJSON confirms a decode error surfaces rather than
// yielding a zero-value user.
func TestParseUserInfo_InvalidJSON(t *testing.T) {
	am := authManagerForProvider("okta")
	if _, err := am.parseUserInfo([]byte(`{not json`)); err == nil {
		t.Fatal("expected an error decoding malformed JSON, got nil")
	}
}
