package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/autonomousops-co/accretion/internal/config"
)

// healthcheckTimeout bounds the WHOLE run — config load, CA parse, and every
// listener probed — via a single context deadline started before any work.
//
// It was previously an http.Client.Timeout, which is per-REQUEST. That made the
// real worst case startup + N*5s, one N per enabled listener, so with both
// listeners enabled it exceeded the kubelet's timeoutSeconds and the process was
// SIGKILLed with no output — precisely the failure this constant claims to
// prevent. Observed as CrashLoopBackOff with the kubelet reporting only
// "command timed out". See #88.
//
// Two budgets have to stay ordered for this to mean anything:
//
//	healthcheckTimeout (5s) < probe timeoutSeconds (6s) in the chart
//
// The kubelet's default timeoutSeconds is 1, which would kill the process before
// this deadline could fire, so charts/accretion-api/templates/deployment.yaml
// sets 6 explicitly. Raising either without the other puts this back out of
// reach — silently, because the symptom is a killed process rather than an
// error.
const healthcheckTimeout = 5 * time.Second

// processStart anchors the budget to when this PROCESS began, not to when the
// command body started running.
//
// Package-level initialisation happens before main(), so this is captured before
// cobra parses flags and before cobra.OnInitialize runs initConfig — which does
// viper setup and reads /etc/config/config.yaml. An earlier fix started the
// deadline inside RunE, leaving all of that outside the budget, so the real
// total stayed "startup + 5s" and the kubelet still timed the command out at 6s.
// Anchoring here means time already spent is deducted rather than the clock
// restarting. See #88.
var processStart = time.Now()

// healthcheckContext returns a context that expires healthcheckTimeout after the
// process started. If that budget is already gone the context is expired on
// return, which is the point: the command fails fast with a message instead of
// running to completion and being SIGKILLed with no output.
func healthcheckContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, processStart.Add(healthcheckTimeout))
}

// probeTarget is one listener to check: a URL to GET and the name used in
// messages.
type probeTarget struct {
	name string
	url  string
}

// healthcheckTargets returns one target per ENABLED listener.
//
// Covering every enabled listener rather than just the API preserves what the
// wget exec probes did. The two listeners run in one process but can fail
// independently — which is the whole point of having split them — so a probe
// that checks only one of them reports healthy while half the service is down.
//
// 127.0.0.1 rather than a DNS name avoids a lookup on every interval, and the
// certificate issued by the chart carries 127.0.0.1 as a SAN precisely so this
// still verifies.
func healthcheckTargets(cfg *config.Config) []probeTarget {
	var targets []probeTarget

	if cfg.API.Enabled {
		scheme := "http"
		if cfg.API.TLS.Enabled {
			scheme = "https"
		}
		targets = append(targets, probeTarget{
			name: "api",
			url:  fmt.Sprintf("%s://%s/api/health", scheme, net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.API.Port))),
		})
	}

	// The UI listener is always cleartext: it is not reachable from outside the
	// pod in any shipped configuration, and api.tls configures the API listener
	// only.
	if cfg.UI.Enabled {
		targets = append(targets, probeTarget{
			name: "ui",
			url:  fmt.Sprintf("http://%s/api/health", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.UI.Port))),
		})
	}

	return targets
}

// runHealthcheck probes every enabled listener and returns the first failure.
// Split from the cobra command so it is testable against a real server rather
// than only through a process exit code.
func runHealthcheck(ctx context.Context, cfg *config.Config) error {
	targets := healthcheckTargets(cfg)
	if len(targets) == 0 {
		return fmt.Errorf("no listener is enabled, so there is nothing to probe: set api.enabled or ui.enabled")
	}

	// Verifies against the configured CA — or the system pool when none is set.
	// There is deliberately no skip-verify path here: a probe that accepts any
	// certificate would report a healthy TLS listener that no real client can
	// talk to.
	tlsConfig, err := cfg.API.TLS.ClientTLSConfig()
	if err != nil {
		return err
	}
	// No Timeout here on purpose: the deadline lives on ctx, which the caller
	// started before config was even loaded, so it covers the whole run rather
	// than resetting for each listener.
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	for _, t := range targets {
		if err := probe(ctx, client, t); err != nil {
			return err
		}
	}
	return nil
}

func probe(ctx context.Context, client *http.Client, t probeTarget) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, http.NoBody)
	if err != nil {
		return fmt.Errorf("%s listener unhealthy: building request for %s: %w", t.name, t.url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Say plainly when the budget is what ran out. The kubelet only reports
		// "command timed out", so without this the reason is invisible.
		if ctx.Err() != nil {
			return fmt.Errorf("%s listener unhealthy: GET %s exceeded the %s healthcheck budget: %w",
				t.name, t.url, healthcheckTimeout, err)
		}
		return fmt.Errorf("%s listener unhealthy: GET %s: %w", t.name, t.url, err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused across targets.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s listener unhealthy: GET %s returned %d, want 200", t.name, t.url, resp.StatusCode)
	}
	return nil
}

// healthcheckCmd exists because the probes cannot be a shell one-liner once the
// API listener is behind TLS. The runtime image is alpine, whose wget is
// BusyBox, and BusyBox wget has no --ca-certificate flag — a private CA cannot
// be passed on the command line. Installing the CA into the image trust store
// would need update-ca-certificates as root, and the container runs as appuser.
//
// Reading the same config the server reads means this knows the port, the scheme
// and the CA path with no duplication, and unlike a shell probe it can be tested
// in CI.
var healthcheckCmd = &cobra.Command{
	Use:   "healthcheck",
	Short: "Probe every enabled listener's /api/health endpoint",
	Long: `Probe the health endpoint of every enabled listener and exit non-zero if any
of them is unhealthy.

Intended as the container's liveness and readiness probe. It resolves the same
configuration the server does, so it knows each listener's port and whether the
API listener is behind TLS, and it verifies the API's certificate against
api.tls.ca_file rather than skipping verification.`,
	// A probe's own usage text is noise in a kubelet event; the error is what
	// matters.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Anchored to process start, so config loading, CA pool construction AND
		// everything cobra did before this point are inside the budget.
		ctx, cancel := healthcheckContext(cmd.Context())
		defer cancel()

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		if err := runHealthcheck(ctx, cfg); err != nil {
			// Print here rather than letting cobra format it, so the message the
			// kubelet captures is exactly one line with no "Error:" decoration
			// wrapping a wrapped error.
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(healthcheckCmd)
}
