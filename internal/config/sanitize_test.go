package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The Sanitize methods are denylists: each one names the fields it masks, so a
// credential field added later is logged in plaintext by default rather than
// masked by default. That is how DiscordConfig.API.Key came to be written to
// the bot's pod log at INFO on every startup, sitting beside four other secrets
// that all showed *** correctly.
//
// These tests replace "remember to update Sanitize" with a check that fails on
// its own. Every string field whose NAME says it holds a credential is filled
// with a unique sentinel, the config is sanitized, and any sentinel that
// survives is reported by its full path. Adding a new secret field to any
// config struct fails this test until Sanitize covers it.

const sentinelPrefix = "SENTINEL-SECRET-"

// credentialField reports whether a field name denotes a secret VALUE that must
// never be logged.
//
// Substring matching on key/token/password/secret is deliberate -- it catches
// APIKey, SessionKey, ClientSecret, RedisPassword and a bare Key alike. But the
// bare substring alone is too wide, and measuring it rather than assuming
// proved that: it flagged API.TLS.KeyFile and Auth.OAuth.TokenURL, a filesystem
// path and a URL. Neither is a credential, and a check that cries wolf twice
// gets switched off, taking the real finding with it.
//
// So a locator suffix disqualifies: a name ending in File, URL, Path, Dir or
// Name points AT a secret rather than holding one. Only the value itself needs
// masking; the path to it is ordinary configuration and is more useful logged.
//
// Both sides of the suffix test are lowercased so this answers for snake_case
// YAML keys as well as Go field names: TestGenerateDefaultOmitsSecrets (#129)
// asks it about `token_url` and `key_file`, which a case-sensitive HasSuffix
// would call credentials. It changes no Go-field answer -- "TokenURL" and
// "token_url" both end in "url" -- so it widens what the helper can be asked
// about without moving the line for its original caller.
func credentialField(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{"file", "url", "uri", "path", "dir", "name"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	// "cred" reaches Credentials-style names that none of the other needles do.
	for _, needle := range []string{"key", "token", "password", "secret", "cred"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// fillSecrets walks v and sets every credential-named string field to a unique
// sentinel, returning the paths it touched. It returns the paths so a caller can
// assert it actually filled something -- a walk that sets nothing would make the
// survivor check below pass vacuously.
func fillSecrets(v reflect.Value, path string, filled *[]string) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		fillSecrets(v.Elem(), path, filled)
		return
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		name := t.Field(i).Name
		if !f.CanSet() {
			continue
		}
		full := name
		if path != "" {
			full = path + "." + name
		}
		switch f.Kind() {
		case reflect.String:
			if credentialField(name) {
				f.SetString(fmt.Sprintf("%s%s", sentinelPrefix, full))
				*filled = append(*filled, full)
			}
		case reflect.Struct, reflect.Pointer:
			fillSecrets(f, full, filled)
		}
	}
}

// findSentinels walks v and reports the path of every string still holding a
// sentinel.
func findSentinels(v reflect.Value, path string, found *[]string) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		findSentinels(v.Elem(), path, found)
		return
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		full := t.Field(i).Name
		if path != "" {
			full = path + "." + t.Field(i).Name
		}
		switch f.Kind() {
		case reflect.String:
			if strings.HasPrefix(f.String(), sentinelPrefix) {
				*found = append(*found, full)
			}
		case reflect.Struct, reflect.Pointer:
			findSentinels(f, full, found)
		}
	}
}

func TestSanitizeMasksEveryCredentialField(t *testing.T) {
	cases := []struct {
		name     string
		build    func() (fill reflect.Value, sanitize func() any)
		minCount int
	}{
		{
			name: "Config",
			build: func() (reflect.Value, func() any) {
				c := &Config{}
				return reflect.ValueOf(c), func() any { return c.Sanitize() }
			},
			minCount: 5,
		},
		{
			name: "DiscordConfig",
			build: func() (reflect.Value, func() any) {
				c := &DiscordConfig{}
				return reflect.ValueOf(c), func() any { return c.Sanitize() }
			},
			minCount: 5,
		},
		{
			name: "CollectorConfig",
			build: func() (reflect.Value, func() any) {
				c := &CollectorConfig{}
				return reflect.ValueOf(c), func() any { return c.Sanitize() }
			},
			minCount: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, sanitize := tc.build()

			var filled []string
			fillSecrets(target, "", &filled)

			// Positive control. If the walker filled nothing -- a renamed field,
			// a struct turned into a pointer, a reflection change -- then the
			// survivor check below would pass while proving nothing.
			if len(filled) < tc.minCount {
				t.Fatalf("filled only %d credential fields (%v); expected at least %d. "+
					"The walk is not reaching the fields, so a clean result here would be vacuous.",
					len(filled), filled, tc.minCount)
			}

			out := sanitize()

			var survived []string
			findSentinels(reflect.ValueOf(out), "", &survived)

			if len(survived) > 0 {
				t.Errorf("%s.Sanitize() left %d credential field(s) unmasked: %v\n"+
					"Every one of these is logged in plaintext wherever the sanitized config is "+
					"logged. Add them to Sanitize().", tc.name, len(survived), survived)
			}
		})
	}
}

// TestSanitizeCheckDetectsAnUnmaskedField proves the test above can fail. It
// sanitizes a config and then reintroduces a secret into the result, which is
// exactly the state an uncovered field produces -- if findSentinels does not
// report it, the test above cannot report a real one either.
func TestSanitizeCheckDetectsAnUnmaskedField(t *testing.T) {
	c := &DiscordConfig{}
	var filled []string
	fillSecrets(reflect.ValueOf(c), "", &filled)
	if len(filled) == 0 {
		t.Fatal("fillSecrets set nothing; the control cannot run")
	}

	out := c.Sanitize()
	var clean []string
	findSentinels(reflect.ValueOf(out), "", &clean)
	if len(clean) != 0 {
		t.Fatalf("expected a sanitized config to be clean before perturbation, got %v", clean)
	}

	// Reintroduce one secret, simulating a field Sanitize does not cover.
	out.API.Key = sentinelPrefix + "API.Key"
	var found []string
	findSentinels(reflect.ValueOf(out), "", &found)
	if len(found) != 1 || found[0] != "API.Key" {
		t.Fatalf("the detector missed a planted unmasked credential: got %v, want [API.Key]", found)
	}
}
