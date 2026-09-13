package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"github.com/autonomousops-co/accretion/internal/mcp/accretion"
	"github.com/autonomousops-co/accretion/internal/mcp/secrets"
)

// caCertKey is the data key cert-manager writes the issuing CA into, which is
// also where a bring-your-own TLS Secret is expected to put it.
const caCertKey = "ca.crt"

// resolveCA returns the CA bundle to verify the API against, following whichever
// source is configured. It exits the process on a source that is named but
// unusable: a CA that was meant to be supplied and could not be read must not
// silently degrade into "verify against the system pool", which would fail later
// and less clearly.
//
// The order is least-privilege first. ACCRETION_API_CA_CONFIGMAP points at a
// ConfigMap of public material and is the preferred in-cluster pointer;
// ACCRETION_API_CA_SECRET points at the serving Secret, which ALSO holds the
// listener's private key — Kubernetes RBAC cannot narrow a read to one key, so
// that grant hands the reader the key too. It stays supported because it needs
// no extra object, but a deployment that cares should publish a CA-only
// ConfigMap (cert-manager's trust-manager does this) and point at that.
// It returns an error rather than exiting so the same function can serve as the
// client's CA reloader (see #89): the CA is re-resolved when a request fails
// against an unknown authority, which happens whenever the certificate is
// rotated under a long-running MCP session. The startup path still exits on
// failure, so fail-loud behavior there is unchanged.
func resolveCA(vals map[string]string) (string, error) {
	if pem := vals["ACCRETION_API_CA_PEM"]; pem != "" {
		return pem, nil
	}
	if vals["ACCRETION_API_CA_FILE"] != "" {
		return "", nil // the client reads the file itself, and re-reads it on reload
	}

	if ref := vals["ACCRETION_API_CA_CONFIGMAP"]; ref != "" {
		pem, err := secrets.ResolveKeyFromConfigMap(ref, caCertKey)
		if err != nil {
			return "", fmt.Errorf("failed to resolve the API CA from configmap %s: %w", ref, err)
		}
		if pem == "" {
			return "", fmt.Errorf("configmap %s has no %s key to verify the API certificate against", ref, caCertKey)
		}
		return pem, nil
	}

	if ref := vals["ACCRETION_API_CA_SECRET"]; ref != "" {
		caVals, err := secrets.ResolveFromK8s(ref, []string{caCertKey})
		if err != nil {
			return "", fmt.Errorf("failed to resolve the API CA from secret %s: %w", ref, err)
		}
		if caVals[caCertKey] == "" {
			return "", fmt.Errorf("secret %s has no %s key to verify the API certificate against", ref, caCertKey)
		}
		return caVals[caCertKey], nil
	}

	return "", nil
}

// version is stamped at build time via -ldflags "-X main.version=...".
// See scripts/version.sh. It is what this server reports to MCP clients.
var version = "dev"

func main() {
	// ACCRETION_API_CA_SECRET is a reference, not the bundle: cert-manager writes
	// ca.crt into its own Secret, and Helm cannot read cluster state at render
	// time, so the MCP Secret cannot carry a copy of the PEM. It carries the
	// pointer instead and this process follows it — one credential fetch and one
	// CA fetch, both through the kubeconfig the contributor already has, with
	// nothing to place on disk by hand. ACCRETION_API_CA_FILE and
	// ACCRETION_API_CA_PEM remain available for anyone who does hold the bundle.
	keys := []string{
		"ACCRETION_API_URL",
		"ACCRETION_API_KEY",
		"ACCRETION_API_CA_FILE",
		"ACCRETION_API_CA_PEM",
		"ACCRETION_API_CA_CONFIGMAP",
		"ACCRETION_API_CA_SECRET",
		"ACCRETION_API_INSECURE_SKIP_VERIFY",
	}

	var vals map[string]string
	if ref := os.Getenv("ACCRETION_MCP_SECRET_NAME"); ref != "" {
		var err error
		vals, err = secrets.ResolveFromK8s(ref, keys)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to resolve secrets: %v\n", err)
			os.Exit(1)
		}
	} else {
		vals = secrets.ResolveFromEnv(keys)
	}

	if vals["ACCRETION_API_URL"] == "" {
		fmt.Fprintln(os.Stderr,
			"ACCRETION_API_URL is required. Set it as an env var,"+
				" or set ACCRETION_MCP_SECRET_NAME=namespace/name to load from K8s.")
		os.Exit(1) //nolint:gocritic // intentional exit before server start
	}

	caPEM, err := resolveCA(vals)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client, err := accretion.NewClientWithTLS(
		vals["ACCRETION_API_URL"],
		vals["ACCRETION_API_KEY"],
		accretion.TLSOptions{
			CAFile:             vals["ACCRETION_API_CA_FILE"],
			CAPEM:              caPEM,
			InsecureSkipVerify: vals["ACCRETION_API_INSECURE_SKIP_VERIFY"] == "true",
			// Re-resolved on an unknown-authority failure. Without this a
			// rotation leaves the session verifying against a CA that no longer
			// signs anything, and only a restart recovers it.
			ReloadCA: func() (string, error) { return resolveCA(vals) },
		},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure the API client: %v\n", err)
		os.Exit(1)
	}
	mcpServer := accretion.NewServer(client, version)

	stdio := server.NewStdioServer(mcpServer)
	if err := stdio.Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "accretion-mcp error: %v\n", err)
		os.Exit(1)
	}
}
