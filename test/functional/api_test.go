//go:build functional

// Package functional holds black-box functional tests that run against a live
// accretion-api over HTTP. They are excluded from the default unit-test build
// via the "functional" build tag, so `go test ./...` and `make test` never hit
// the network.
//
// Run them against a running environment with:
//
//	make test-functional
//
// which port-forwards the in-cluster accretion-api Service and sets
// ACCRETION_API_URL. To point at any other running instance:
//
//	ACCRETION_API_URL=https://host:port [ACCRETION_API_KEY=<bearer>] \
//	  go test -tags functional -count=1 -v ./test/functional/...
//
// ACCRETION_API_CA_FILE points the client at the CA that signed the API's
// certificate, which a privately-issued certificate requires. The suite never
// skips verification.
//
// ACCRETION_API_KEY is optional; supply it when the target enforces auth. It
// must be a key scoped for read:memory AND chat:query, since the suite drives
// /api/chat. ACCRETION_API_KEY_READONLY additionally enables TestAPIKeyScoping,
// which proves a read-only identity is actually denied /api/chat.
package functional

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/config"
)

// requestTimeout mirrors the server's write timeout so the client waits exactly as
// long as the server may take on /api/chat's embedding -> vector search -> LLM path.
// Same source as the server: env ACCRETION_API_WRITE_TIMEOUT, else the shared
// config.DefaultWriteTimeout — so the client and server timeouts always agree.
func requestTimeout() time.Duration {
	if v := os.Getenv("ACCRETION_API_WRITE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return config.DefaultWriteTimeout
}

// The functional client verifies the API's certificate like any other client.
// ACCRETION_API_CA_FILE points it at the private CA the chart issues from;
// without it the system pool applies, which is right for a publicly-trusted
// certificate and for the cleartext case. There is no skip-verify here on
// purpose: a suite that skipped verification would pass against a deployment no
// real client could reach.
var client = newFunctionalClient()

func newFunctionalClient() *http.Client {
	c := &http.Client{Timeout: requestTimeout()}

	caFile := os.Getenv("ACCRETION_API_CA_FILE")
	if caFile == "" {
		return c
	}
	// Same loader the server-side config uses, so "what counts as a usable CA
	// file" has one answer rather than one per client.
	pool, err := config.CertPoolFromFile(caFile)
	if err != nil {
		panic("ACCRETION_API_CA_FILE is unusable: " + err.Error())
	}
	c.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	return c
}

func baseURL() string {
	if u := os.Getenv("ACCRETION_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:18080"
}

// newRequest builds a request against the target, attaching a bearer token when
// ACCRETION_API_KEY is set (needed once the target enforces authentication).
func newRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	return newRequestWithKey(t, method, path, body, os.Getenv("ACCRETION_API_KEY"))
}

// newRequestWithKey builds a request presenting a specific bearer token, so a
// test can exercise one identity's scope rather than the suite-wide key.
func newRequestWithKey(t *testing.T, method, path string, body io.Reader, key string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, baseURL()+path, body)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: request failed (is the environment up? try `make up`): %v",
			req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", req.Method, req.URL.Path, err)
	}
	return resp, body
}

func truncate(b []byte) string {
	const n = 300
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

func TestHealth(t *testing.T) {
	resp, body := do(t, newRequest(t, http.MethodGet, "/api/health", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: got %d, want 200 (body: %s)", resp.StatusCode, truncate(body))
	}
}

// TestSearchReturnsJSONArray exercises the semantic-search endpoint end to end:
// it embeds the query and returns matched rows from the live store.
func TestSearchReturnsJSONArray(t *testing.T) {
	resp, body := do(t, newRequest(t, http.MethodGet, "/api/search?q=error&limit=3", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: got %d, want 200 (body: %s)", resp.StatusCode, truncate(body))
	}
	var entries []map[string]any
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("search: response is not a JSON array: %v (body: %s)", err, truncate(body))
	}
}

func TestReadEndpointsReturn200(t *testing.T) {
	for _, path := range []string{"/api/logs?limit=5", "/api/resources?limit=5", "/api/stats"} {
		t.Run(path, func(t *testing.T) {
			resp, body := do(t, newRequest(t, http.MethodGet, path, nil))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: got %d, want 200 (body: %s)", path, resp.StatusCode, truncate(body))
			}
		})
	}
}

// TestChatRAGRoundTrip drives the full retrieval-augmented chat path and asserts
// a non-empty answer comes back.
func TestChatRAGRoundTrip(t *testing.T) {
	payload := `{"messages":[{"role":"user","content":"Summarize any errors in the accretion-db namespace."}]}`
	resp, body := do(t, newRequest(t, http.MethodPost, "/api/chat", bytes.NewBufferString(payload)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: got %d, want 200 (body: %s)", resp.StatusCode, truncate(body))
	}
	var out struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("chat: response is not valid JSON: %v (body: %s)", err, truncate(body))
	}
	if strings.TrimSpace(out.Content) == "" {
		t.Fatalf("chat: empty content in response (body: %s)", truncate(body))
	}
}

// TestChatRejectsEmptyMessages pins the input-validation contract: an empty
// messages array must be a 400, not a 500 or a silent success.
func TestChatRejectsEmptyMessages(t *testing.T) {
	resp, body := do(t, newRequest(t, http.MethodPost, "/api/chat", bytes.NewBufferString(`{"messages":[]}`)))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat (empty messages): got %d, want 400 (body: %s)", resp.StatusCode, truncate(body))
	}
}

// TestTracesEnabledOrGracefullyDisabled accepts either a 200 (Tempo wired) or a
// 503 (Tempo integration disabled) — both are correct; anything else is a bug.
func TestTracesEnabledOrGracefullyDisabled(t *testing.T) {
	resp, body := do(t, newRequest(t, http.MethodGet, "/api/traces?limit=1", nil))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("traces: got %d, want 200 or 503 (body: %s)", resp.StatusCode, truncate(body))
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	resp, body := do(t, newRequest(t, http.MethodGet, "/api/nope", nil))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route: got %d, want 404 (body: %s)", resp.StatusCode, truncate(body))
	}
}

// TestAPIKeyScoping covers issue #42: per-client identities are only worth
// having if a narrower key is actually denied. A read-only identity must reach
// /api/stats and be refused /api/chat — the endpoint that spends LLM tokens on
// every call. That the chat-scoped key reaches both is covered by the rest of
// the suite, which runs under ACCRETION_API_KEY.
func TestAPIKeyScoping(t *testing.T) {
	readOnly := os.Getenv("ACCRETION_API_KEY_READONLY")
	if readOnly == "" {
		t.Skip("set ACCRETION_API_KEY_READONLY to exercise per-identity scoping (make test-functional does)")
	}

	t.Run("read-only key reaches /api/stats", func(t *testing.T) {
		resp, body := do(t, newRequestWithKey(t, http.MethodGet, "/api/stats", nil, readOnly))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stats with read-only key: got %d, want 200 (body: %s)", resp.StatusCode, truncate(body))
		}
	})

	// The 403 is asserted before the LLM is ever reached, so this costs no
	// provider tokens. The chat-scoped key's own 200s are already covered by
	// TestReadEndpointsReturn200 and TestChatRAGRoundTrip, which run under
	// ACCRETION_API_KEY — re-running the full embedding -> search -> LLM round
	// trip here would add tokens and no coverage.
	t.Run("read-only key is denied /api/chat", func(t *testing.T) {
		body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"ping"}]}`)
		resp, respBody := do(t, newRequestWithKey(t, http.MethodPost, "/api/chat", body, readOnly))
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("chat with read-only key: got %d, want 403 (body: %s)", resp.StatusCode, truncate(respBody))
		}
	})
}
