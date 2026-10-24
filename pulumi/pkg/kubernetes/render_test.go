package kubernetes

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"accretion-infrastructure/pkg/config"
)

// The acceptance check for this whole configuration surface: render each chart
// with EXACTLY the values its release receives.
//
// A values map can look correct, pass every unit test about its own contents,
// and still configure nothing (see config.UndeclaredPaths). Only the rendered
// manifests show that a setting arrived, so every assertion below is made
// against those and not against the map.
func TestChartsRenderWithReleaseValues(t *testing.T) {
	helm := requireHelm(t)

	vals, err := config.LoadChartValues()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()

	base := vals.Values(config.ChartAccretionAPI)
	identities, err := config.ResolveAPIKeyIdentities(base["apiKeys"], cfg.APIKeyHashes, cfg.Stack)
	if err != nil {
		t.Fatal(err)
	}

	t.Run(config.ChartAccretionAPI, func(t *testing.T) {
		rendered := render(t, helm, vals, cfg, config.ChartAccretionAPI,
			accretionValues(cfg, base, mustSizing(t, cfg), identities, testStaticIPName, "postgresql"))

		// Finding 1: this release could not render at all. It supplied no
		// apiKeys and loaded no values file, so the chart's fail-closed guard
		// rejected it -- on main too, so it predates sprint-12.
		mustContain(t, rendered, "accretion-api:stable", "the per-stack image tag")
		mustContain(t, rendered, testDigest, "the API-key digest, in the ConfigMap the server reads")
		mustContain(t, rendered, `host: "example.com"`, "the Ingress host from the stack's domain")
		// GCE resolves this annotation against the NAME of a reserved global
		// address, not against an address literal.
		mustContain(t, rendered, testStaticIPName, "the reserved static IP's name")

		// The ManagedCertificate is useless unless the Ingress is annotated to
		// use it, and the two are emitted by different templates. This asserts
		// the join, not just that each half exists -- charts/accretion-api's
		// ingress template is lane 1's this sprint, so this is also what would
		// catch a change there regressing the Pulumi path.
		mustContain(t, rendered, "networking.gke.io/managed-certificates: accretion-api-ssl-cert",
			"the annotation attaching the ManagedCertificate to the Ingress")
		mustContain(t, rendered, "kubernetes.io/ingress.global-static-ip-name: "+testStaticIPName,
			"the annotation binding the Ingress to the reserved static IP")
		mustContain(t, rendered, "kind: ManagedCertificate", "the ManagedCertificate itself")

		// Finding 2: the database password never reached the pod. Pulumi
		// provisions accretion-api-secrets, the chart read accretion-api-db.
		mustReferenceSecret(t, rendered, "accretion-api-secrets", "db-password")
		if strings.Contains(rendered, "name: accretion-api-db\n") {
			t.Error("the chart still creates its own accretion-api-db Secret, so Pulumi's is mounted by nothing")
		}
	})

	t.Run(config.ChartAccretionCollector, func(t *testing.T) {
		rendered := render(t, helm, vals, cfg, config.ChartAccretionCollector,
			collectorValues(cfg, vals.Values(config.ChartAccretionCollector), mustSizing(t, cfg), "postgresql"))

		// The tag reaching every collector Deployment is the drift this file
		// exists to catch: it previously reached none of them.
		if got := strings.Count(rendered, "accretion-api:stable"); got < 3 {
			t.Errorf("the image tag reached %d containers, want every collector Deployment", got)
		}
		mustContain(t, rendered, "kind: HorizontalPodAutoscaler", "the autoscaling settings")
		mustContain(t, rendered, "name: accretion-api-secrets", "the database password Secret")
		if strings.Contains(rendered, "kind: Secret") && strings.Contains(rendered, "postgresql-secret\n") {
			t.Error("the chart still creates its own postgresql-secret despite config.database.existingSecret")
		}

		// The queue password must come from the Secret Pulumi provisions, not
		// from the chart's checked-in default. base64 because the chart's own
		// Secret writes it under `data`, which is how it was missed by eye.
		defaultPassword := base64.StdEncoding.EncodeToString([]byte("accretion-api-redis-password"))
		if strings.Contains(rendered, defaultPassword) || strings.Contains(rendered, "accretion-api-redis-password") {
			t.Error("the collector still ships the Redis password checked into this repository")
		}
		// The two shapes reach the pods differently: clients read the raw
		// password by secretKeyRef, while redis-server has no env-var form of
		// requirepass and takes the directive as a mounted file.
		mustReferenceSecret(t, rendered, config.RedisSecretName, config.RedisPasswordKey)
		mustContain(t, rendered, "secretName: "+config.RedisSecretName,
			"the Redis Secret mounted for the server")
		mustContain(t, rendered, "- key: "+config.RedisConfKey,
			"the requirepass directive projected from that Secret")
	})

	t.Run(config.ChartAccretionDiscord, func(t *testing.T) {
		rendered := render(t, helm, vals, cfg, config.ChartAccretionDiscord,
			discordValues(cfg, vals.Values(config.ChartAccretionDiscord)))

		// The chart is off by default, and a release that does not turn it on
		// renders zero objects without saying so.
		mustContain(t, rendered, "kind: Deployment", "the bot's Deployment")
		mustContain(t, rendered, "accretion-api:stable", "the per-stack image tag")

		// Every credential arrives by reference, from the Secret Pulumi
		// provisions -- including the bot's own API key, which the accretion-api
		// chart never publishes for a hash-only identity.
		//
		// Both directions, against the one list secrets.go builds the Secret
		// from: every provisioned key is read by the pod, and the pod reads no
		// key from that Secret that Pulumi does not provision. A ref naming a
		// key nobody wrote renders perfectly and then fails the pod at rollout,
		// because none of these refs is optional.
		provisioned := config.DiscordSecretKeys()
		for _, key := range provisioned {
			mustReferenceSecret(t, rendered, config.DiscordSecretName, key)
		}
		referenced := referencedKeys(rendered, config.DiscordSecretName)
		if len(referenced) != len(provisioned) {
			t.Errorf("the bot reads %d keys from %s, but Pulumi provisions %d: %v vs %v",
				len(referenced), config.DiscordSecretName, len(provisioned), referenced, provisioned)
		}
		for _, key := range referenced {
			if !slices.Contains(provisioned, key) {
				t.Errorf("the bot reads %q from %s, which Pulumi does not provision",
					key, config.DiscordSecretName)
			}
		}

		// The chart creates no Secret of its own, so no credential is in the
		// release. REPLACE_WITH_SECRET_MANAGER_REF is what Pulumi puts in the
		// Secret; seeing it here would mean a value had traveled through Helm.
		if strings.Contains(rendered, "kind: Secret") {
			t.Error("the chart rendered a Secret, so a credential traveled as a Helm value")
		}
		// Both encodings, for the same reason as the Redis check above: a
		// Secret's values are base64 under `data`, and a plaintext-only grep is
		// how that one was missed. Moot while this chart renders no Secret,
		// which is exactly when a blind spot goes unnoticed.
		const placeholder = "REPLACE_WITH_SECRET_MANAGER_REF"
		for _, form := range []string{placeholder, base64.StdEncoding.EncodeToString([]byte(placeholder))} {
			if strings.Contains(rendered, form) {
				t.Error("a Secret Manager placeholder reached the rendered manifests")
			}
		}
	})
}

// requireHelm resolves helm, or fails.
//
// A skip cannot be made loud: `go test` prints a passing package's output only
// under -v, so t.Skip's message and a write to stderr are both discarded and a
// bare `go test ./...` says "ok" either way. So a missing helm fails, and the
// only way to reach a skip is to ask for one with -short.
//
// -short rather than an environment variable, which was the previous shape and
// was wrong: it tested for PRESENCE, so setting it to "0" or "false" -- the two
// spellings of "off" -- turned the hatch ON and silenced the gate. That is the
// mistake charts/accretion-api/templates/_validation.tpl argues at length in
// this same sprint, that only an explicit false may be an off switch. A flag
// has no value to misread, and CI cannot inherit one by accident the way it can
// inherit an exported variable, so no CI branch is needed here at all.
func requireHelm(t *testing.T) string {
	t.Helper()

	helm, err := exec.LookPath("helm")
	if err == nil {
		return helm
	}
	if testing.Short() {
		t.Skip("helm is not installed and -short was given; the render check did not run")
	}
	t.Fatal("helm is not on PATH, so no chart could be rendered. This is the check that proves the " +
		"release values reach the manifests, so it fails rather than skipping quietly -- `go test` " +
		"hides a skip unless you pass -v. Install helm, or pass -short to skip it deliberately.")
	return ""
}

func render(t *testing.T, helm string, vals *config.ChartValues, cfg *config.Config, chart string, values map[string]any) string {
	t.Helper()

	encoded, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(helm, "template", chart, vals.ChartPath(chart),
		"--namespace", cfg.App.Namespace, "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s failed: %v\nvalues:\n%s\noutput:\n%s", chart, err, encoded, out)
	}
	return string(out)
}

// renderedSSLModes returns every sslmode value the manifests actually set.
func renderedSSLModes(rendered string) []string {
	matches := sslModeLine.FindAllStringSubmatch(rendered, -1)
	modes := make([]string, 0, len(matches))
	for _, match := range matches {
		modes = append(modes, match[1])
	}
	return modes
}

var sslModeLine = regexp.MustCompile(`sslmode:\s*"?([^"\s]+)"?`)

// referencedKeys finds every key the manifests read from one Secret, in either
// field ordering (see mustReferenceSecret).
func referencedKeys(rendered, secretName string) []string {
	var keys []string
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if i+1 >= len(lines) {
			break
		}
		this, next := strings.TrimSpace(line), strings.TrimSpace(lines[i+1])
		switch {
		case this == "name: "+secretName && strings.HasPrefix(next, "key: "):
			keys = append(keys, strings.TrimPrefix(next, "key: "))
		case strings.HasPrefix(this, "key: ") && next == "name: "+secretName:
			keys = append(keys, strings.TrimPrefix(this, "key: "))
		}
	}
	return keys
}

// mustReferenceSecret asserts a secretKeyRef naming this Secret and key is in
// the manifests. Both field orderings are handled by referencedKeys, which is
// the only place that knows them.
func mustReferenceSecret(t *testing.T, rendered, secretName, key string) {
	t.Helper()
	if !slices.Contains(referencedKeys(rendered, secretName), key) {
		t.Errorf("no secretKeyRef reads %s from %s in the rendered manifests", key, secretName)
	}
}

func mustContain(t *testing.T, rendered, want, what string) {
	t.Helper()
	if !strings.Contains(rendered, want) {
		t.Errorf("%s did not reach the rendered manifests (looked for %q)", what, want)
	}
}

// No stack may send database traffic in the clear. Checked where it counts, in
// the rendered ConfigMap of BOTH charts, for EVERY environment a stack file can
// set -- the charts default sslmode to "disable", and before this the central
// file set it nowhere, so all three stacks took that default.
func TestNoStackRendersUnencryptedDatabaseTraffic(t *testing.T) {
	helm := requireHelm(t)

	vals, err := config.LoadChartValues()
	if err != nil {
		t.Fatal(err)
	}

	for env := range environmentSizing {
		t.Run(env, func(t *testing.T) {
			cfg := testConfig()
			cfg.Environment = env
			cfg.Stack = env

			base := vals.Values(config.ChartAccretionAPI)
			identities, err := config.ResolveAPIKeyIdentities(base["apiKeys"], cfg.APIKeyHashes, cfg.Stack)
			if err != nil {
				t.Fatal(err)
			}

			for chart, values := range map[string]map[string]any{
				config.ChartAccretionAPI: accretionValues(
					cfg, base, mustSizing(t, cfg), identities, testStaticIPName, "postgresql"),
				config.ChartAccretionCollector: collectorValues(
					cfg, vals.Values(config.ChartAccretionCollector), mustSizing(t, cfg), "postgresql"),
			} {
				rendered := render(t, helm, vals, cfg, chart, values)

				// Asserts the rendered mode is IN the allowlist, not that it is
				// not one denied value. Interpolating cfg.Database.SSLMode here
				// asserted only that whatever the config held reached the
				// manifest, whatever that was -- plumbing, not the invariant --
				// so this test passed green with every stack rendering
				// "prefer", which permits plaintext.
				modes := renderedSSLModes(rendered)
				if len(modes) == 0 {
					t.Fatalf("%s rendered no sslmode at all; this check would pass vacuously", chart)
				}
				for _, mode := range modes {
					if !slices.Contains(config.EncryptedSSLModes(), mode) {
						t.Errorf("%s renders sslmode %q on stack %q, which does not guarantee an "+
							"encrypted connection (accepted: %v)",
							chart, mode, env, config.EncryptedSSLModes())
					}
				}
			}
		})
	}
}

// The invariant itself, independent of rendering: "disable" is refused however
// it arrives, and so is an empty value.
func TestDatabaseSSLModeInvariant(t *testing.T) {
	// THIS LIST IS WHERE THE PROTECTION LIVES. Do not trim it.
	//
	// The loop below over config.EncryptedSSLModes() is tautological on its own
	// -- it asserts the implementation accepts what the implementation lists --
	// and the length check only catches growth, not a same-size swap. Only these
	// hardcoded refusals are stated independently of the implementation, so they
	// are the half that would actually catch a regression. All three together
	// are sound; remove this one and the protection goes with it.
	//
	// prefer and allow are the ones a denylist misses: both permit plaintext,
	// and prefer is what an operator reaches for when require will not connect.
	// requre and REQUIRE are near-misses libpq would reject at connection time.
	for _, mode := range []string{"", "disable", "prefer", "allow", "requre", "REQUIRE", "verify_full"} {
		if err := config.ValidateDatabaseSSLMode(mode); err == nil {
			t.Errorf("sslMode %q was accepted; it does not guarantee an encrypted connection", mode)
		}
	}
	// Asserted against the implementation's own list rather than a restatement,
	// so the two cannot drift.
	for _, mode := range config.EncryptedSSLModes() {
		if err := config.ValidateDatabaseSSLMode(mode); err != nil {
			t.Errorf("sslMode %q was refused: %v", mode, err)
		}
	}
	if len(config.EncryptedSSLModes()) != 3 {
		t.Errorf("expected exactly require, verify-ca and verify-full: %v", config.EncryptedSSLModes())
	}
}
