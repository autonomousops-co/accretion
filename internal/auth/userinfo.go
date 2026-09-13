package auth

import (
	"encoding/json"
	"fmt"
	"strings"
)

// flexBool is a bool that tolerates either a JSON boolean or a JSON string
// "true"/"false" on the wire. OIDC-compliant providers encode email_verified as
// a real boolean, but some IdPs send the quoted string form; both are accepted,
// mirroring the tolerance claimBool already gives the signed ID-token path.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	// Prefer a native JSON boolean.
	var asBool bool
	if err := json.Unmarshal(data, &asBool); err == nil {
		*b = flexBool(asBool)
		return nil
	}
	// Fall back to a quoted "true"/"false" string.
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		*b = flexBool(strings.EqualFold(asString, "true"))
		return nil
	}
	return fmt.Errorf("flexBool: cannot unmarshal %s as bool or string", data)
}

// googleUserInfo models Google's OAuth2 v2 userinfo response, which keys the
// subject as `id` and the verified flag as `verified_email`.
type googleUserInfo struct {
	ID            string   `json:"id"`
	Email         string   `json:"email"`
	VerifiedEmail flexBool `json:"verified_email"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
	HD            string   `json:"hd"`
}

func (g *googleUserInfo) toUser() *User {
	return &User{
		ID:       g.ID,
		Email:    g.Email,
		Name:     g.Name,
		Picture:  g.Picture,
		Verified: bool(g.VerifiedEmail),
		HD:       g.HD,
	}
}

// oidcUserInfo models the standard OIDC UserInfo response, which keys the
// subject as `sub` and the verified flag as `email_verified`.
type oidcUserInfo struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
}

func (o *oidcUserInfo) toUser() *User {
	return &User{
		ID:       o.Sub,
		Email:    o.Email,
		Name:     o.Name,
		Picture:  o.Picture,
		Verified: bool(o.EmailVerified),
	}
}

// parseUserInfo decodes a userinfo response body into a neutral User, selecting
// the provider-specific target struct by OAuth.Provider. The default is the
// standards-compliant OIDC shape (subject `sub`, verified flag `email_verified`),
// so any non-Google IdP is decoded correctly; only "google" opts into Google's
// OAuth2 v2 field names.
func (am *AuthManager) parseUserInfo(body []byte) (*User, error) {
	switch strings.ToLower(am.config.OAuth.Provider) {
	case "google":
		var info googleUserInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return nil, fmt.Errorf("failed to decode Google user info: %w", err)
		}
		return info.toUser(), nil
	default:
		var info oidcUserInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return nil, fmt.Errorf("failed to decode OIDC user info: %w", err)
		}
		return info.toUser(), nil
	}
}
