package accretion

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is an HTTP client for the accretion-api REST endpoints.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// TLSOptions configures how this client verifies the API's certificate.
//
// The zero value uses the default transport, which verifies against the system
// pool — already correct for a publicly-trusted certificate. These options exist
// for the private CA the chart issues from.
type TLSOptions struct {
	// CAFile is a path to a PEM bundle to verify against.
	CAFile string
	// CAPEM is the same bundle inline. The MCP resolves its credentials from a
	// Kubernetes Secret (ACCRETION_MCP_SECRET_NAME, the form both .mcp.json and
	// opencode.json use), and it runs on a contributor's machine rather than in
	// the cluster — so the CA has to travel with the URL and the key rather than
	// being a path that only exists inside a pod. Carrying the PEM in that same
	// Secret is the decision: one credential fetch, nothing to place by hand.
	//
	// CAFile wins if both are set.
	CAPEM string
	// InsecureSkipVerify disables verification. It is REFUSED for any
	// non-loopback host — see NewClientWithTLS.
	InsecureSkipVerify bool
	// ReloadCA re-resolves the CA from wherever it originally came from. It is
	// called at most once per request, and only when a request failed against an
	// unknown authority.
	//
	// The MCP server is long-lived — a client keeps it running for a whole
	// session — while the CA is not: recreating the cluster reissues it, and
	// cert-manager renews on its own schedule. Without this the process keeps
	// verifying against a CA that no longer signs anything and only a restart
	// recovers, while the error names the certificate and sends you to debug the
	// wrong end. See #89.
	//
	// Optional. Nil means no reload, which is the right behavior for a caller
	// that has no way to fetch a fresh copy.
	ReloadCA func() (string, error)
}

// NewClientWithTLS creates a client that verifies the API's certificate against
// a specific CA, which is what reaching a privately-issued certificate requires.
//
// The skip-verify escape hatch is deliberately constrained. Unconstrained, it
// becomes the setting everyone copies out of a README, and the result is a stack
// that reports TLS everywhere and verifies nothing. So it is refused outright for
// a non-loopback host — where it would be a real downgrade over a real network —
// and logged loudly where it is allowed.
func NewClientWithTLS(baseURL, apiKey string, opts TLSOptions) (*Client, error) {
	tlsConfig, err := opts.tlsConfig(baseURL)
	if err != nil {
		return nil, err
	}

	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: clientTimeout,
		},
	}
	if tlsConfig != nil {
		// Clone the default transport rather than building a bare one. A
		// zero-value http.Transport silently discards every default: Proxy (so
		// HTTP_PROXY/HTTPS_PROXY stop working, and this process runs on
		// contributors' machines, some behind a corporate proxy), HTTP/2
		// negotiation, IdleConnTimeout, MaxIdleConns, ExpectContinueTimeout. That
		// degradation would apply only on the TLS path — which is now the
		// default — so it would look like "TLS made things worse".
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("http.DefaultTransport is %T, not *http.Transport", http.DefaultTransport)
		}
		transport := base.Clone()
		transport.TLSClientConfig = tlsConfig
		if opts.ReloadCA != nil {
			c.httpClient.Transport = &reloadingTransport{
				base:     transport,
				opts:     opts,
				baseURL:  baseURL,
				reloadCA: opts.ReloadCA,
			}
		} else {
			c.httpClient.Transport = transport
		}
	}
	return c, nil
}

// reloadingTransport retries a request once against a freshly resolved CA when
// the first attempt failed because the authority was unknown.
//
// It sits at the RoundTripper layer rather than in each method because the
// client has eight separate Do call sites and no shared helper; putting the
// retry here covers all of them and cannot be forgotten by a ninth.
type reloadingTransport struct {
	mu       sync.Mutex
	base     *http.Transport
	opts     TLSOptions
	baseURL  string
	reloadCA func() (string, error)
}

// isUnknownAuthority reports whether err is a verification failure caused by an
// untrusted signer — the signature of a rotated CA. Deliberately narrow: an
// expired certificate, a hostname mismatch, a refused connection or an HTTP
// error must not trigger a reload, because re-reading the CA cannot fix them and
// retrying would just double the work before failing the same way.
func isUnknownAuthority(err error) bool {
	if err == nil {
		return false
	}
	var unknown x509.UnknownAuthorityError
	return errors.As(err, &unknown)
}

func (t *reloadingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	base := t.base
	t.mu.Unlock()

	resp, err := base.RoundTrip(req)
	if !isUnknownAuthority(err) {
		return resp, err
	}

	// A request with a body has had it consumed by the attempt above. GetBody is
	// set by http.NewRequest for the body types this client uses; without a
	// rewind, replaying POST /api/chat would send an empty body and the retry
	// would "succeed" against the wrong request.
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return resp, err
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return resp, err
		}
		req = req.Clone(req.Context())
		req.Body = body
	}

	pem, reloadErr := t.reloadCA()
	if reloadErr != nil {
		// Return the ORIGINAL error: it describes what actually failed for the
		// caller, and a reload problem here is secondary noise.
		return resp, err
	}

	opts := t.opts
	opts.CAPEM = pem
	fresh, cfgErr := opts.tlsConfig(t.baseURL)
	if cfgErr != nil || fresh == nil {
		return resp, err
	}

	t.mu.Lock()
	next := t.base.Clone()
	next.TLSClientConfig = fresh
	t.base = next
	t.mu.Unlock()

	// Exactly one retry. If the refreshed CA still does not verify, the caller
	// gets that second failure, which is the accurate one.
	return next.RoundTrip(req)
}

// clientTimeout must cover /api/chat's non-streaming RAG flow, the same budget
// the server's write timeout allows for it.
const clientTimeout = 120 * time.Second

func (o TLSOptions) tlsConfig(baseURL string) (*tls.Config, error) {
	if o.InsecureSkipVerify {
		loopback, err := isLoopbackURL(baseURL)
		if err != nil {
			return nil, err
		}
		if !loopback {
			return nil, fmt.Errorf(
				"insecure_skip_verify is refused for %q: it is only permitted for a loopback host "+
					"(localhost or 127.0.0.1), where the traffic never crosses a network. "+
					"Supply the CA instead (ACCRETION_API_CA_FILE)", baseURL)
		}
		log.Printf("WARNING: TLS certificate verification is DISABLED for %s. "+
			"This is permitted only because the host is loopback. Supply the CA "+
			"(ACCRETION_API_CA_FILE) rather than relying on this.", baseURL)
		// #nosec G402 -- InsecureSkipVerify is reached only after isLoopbackURL
		// above has confirmed a literal loopback host, so the traffic never
		// leaves the machine, and the case is logged at WARNING. A //nolint
		// directive alone was not enough: `make security` runs gosec directly,
		// which reads #nosec and ignores golangci-lint's //nolint entirely.
		// MinVersion is set so this path matches every other tls.Config here.
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil
	}

	var pemBytes []byte
	switch {
	case o.CAFile != "":
		b, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file %q: %w", o.CAFile, err)
		}
		pemBytes = b
	case o.CAPEM != "":
		pemBytes = []byte(o.CAPEM)
	default:
		return nil, nil // system pool
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no PEM certificate found in the configured CA (CAFile=%q)", o.CAFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// isLoopbackURL reports whether the URL's host is the loopback interface. A
// hostname that merely resolves to 127.0.0.1 does not count: only the literal
// loopback names are accepted, so the check cannot be defeated by DNS.
//
// The case folding and trailing-dot trim are deliberate rather than incidental.
// Without them "LOCALHOST" and "localhost." are still refused — the safe
// direction — but they are refused by an exact comparison that happens to fail,
// while the error tells the user loopback hosts are permitted. Normalizing makes
// the safety a decision and the message true.
func isLoopbackURL(raw string) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, fmt.Errorf("parsing API URL %q: %w", raw, err)
	}
	// DNS names are case-insensitive, and a trailing dot denotes the same
	// fully-qualified name.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" {
		return true, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	return false, nil
}

type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

// ChatTurn is one prior message in a conversation, as supplied by a caller to
// ChatWithHistory. It is the exported mirror of chatMessage, which stays
// unexported because it is the wire shape; the two are field-identical so a
// turn converts directly.
type ChatTurn struct {
	Role    string
	Content string
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Content string `json:"content"`
}

// Chat sends a question to the accretion-api /api/chat endpoint and returns the response.
func (c *Client) Chat(ctx context.Context, question string) (string, error) {
	return c.ChatWithHistory(ctx, nil, question)
}

// ChatWithHistory is Chat with prior turns replayed ahead of the question, so a
// caller holding a conversation (the Discord bot) gets a follow-up resolved
// against what was already said. History is oldest first and may be nil.
func (c *Client) ChatWithHistory(
	ctx context.Context,
	history []ChatTurn,
	question string,
) (string, error) {
	messages := make([]chatMessage, 0, len(history)+1)
	for _, h := range history {
		messages = append(messages, chatMessage(h))
	}
	messages = append(messages, chatMessage{Role: "user", Content: question})

	body := chatRequest{Messages: messages}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("accretion-api returned %d", resp.StatusCode)
		}
		return "", fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Content, nil
}

type logsResponse struct {
	Logs []logEntry `json:"logs"`
}

type logEntry struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"pod_name"`
	Container string `json:"container"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// Logs retrieves recent log entries from the accretion-api /api/logs endpoint.
func (c *Client) Logs(ctx context.Context, namespace string, limit int) (string, error) {
	url := c.baseURL + "/api/logs"
	params := []string{}
	if namespace != "" {
		params = append(params, "namespace="+namespace)
	}
	if limit > 0 {
		params = append(params, "limit="+strconv.Itoa(limit))
	}
	if len(params) > 0 {
		url += "?" + strings.Join(params, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("accretion-api returned %d", resp.StatusCode)
		}
		return "", fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result logsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	var sb strings.Builder
	for _, entry := range result.Logs {
		fmt.Fprintf(&sb, "[%s] %s/%s (%s): %s\n",
			entry.Timestamp, entry.Namespace, entry.PodName, entry.Container, entry.Message)
	}
	if sb.Len() == 0 {
		return "No log entries found.", nil
	}
	return sb.String(), nil
}

// Resources queries the /api/resources endpoint with structured filters.
func (c *Client) Resources(ctx context.Context, params map[string]string) (string, error) {
	url := c.baseURL + "/api/resources"
	var parts []string
	for k, v := range params {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) > 0 {
		url += "?" + strings.Join(parts, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// Search queries the /api/search endpoint, returning the JSON array of matched
// entries (above the similarity threshold) as a string.
func (c *Client) Search(ctx context.Context, query string, limit int, start, end string) (string, error) {
	endpoint := c.baseURL + "/api/search"
	vals := url.Values{}
	if query != "" {
		vals.Set("q", query)
	}
	if limit > 0 {
		vals.Set("limit", strconv.Itoa(limit))
	}
	if start != "" {
		vals.Set("start_time", start)
	}
	if end != "" {
		vals.Set("end_time", end)
	}
	if encoded := vals.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// Stats queries the /api/stats endpoint.
func (c *Client) Stats(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/stats", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// Health checks if the accretion-api server is reachable.
func (c *Client) Health(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/health", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unhealthy: status %d, body: %s", resp.StatusCode, string(body))
	}

	return fmt.Sprintf("Accretion API is healthy (status %d): %s", resp.StatusCode, string(body)), nil
}

// SearchTraces queries the /api/traces endpoint with the given parameters.
func (c *Client) SearchTraces(
	ctx context.Context,
	service, operation, minDuration, maxDuration string,
	limit int, start, end string,
) (json.RawMessage, error) {
	endpoint := c.baseURL + "/api/traces"
	vals := url.Values{}
	if service != "" {
		vals.Set("service", service)
	}
	if operation != "" {
		vals.Set("operation", operation)
	}
	if minDuration != "" {
		vals.Set("minDuration", minDuration)
	}
	if maxDuration != "" {
		vals.Set("maxDuration", maxDuration)
	}
	if limit > 0 {
		vals.Set("limit", strconv.Itoa(limit))
	}
	if start != "" {
		vals.Set("start", start)
	}
	if end != "" {
		vals.Set("end", end)
	}
	if encoded := vals.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}

// GetTrace retrieves a single trace by ID from the /api/traces/{traceID} endpoint.
func (c *Client) GetTrace(ctx context.Context, traceID string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/traces/"+traceID, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accretion-api returned %d: %s", resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}
