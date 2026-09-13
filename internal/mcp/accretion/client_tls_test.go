package accretion

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tlsAPIServer stands in for accretion-api behind a privately-issued
// certificate, and returns its URL alongside the PEM that signed it.
func tlsAPIServer(t *testing.T) (baseURL string, caPEM []byte) {
	t.Helper()

	certPEM, keyPEM := loopbackCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	srv := httptest.NewUnstartedServer(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.Listener.Close()
	srv.Listener = ln
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv.URL, certPEM
}

func loopbackCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "accretion-api"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// reach exercises a real request so the assertion is about the transport the
// client actually uses, not about the shape of a tls.Config.
func reach(t *testing.T, c *Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/stats", http.NoBody)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func TestClientVerifiesAgainstCAFile(t *testing.T) {
	baseURL, caPEM := tlsAPIServer(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("writing CA: %v", err)
	}

	c, err := NewClientWithTLS(baseURL, "key", TLSOptions{CAFile: caFile})
	if err != nil {
		t.Fatalf("NewClientWithTLS: %v", err)
	}
	if err := reach(t, c); err != nil {
		t.Fatalf("request failed against the CA that signed the certificate: %v", err)
	}
}

// The MCP receives its CA inline from the Kubernetes Secret rather than as a
// path, so the inline form has to work identically.
func TestClientVerifiesAgainstInlineCA(t *testing.T) {
	baseURL, caPEM := tlsAPIServer(t)

	c, err := NewClientWithTLS(baseURL, "key", TLSOptions{CAPEM: string(caPEM)})
	if err != nil {
		t.Fatalf("NewClientWithTLS: %v", err)
	}
	if err := reach(t, c); err != nil {
		t.Fatalf("request failed with an inline CA: %v", err)
	}
}

// The negative case that makes the positive one mean something: a certificate
// the client was not given a CA for must be REJECTED.
func TestClientRejectsUntrustedCertificate(t *testing.T) {
	baseURL, _ := tlsAPIServer(t)
	_, otherCA := tlsAPIServer(t) // a CA that signed something else

	t.Run("system pool only", func(t *testing.T) {
		c, err := NewClientWithTLS(baseURL, "key", TLSOptions{})
		if err != nil {
			t.Fatalf("NewClientWithTLS: %v", err)
		}
		if err := reach(t, c); err == nil {
			t.Fatal("client accepted a privately-issued certificate against the system pool")
		}
	})

	t.Run("wrong CA", func(t *testing.T) {
		c, err := NewClientWithTLS(baseURL, "key", TLSOptions{CAPEM: string(otherCA)})
		if err != nil {
			t.Fatalf("NewClientWithTLS: %v", err)
		}
		if err := reach(t, c); err == nil {
			t.Fatal("client accepted a certificate signed by a different CA")
		}
	})
}

func TestClientRejectsUnusableCA(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := NewClientWithTLS("https://api.example.com", "key",
			TLSOptions{CAFile: filepath.Join(t.TempDir(), "absent.crt")})
		if err == nil {
			t.Fatal("expected an error for a CA file that does not exist")
		}
	})

	t.Run("not a certificate", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.crt")
		if err := os.WriteFile(bad, []byte("nope"), 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		_, err := NewClientWithTLS("https://api.example.com", "key", TLSOptions{CAFile: bad})
		if err == nil {
			t.Fatal("expected an error for a file containing no PEM certificate")
		}
	})
}

// The escape hatch is the main risk in this change. Unconstrained, skip-verify
// becomes the setting everyone copies out of a README and the stack reports TLS
// everywhere while verifying nothing. It must be refused for any host where it
// would be a real downgrade.
func TestSkipVerifyRefusedForNonLoopback(t *testing.T) {
	refused := []string{
		"https://api.example.com",
		"https://accretion-api.accretion.svc.cluster.local:41337",
		"https://10.0.0.5:41337",
		"https://[2001:db8::1]:41337",
	}
	for _, u := range refused {
		t.Run(u, func(t *testing.T) {
			_, err := NewClientWithTLS(u, "key", TLSOptions{InsecureSkipVerify: true})
			if err == nil {
				t.Fatalf("insecure_skip_verify was accepted for the non-loopback host %s", u)
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Fatalf("error %q does not say the option was refused", err)
			}
		})
	}
}

func TestSkipVerifyAllowedForLoopback(t *testing.T) {
	allowed := []string{
		"https://localhost:41337",
		"https://127.0.0.1:41337",
		"https://[::1]:41337",
	}
	for _, u := range allowed {
		t.Run(u, func(t *testing.T) {
			if _, err := NewClientWithTLS(u, "key", TLSOptions{InsecureSkipVerify: true}); err != nil {
				t.Fatalf("insecure_skip_verify refused for the loopback host %s: %v", u, err)
			}
		})
	}
}

// Case and a trailing dot name the same host, so they get the same answer. The
// point is not to widen the hatch — these were already refused — but to make the
// refusal a decision rather than a side effect of an exact string compare, so
// the error message ("permitted for a loopback host") is not a lie to someone
// who typed LOCALHOST.
func TestLoopbackCheckNormalizesHost(t *testing.T) {
	for _, u := range []string{
		"https://LOCALHOST:41337",
		"https://LocalHost:41337",
		"https://localhost.:41337",
		"https://LOCALHOST.:41337",
	} {
		t.Run(u, func(t *testing.T) {
			if _, err := NewClientWithTLS(u, "key", TLSOptions{InsecureSkipVerify: true}); err != nil {
				t.Fatalf("insecure_skip_verify refused for the loopback host %s: %v", u, err)
			}
		})
	}

	// Normalizing must not widen anything: a non-loopback host is still refused
	// whatever its case.
	for _, u := range []string{
		"https://EVIL.example.com",
		"https://LOCALHOST.evil.example.com",
	} {
		t.Run(u, func(t *testing.T) {
			if _, err := NewClientWithTLS(u, "key", TLSOptions{InsecureSkipVerify: true}); err == nil {
				t.Fatalf("insecure_skip_verify accepted for the non-loopback host %s", u)
			}
		})
	}
}

// A hostname is never treated as loopback just because it might resolve there:
// only the literal loopback names qualify, so the check cannot be sidestepped
// with DNS.
func TestLoopbackCheckIgnoresDNS(t *testing.T) {
	_, err := NewClientWithTLS("https://localhost.evil.example.com", "key",
		TLSOptions{InsecureSkipVerify: true})
	if err == nil {
		t.Fatal("a hostname merely containing \"localhost\" was treated as loopback")
	}
}

// The TLS path must not silently downgrade the transport. A bare
// &http.Transport{} discards every http.DefaultTransport default — HTTP/2
// negotiation, Proxy (so HTTP_PROXY is ignored, and this process runs on
// contributors' machines), IdleConnTimeout, MaxIdleConns. That degradation would
// have applied only when TLS is on, which is now the default, so it would have
// looked like TLS made the client worse.
func TestTLSClientKeepsTransportDefaults(t *testing.T) {
	// A CA must be configured for the client to install a transport at all: with
	// no TLS options it leaves Transport nil and http.DefaultTransport applies,
	// which is already correct. The defaults are only at risk on the path that
	// replaces the transport.
	certPEM, _ := loopbackCert(t)
	c, err := NewClientWithTLS("https://localhost:41337", "key", TLSOptions{CAPEM: string(certPEM)})
	if err != nil {
		t.Fatalf("NewClientWithTLS: %v", err)
	}
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}

	if tr.Proxy == nil {
		t.Error("Proxy is nil: HTTP_PROXY/HTTPS_PROXY would be silently ignored")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 is false: the client would negotiate HTTP/1.1 only")
	}
	if tr.IdleConnTimeout != def.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want the default %v (0 means idle conns never expire)",
			tr.IdleConnTimeout, def.IdleConnTimeout)
	}
	if tr.MaxIdleConns != def.MaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want the default %d", tr.MaxIdleConns, def.MaxIdleConns)
	}
	if tr.ExpectContinueTimeout != def.ExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want the default %v",
			tr.ExpectContinueTimeout, def.ExpectContinueTimeout)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Error("the configured CA was lost while cloning the default transport")
	}
}

// With no TLS options the client installs no transport, so http.DefaultTransport
// applies untouched. Pinned so a future "always build a transport" refactor
// cannot quietly reintroduce the same downgrade on the cleartext path.
func TestClientWithoutTLSOptionsUsesDefaultTransport(t *testing.T) {
	c, err := NewClientWithTLS("http://localhost:41337", "key", TLSOptions{})
	if err != nil {
		t.Fatalf("NewClientWithTLS: %v", err)
	}
	if c.httpClient.Transport != nil {
		t.Errorf("Transport = %#v, want nil so http.DefaultTransport applies", c.httpClient.Transport)
	}
}

// End-to-end proof of the HTTP/2 half: against a real h2 server the client must
// negotiate HTTP/2, which a bare transport cannot do.
func TestTLSClientNegotiatesHTTP2(t *testing.T) {
	certPEM, keyPEM := loopbackCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	var gotProto string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.Listener.Close()
	srv.Listener = ln
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c, err := NewClientWithTLS(srv.URL, "key", TLSOptions{CAPEM: string(certPEM)})
	if err != nil {
		t.Fatalf("NewClientWithTLS: %v", err)
	}
	if err := reach(t, c); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if gotProto != "HTTP/2.0" {
		t.Fatalf("server saw %s, want HTTP/2.0 — the transport lost its defaults", gotProto)
	}
}

// rotatableTLSServer serves a certificate that can be swapped at runtime, which
// is what a cluster rebuild or a cert-manager renewal does underneath a
// long-running MCP session (#89).
type rotatableTLSServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	cur  *tls.Certificate
	hits int
}

func newRotatableTLSServer(t *testing.T) (*rotatableTLSServer, string, []byte) {
	t.Helper()
	certPEM, keyPEM := loopbackCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	r := &rotatableTLSServer{cur: &cert}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.hits++
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits++
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Echo the request body back in the field Chat actually parses ("content"),
		// so a retry that lost its body shows up as an empty answer.
		_ = json.NewEncoder(w).Encode(map[string]any{"content": string(body)})
	})

	srv := httptest.NewUnstartedServer(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Certificates must be non-empty or httptest.StartTLS substitutes its own
		// certificate and GetCertificate is never consulted. GetCertificate is
		// what actually serves, so this entry is only there to stop the override.
		Certificates: []tls.Certificate{cert},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.cur, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.srv = srv
	// Dial by name, not by IP. Go consults GetCertificate only when Certificates
	// is empty OR the ClientHello carries SNI, and a connection to 127.0.0.1
	// sends no SNI -- so an IP URL would serve the original certificate forever
	// and the rotation under test would silently not happen. The certificate
	// carries localhost as a DNS SAN.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	return r, url, certPEM
}

// rotate swaps in a freshly signed certificate and returns its new CA PEM.
func (r *rotatableTLSServer) rotate(t *testing.T) []byte {
	t.Helper()
	certPEM, keyPEM := loopbackCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	r.mu.Lock()
	r.cur = &cert
	r.mu.Unlock()
	// Drop existing connections. A keep-alive connection established before the
	// rotation keeps working -- correct TLS behavior, since the certificate is
	// only checked at handshake -- so without this the client never revalidates
	// and the test proves nothing. A real rebuild restarts the server, which
	// breaks the connections exactly like this.
	r.srv.CloseClientConnections()
	return certPEM
}

// The whole point of #89: a rotation must not require restarting the process.
func TestClientReloadsCAAfterRotation(t *testing.T) {
	srv, url, caPEM := newRotatableTLSServer(t)

	current := caPEM
	reloads := 0
	c, err := NewClientWithTLS(url, "k", TLSOptions{
		CAPEM:    string(caPEM),
		ReloadCA: func() (string, error) { reloads++; return string(current), nil },
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	if _, err := c.Stats(context.Background()); err != nil {
		t.Fatalf("before rotation: %v", err)
	}
	if reloads != 0 {
		t.Fatalf("reloaded %d times on a healthy request; want 0", reloads)
	}

	current = srv.rotate(t) // cluster rebuilt: new CA, new leaf

	if _, err := c.Stats(context.Background()); err != nil {
		t.Fatalf("after rotation the client did not recover: %v", err)
	}
	if reloads != 1 {
		t.Errorf("reloaded %d times; want exactly 1", reloads)
	}
}

// A retried POST must carry its body. Without a GetBody rewind the retry sends
// an empty body and appears to succeed.
func TestClientRetryPreservesRequestBody(t *testing.T) {
	srv, url, caPEM := newRotatableTLSServer(t)
	current := caPEM
	c, err := NewClientWithTLS(url, "k", TLSOptions{
		CAPEM:    string(caPEM),
		ReloadCA: func() (string, error) { return string(current), nil },
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	current = srv.rotate(t)

	got, err := c.Chat(context.Background(), "hello")
	if err != nil {
		t.Fatalf("chat after rotation: %v", err)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("retried request lost its body: response %q does not contain the question", got)
	}
}

// Only unknown-authority failures reload. A refused connection must not.
func TestClientDoesNotReloadOnNonCAFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing is listening now

	_, caPEM := tlsAPIServer(t)
	reloads := 0
	c, err := NewClientWithTLS("https://"+addr, "k", TLSOptions{
		CAPEM:    string(caPEM),
		ReloadCA: func() (string, error) { reloads++; return string(caPEM), nil },
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := c.Stats(context.Background()); err == nil {
		t.Fatal("expected a connection failure")
	}
	if reloads != 0 {
		t.Errorf("reloaded %d times on a connection-refused error; want 0", reloads)
	}
}
