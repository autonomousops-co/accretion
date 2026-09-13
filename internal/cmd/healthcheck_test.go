package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/config"
)

// healthServer starts a server answering 200 on /api/health, over TLS when
// asked. It returns the port and, for the TLS case, the path to a PEM file
// holding the signing certificate so the probe can VERIFY rather than skip.
func healthServer(t *testing.T, useTLS bool) (port int, caFile string) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewUnstartedServer(mux)
	// Bind to loopback explicitly: the certificate below carries 127.0.0.1 as
	// its only IP SAN, matching what the chart issues.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.Listener.Close()
	srv.Listener = ln

	if useTLS {
		certPEM, keyPEM := selfSignedLoopbackCert(t)
		dir := t.TempDir()
		caFile = filepath.Join(dir, "ca.crt")
		if writeErr := os.WriteFile(caFile, certPEM, 0o600); writeErr != nil {
			t.Fatalf("writing CA: %v", writeErr)
		}
		cert, pairErr := tls.X509KeyPair(certPEM, keyPEM)
		if pairErr != nil {
			t.Fatalf("keypair: %v", pairErr)
		}
		// Setting srv.TLS before StartTLS makes httptest serve OUR certificate
		// rather than its own, so the probe is verifying against the CA file
		// written above.
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing %s: %v", srv.URL, err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parsing port from %s: %v", srv.URL, err)
	}
	return port, caFile
}

func selfSignedLoopbackCert(t *testing.T) (certPEM, keyPEM []byte) {
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

func baseConfig(apiPort int) *config.Config {
	cfg := &config.Config{}
	cfg.API.Enabled = true
	cfg.API.Port = apiPort
	return cfg
}

func TestHealthcheckPassesAgainstCleartextListener(t *testing.T) {
	port, _ := healthServer(t, false)
	if err := runHealthcheck(context.Background(), baseConfig(port)); err != nil {
		t.Fatalf("healthcheck failed against a healthy cleartext listener: %v", err)
	}
}

// The TLS case verifies against the CA rather than skipping verification, which
// is the difference between a probe that proves the listener is usable and one
// that only proves something is listening.
func TestHealthcheckPassesAgainstTLSListener(t *testing.T) {
	port, caFile := healthServer(t, true)
	cfg := baseConfig(port)
	cfg.API.TLS = config.TLSConfig{Enabled: true, CAFile: caFile}

	if err := runHealthcheck(context.Background(), cfg); err != nil {
		t.Fatalf("healthcheck failed against a healthy TLS listener: %v", err)
	}
}

// Without the CA the probe must FAIL rather than fall back to accepting the
// certificate. A probe that passes here would mask exactly the misconfiguration
// it exists to catch.
func TestHealthcheckRejectsUntrustedCertificate(t *testing.T) {
	port, _ := healthServer(t, true)
	cfg := baseConfig(port)
	cfg.API.TLS = config.TLSConfig{Enabled: true} // no CA file: system pool only

	err := runHealthcheck(context.Background(), cfg)
	if err == nil {
		t.Fatal("healthcheck accepted a certificate signed by an unknown CA")
	}
	if !strings.Contains(err.Error(), "api listener unhealthy") {
		t.Fatalf("error %q does not identify the api listener", err)
	}
}

func TestHealthcheckFailsAgainstStoppedListener(t *testing.T) {
	// Take a port, then release it, so the probe dials something not listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	port := addr.Port
	_ = ln.Close()

	err = runHealthcheck(context.Background(), baseConfig(port))
	if err == nil {
		t.Fatal("healthcheck passed against a stopped listener")
	}
	if !strings.Contains(err.Error(), "api listener unhealthy") {
		t.Fatalf("error %q does not name the failing listener", err)
	}
}

// Both listeners enabled means both get probed. The two run in one process but
// fail independently, so a probe covering only the API would report healthy with
// the UI down — the behavior the wget exec probes had and this must preserve.
func TestHealthcheckCoversBothListeners(t *testing.T) {
	apiPort, _ := healthServer(t, false)

	deadPort := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
		}
		return addr.Port
	}()

	cfg := baseConfig(apiPort)
	cfg.UI.Enabled = true
	cfg.UI.Port = deadPort

	err := runHealthcheck(context.Background(), cfg)
	if err == nil {
		t.Fatal("healthcheck passed with the UI listener down")
	}
	if !strings.Contains(err.Error(), "ui listener unhealthy") {
		t.Fatalf("error %q does not name the ui listener", err)
	}
}

func TestHealthcheckTargets(t *testing.T) {
	t.Run("api only, cleartext", func(t *testing.T) {
		got := healthcheckTargets(baseConfig(41337))
		want := []probeTarget{{name: "api", url: "http://127.0.0.1:41337/api/health"}}
		assertTargets(t, got, want)
	})

	t.Run("api only, TLS", func(t *testing.T) {
		cfg := baseConfig(41337)
		cfg.API.TLS.Enabled = true
		got := healthcheckTargets(cfg)
		want := []probeTarget{{name: "api", url: "https://127.0.0.1:41337/api/health"}}
		assertTargets(t, got, want)
	})

	t.Run("both listeners", func(t *testing.T) {
		cfg := baseConfig(41337)
		cfg.API.TLS.Enabled = true
		cfg.UI.Enabled = true
		cfg.UI.Port = 8080
		got := healthcheckTargets(cfg)
		want := []probeTarget{
			{name: "api", url: "https://127.0.0.1:41337/api/health"},
			{name: "ui", url: "http://127.0.0.1:8080/api/health"},
		}
		assertTargets(t, got, want)
	})
}

// Nothing enabled is a misconfiguration, and reporting it as healthy would let a
// pod that serves no one pass its readiness probe.
func TestHealthcheckFailsWithNoListenerEnabled(t *testing.T) {
	err := runHealthcheck(context.Background(), &config.Config{})
	if err == nil {
		t.Fatal("healthcheck passed with no listener enabled")
	}
	if !strings.Contains(err.Error(), "no listener is enabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertTargets(t *testing.T, got, want []probeTarget) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d targets %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// hangingServer is a listener that accepts and then never answers, standing in
// for the slow node that produced the CrashLoopBackOff in #88.
func hangingServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without replying.
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	return addr.Port
}

// The budget must bound the WHOLE run, not each request. Before #88 the deadline
// was an http.Client.Timeout, so two enabled listeners cost 2x the budget and the
// kubelet SIGKILLed the process before it could report anything.
func TestHealthcheckBudgetCoversAllListenersNotEachRequest(t *testing.T) {
	apiPort, _ := healthServer(t, false)

	cfg := baseConfig(apiPort)
	cfg.UI.Enabled = true
	cfg.UI.Port = hangingServer(t)

	budget := 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	err := runHealthcheck(ctx, cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("healthcheck passed with a hung listener")
	}
	// The margin is for scheduling, not for a second full budget. A per-request
	// deadline would land at ~2x here.
	if elapsed > budget*3/2 {
		t.Fatalf("took %v against a %v budget: the deadline is per-request, not whole-run", elapsed, budget)
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error %q does not say the budget ran out; the kubelet only reports \"command timed out\", "+
			"so this message is the only evidence of why", err)
	}
}

// The deadline is started before config is loaded, so an already-spent budget
// fails immediately rather than doing a full round of work first.
func TestHealthcheckHonoursAnAlreadyExpiredBudget(t *testing.T) {
	port, _ := healthServer(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	start := time.Now()
	err := runHealthcheck(ctx, baseConfig(port))
	if err == nil {
		t.Fatal("healthcheck passed with an already-expired context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v to notice an expired deadline", elapsed)
	}
}

// The budget must be anchored to process start, not to the moment the command
// body runs. The first attempt at #88 restarted the clock inside RunE, leaving
// cobra's OnInitialize and the runtime's own startup outside the budget, so the
// kubelet still timed the command out.
func TestHealthcheckBudgetIsAnchoredToProcessStart(t *testing.T) {
	orig := processStart
	t.Cleanup(func() { processStart = orig })

	// Pretend most of the budget was already spent before the command ran.
	spent := healthcheckTimeout - 300*time.Millisecond
	processStart = time.Now().Add(-spent)

	ctx, cancel := healthcheckContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline set")
	}
	remaining := time.Until(deadline)
	if remaining > 500*time.Millisecond {
		t.Fatalf("remaining budget is %v; the clock restarted instead of deducting the %v already spent",
			remaining, spent)
	}
}

// An already-exhausted budget yields an expired context, so the command reports
// rather than running on and being killed with no output.
func TestHealthcheckContextExpiredWhenBudgetAlreadyGone(t *testing.T) {
	orig := processStart
	t.Cleanup(func() { processStart = orig })
	processStart = time.Now().Add(-2 * healthcheckTimeout)

	ctx, cancel := healthcheckContext(context.Background())
	defer cancel()

	if ctx.Err() == nil {
		t.Fatal("context is live despite the whole budget having elapsed before the command started")
	}

	port, _ := healthServer(t, false)
	start := time.Now()
	if err := runHealthcheck(ctx, baseConfig(port)); err == nil {
		t.Fatal("healthcheck passed on an exhausted budget")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v to fail on an exhausted budget", elapsed)
	}
}
