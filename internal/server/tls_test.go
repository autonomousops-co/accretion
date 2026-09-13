package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/autonomousops-co/accretion/internal/config"
)

// writeKeypair generates a short-lived self-signed certificate for 127.0.0.1 and
// writes it to two files in a temp dir, returning their paths. Tests need a real
// loadable keypair because Validate calls tls.LoadX509KeyPair — a fixture string
// would not exercise the path that matters.
func writeKeypair(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "accretion-test"},
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

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// serveTLS starts an http.Server the way Start does — the keypair comes from a
// validated TLSConfig and ListenAndServeTLS is called with empty filenames — and
// returns its address. Going through the real server rather than httptest.NewTLSServer
// is the point: it pins the production wiring, including the empty-filename call.
func serveTLS(t *testing.T, tlsConfig *tls.Config) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		TLSConfig: tlsConfig,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "ok")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestTLSConfigDisabledReturnsNil(t *testing.T) {
	c := &config.TLSConfig{Enabled: false, CertFile: "/nonexistent", KeyFile: "/nonexistent"}
	got, err := c.Validate()
	if err != nil {
		t.Fatalf("disabled TLS should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("disabled TLS should produce no tls.Config, got %+v", got)
	}
}

// The fail-closed guarantee: every way of being enabled-but-unusable is an
// error, never a nil tls.Config that would leave the listener in the clear.
func TestTLSConfigFailsClosed(t *testing.T) {
	certFile, keyFile := writeKeypair(t)
	otherCert, _ := writeKeypair(t)

	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	writeFile(t, garbage, []byte("not a certificate"))

	tests := []struct {
		name string
		cfg  config.TLSConfig
		want string
	}{
		{
			name: "missing cert file",
			cfg:  config.TLSConfig{Enabled: true, CertFile: filepath.Join(t.TempDir(), "absent.crt"), KeyFile: keyFile},
			want: "loading TLS keypair",
		},
		{
			name: "missing key file",
			cfg:  config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: filepath.Join(t.TempDir(), "absent.key")},
			want: "loading TLS keypair",
		},
		{
			name: "malformed certificate",
			cfg:  config.TLSConfig{Enabled: true, CertFile: garbage, KeyFile: keyFile},
			want: "loading TLS keypair",
		},
		{
			name: "cert and key do not match",
			cfg:  config.TLSConfig{Enabled: true, CertFile: otherCert, KeyFile: keyFile},
			want: "loading TLS keypair",
		},
		{
			name: "empty cert and key paths",
			cfg:  config.TLSConfig{Enabled: true},
			want: "cert_file and/or key_file is empty",
		},
		{
			name: "min_version below the floor",
			cfg:  config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, MinVersion: "1.1"},
			want: "below the TLS 1.2 floor",
		},
		{
			name: "unrecognized min_version",
			cfg:  config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, MinVersion: "banana"},
			want: "not a recognized TLS version",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("expected an error, got tls.Config %+v", got)
			}
			if got != nil {
				t.Fatalf("a failed Validate must return no tls.Config, got %+v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTLSMinVersionResolution(t *testing.T) {
	tests := []struct {
		in   string
		want uint16
	}{
		{"", tls.VersionTLS12},
		{"1.2", tls.VersionTLS12},
		{"1.3", tls.VersionTLS13},
		{"TLS1.3", tls.VersionTLS13},
		{"v1.3", tls.VersionTLS13},
		{" 1.3 ", tls.VersionTLS13},
	}
	for _, tc := range tests {
		t.Run("min="+tc.in, func(t *testing.T) {
			c := &config.TLSConfig{MinVersion: tc.in}
			got, err := c.GetMinVersion()
			if err != nil {
				t.Fatalf("GetMinVersion(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("GetMinVersion(%q) = %#x, want %#x", tc.in, got, tc.want)
			}
		})
	}
}

// A valid keypair serves HTTPS, and the negotiated version respects the floor.
func TestTLSListenerServesHTTPS(t *testing.T) {
	certFile, keyFile := writeKeypair(t)
	cfg := &config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	tlsConfig, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2 by default", tlsConfig.MinVersion)
	}

	addr := serveTLS(t, tlsConfig)
	client := &http.Client{
		Transport: &http.Transport{
			// The certificate is self-signed and unknown to the system pool, so
			// pin it explicitly rather than skipping verification — the test still
			// proves the handshake verifies against a known root.
			TLSClientConfig: &tls.Config{RootCAs: poolFor(t, certFile), MinVersion: tls.VersionTLS12},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("connection did not negotiate at least TLS 1.2: %+v", resp.TLS)
	}
}

// A client capped at TLS 1.1 must be refused rather than served, which is what
// makes min_version an enforced floor rather than a documented preference.
func TestTLSListenerRefusesBelowMinVersion(t *testing.T) {
	certFile, keyFile := writeKeypair(t)
	cfg := &config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, MinVersion: "1.2"}
	tlsConfig, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	addr := serveTLS(t, tlsConfig)

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		RootCAs:    poolFor(t, certFile),
		MinVersion: tls.VersionTLS10,
		MaxVersion: tls.VersionTLS11,
	})
	if err == nil {
		conn.Close()
		t.Fatal("a TLS 1.1 client was accepted; min_version is not enforced")
	}
}

// Plaintext to the TLS port must fail rather than be served. This is the
// regression that a "fall back to cleartext" implementation would hide.
func TestTLSListenerRefusesPlaintext(t *testing.T) {
	certFile, keyFile := writeKeypair(t)
	cfg := &config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	tlsConfig, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	addr := serveTLS(t, tlsConfig)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		return // refused at the transport layer, which is also a pass
	}
	defer resp.Body.Close()

	// Go's HTTP server does not drop a plaintext connection to a TLS port; it
	// answers 400 with an explicit diagnostic. That is a refusal, not service,
	// so assert on it precisely rather than on "the request errored" — the
	// failure this guards against is the handler running at all.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plaintext request to the TLS port returned %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), "HTTP request to an HTTPS server") {
		t.Fatalf("plaintext request was answered by the handler, not refused: %q", body)
	}
}

// Start must surface a bad keypair as a returned startup error. Before this it
// would have been a Fatal from inside a listener goroutine, which is both
// untestable and much harder to diagnose in a crash loop.
func TestStartFailsOnUnusableKeypair(t *testing.T) {
	cfg := &config.Config{}
	cfg.API.Enabled = true
	cfg.API.Port = 0
	cfg.API.TLS = config.TLSConfig{
		Enabled:  true,
		CertFile: filepath.Join(t.TempDir(), "absent.crt"),
		KeyFile:  filepath.Join(t.TempDir(), "absent.key"),
	}

	s := &Server{config: cfg, apiRouter: mux.NewRouter()}
	err := s.Start()
	if err == nil {
		t.Fatal("Start returned nil with an unloadable keypair; it must abort at startup")
	}
	if !strings.Contains(err.Error(), "api listener TLS") {
		t.Fatalf("error %q does not identify the API listener's TLS as the cause", err)
	}
}

func poolFor(t *testing.T, certFile string) *x509.CertPool {
	t.Helper()
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading %s: %v", certFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("no certificate found in %s", certFile)
	}
	return pool
}
