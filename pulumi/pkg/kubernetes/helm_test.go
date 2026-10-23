package kubernetes

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"accretion-infrastructure/pkg/config"
)

const testDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// The NAME of a reserved global address, which is what GCE resolves the
// global-static-ip-name annotation against -- not an address literal.
const testStaticIPName = "accretion-infrastructure-prod-static-ip"

func testConfig() *config.Config {
	cfg := &config.Config{Environment: "prod", Stack: "prod"}
	cfg.DNS.Domain = "example.com"
	cfg.App.ImageTag = "stable"
	cfg.App.Namespace = "accretion-system"
	cfg.App.EnableDiscord = true
	cfg.APIKeyHashes = map[string]string{"accretion-mcp": testDigest, "accretion-discord": testDigest}
	cfg.Database.SSLMode = config.DefaultDatabaseSSLMode
	return cfg
}

func mustSizing(t *testing.T, cfg *config.Config) stackSizing {
	t.Helper()
	sizing, err := sizingFor(cfg.Environment)
	if err != nil {
		t.Fatal(err)
	}
	return sizing
}

// dig walks a dotted path through a values map.
func dig(t *testing.T, values map[string]any, path ...string) any {
	t.Helper()
	var current any = values
	for i, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%v is not a mapping at %q", path[:i], key)
		}
		current, ok = m[key]
		if !ok {
			t.Fatalf("no value at %v", path[:i+1])
		}
	}
	return current
}

// Every key either release receives must be one its chart declares. See
// config.UndeclaredPaths for the failure this catches.
func TestReleaseValuesAreDeclaredByTheirCharts(t *testing.T) {
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

	for _, tc := range []struct {
		values func() map[string]any
		chart  string
	}{
		{chart: config.ChartAccretionAPI, values: func() map[string]any {
			return accretionValues(cfg, base, mustSizing(t, cfg), identities, testStaticIPName, "postgresql")
		}},
		{chart: config.ChartAccretionCollector, values: func() map[string]any {
			return collectorValues(cfg, vals.Values(config.ChartAccretionCollector), mustSizing(t, cfg), "postgresql")
		}},
		{chart: config.ChartAccretionDiscord, values: func() map[string]any {
			return discordValues(cfg, vals.Values(config.ChartAccretionDiscord))
		}},
	} {
		declared, err := vals.ChartDefaults(tc.chart)
		if err != nil {
			t.Fatalf("%s: %v", tc.chart, err)
		}
		if undeclared := config.UndeclaredPaths(tc.values(), declared); len(undeclared) > 0 {
			t.Errorf("the %s release sets keys the chart does not declare, so Helm would ignore them: %v",
				tc.chart, undeclared)
		}
	}
}

func TestAccretionValues(t *testing.T) {
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
	values := accretionValues(cfg, base, mustSizing(t, cfg), identities, testStaticIPName, "postgresql")

	if got := dig(t, values, "image", "tag"); got != "stable" {
		t.Errorf("image.tag = %v, want the per-stack tag", got)
	}
	if got := dig(t, values, "ingress", "globalStaticIP"); got != testStaticIPName {
		t.Errorf("ingress.globalStaticIP = %v, want the infrastructure output", got)
	}
	if got := dig(t, values, "config", "database", "host"); got != "postgresql" {
		t.Errorf("config.database.host = %v, want the infrastructure output", got)
	}

	// Finding 2: the password only reaches the pod because the chart is pointed
	// at the Secret Pulumi provisions. Without this the chart creates its own
	// accretion-api-db Secret and Pulumi's is mounted by nothing.
	if got := dig(t, values, "config", "database", "existingSecret"); got != "accretion-api-secrets" {
		t.Errorf("config.database.existingSecret = %v, want the Secret Pulumi provisions", got)
	}

	// One domain source: the Ingress hosts and the managed certificate both come
	// from the stack's domain, which is also what the DNS module provisions the
	// managed zone from.
	hosts, _ := dig(t, values, "ingress", "hosts").([]any)
	if len(hosts) != 2 {
		t.Fatalf("got %d ingress hosts, want the apex and www", len(hosts))
	}
	first, _ := hosts[0].(map[string]any)
	if first["host"] != cfg.DNS.Domain {
		t.Errorf("first ingress host is %v, want %v", first["host"], cfg.DNS.Domain)
	}
	domains, _ := dig(t, values, "ingress", "managedCertificate", "domains").([]any)
	if len(domains) != 2 || domains[0] != cfg.DNS.Domain || domains[1] != "www."+cfg.DNS.Domain {
		t.Errorf("certificate domains %v do not match the hosts the Ingress serves", domains)
	}

	// Finding 1: the release could not render at all without identities.
	apiKeys, _ := values["apiKeys"].([]any)
	if len(apiKeys) == 0 {
		t.Fatal("apiKeys is empty; the chart's fail-closed guard would reject the render")
	}
	for _, entry := range apiKeys {
		id, _ := entry.(map[string]any)
		if id["hash"] != testDigest {
			t.Errorf("identity %v did not receive its configured digest", id["name"])
		}
		if _, leaked := id["key"]; leaked {
			t.Errorf("identity %v carries a plaintext key", id["name"])
		}
	}
}

func TestCollectorValues(t *testing.T) {
	vals, err := config.LoadChartValues()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	values := collectorValues(cfg, vals.Values(config.ChartAccretionCollector), mustSizing(t, cfg), "postgresql")

	// The tag has to reach all three collector Deployments. Naming a Deployment
	// that no longer exists is exactly how it previously reached none of them.
	for _, section := range []string{"collector", "dataCollector", "statusCollector"} {
		if got := dig(t, values, section, "image", "tag"); got != "stable" {
			t.Errorf("%s.image.tag = %v, want the per-stack tag", section, got)
		}
		if got := dig(t, values, section, "image", "pullPolicy"); got != "Always" {
			t.Errorf("%s.image.pullPolicy = %v; Never is a local-kind default that "+
				"ImagePullBackOffs against a registry tag", section, got)
		}
	}

	autoscaling := mustSizing(t, cfg).collectorAutoscaling
	for _, kind := range []string{"log", "metric"} {
		if got := dig(t, values, "dataCollector", "kinds", kind, "autoscaling", "maxReplicas"); got != autoscaling["maxReplicas"] {
			t.Errorf("dataCollector.kinds.%s autoscaling did not land: %v", kind, got)
		}
		// Splitting is what makes the per-signal sizing above mean anything.
		if got := dig(t, values, "dataCollector", "kinds", kind, "split"); got != true {
			t.Errorf("dataCollector.kinds.%s.split = %v, want true", kind, got)
		}
	}

	// redis.enabled selects a Bitnami subchart this chart does not declare, so
	// true deploys no Redis at all -- fatal under the scale-out the splits above
	// enable.
	if got := dig(t, values, "redis", "enabled"); got != false {
		t.Errorf("redis.enabled = %v, want false", got)
	}

	if got := dig(t, values, "config", "database", "existingSecret"); got != "accretion-api-secrets" {
		t.Errorf("config.database.existingSecret = %v, want the Secret Pulumi provisions", got)
	}
	// This chart's default key name is "password"; Pulumi's Secret names it
	// db-password, so an unnamed key is a key that is not there.
	if got := dig(t, values, "config", "database", "existingSecretKey"); got != "db-password" {
		t.Errorf("config.database.existingSecretKey = %v, want db-password", got)
	}
}

func TestDiscordValues(t *testing.T) {
	vals, err := config.LoadChartValues()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	values := discordValues(cfg, vals.Values(config.ChartAccretionDiscord))

	// The chart ships off by default (#94): a release that does not say so
	// renders zero objects, silently.
	if got := dig(t, values, "enabled"); got != true {
		t.Errorf("enabled = %v, want true", got)
	}
	if got := dig(t, values, "image", "tag"); got != "stable" {
		t.Errorf("image.tag = %v, want the per-stack tag", got)
	}

	// No credential may travel as a value. The chart would happily accept these
	// and render its own Secret from them.
	secret, _ := values["secret"].(map[string]any)
	if secret["create"] != false {
		t.Errorf("secret.create = %v, want false so Pulumi provisions the Secret", secret["create"])
	}
	for _, field := range []string{"botToken", "anthropicApiKey", "openaiApiKey"} {
		if value, present := secret[field]; present && value != "" {
			t.Errorf("secret.%s carries a credential in the release values: %v", field, value)
		}
	}

	// The bot's own API key must come from the Secret Pulumi provisions. The
	// chart's default points at accretion-api's client-keys Secret, which is
	// never created for hash-only identities.
	if got := dig(t, values, "api", "secretName"); got != config.DiscordSecretName {
		t.Errorf("api.secretName = %v, want %v", got, config.DiscordSecretName)
	}
	if got := dig(t, values, "api", "secretKey"); got != config.DiscordAPIKeySecretKey() {
		t.Errorf("api.secretKey = %v, want %v", got, config.DiscordAPIKeySecretKey())
	}

	// The model keys reach the pod by reference, from that same Secret.
	env, _ := values["env"].([]any)
	wanted := map[string]string{
		"ACCRETION_LLM_ANTHROPIC_API_KEY": "anthropic-api-key",
		"ACCRETION_LLM_OPENAI_API_KEY":    "openai-api-key",
	}
	for _, entry := range env {
		e, _ := entry.(map[string]any)
		name, _ := e["name"].(string)
		wantKey, ok := wanted[name]
		if !ok {
			continue
		}
		ref := dig(t, e, "valueFrom", "secretKeyRef")
		refMap, _ := ref.(map[string]any)
		if refMap["name"] != config.DiscordSecretName || refMap["key"] != wantKey {
			t.Errorf("%s reads %v, want %s/%s", name, refMap, config.DiscordSecretName, wantKey)
		}
		delete(wanted, name)
		if _, hasValue := e["value"]; hasValue {
			t.Errorf("%s carries a literal value rather than a secret reference", name)
		}
	}
	if len(wanted) > 0 {
		t.Errorf("model keys never reach the pod: %v", wanted)
	}
}

// A stack that does not deploy the bot must not be asked to configure a digest
// for an identity that would authenticate nothing.
func TestDiscordIdentityFollowsTheGate(t *testing.T) {
	vals, err := config.LoadChartValues()
	if err != nil {
		t.Fatal(err)
	}
	roster := vals.Values(config.ChartAccretionAPI)["apiKeys"]

	names := func(identities []any) []string {
		out := make([]string, 0, len(identities))
		for _, entry := range identities {
			id, _ := entry.(map[string]any)
			name, _ := id["name"].(string)
			out = append(out, name)
		}
		return out
	}

	withBot, err := config.ResolveAPIKeyIdentities(roster, testConfig().APIKeyHashes, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(withBot), config.DiscordIdentity) {
		t.Errorf("the bot has no identity when it is deployed: %v", names(withBot))
	}

	// Only the mcp digest configured, and the bot excluded: this must succeed.
	withoutBot, err := config.ResolveAPIKeyIdentities(
		config.RosterExcluding(roster, config.DiscordIdentity),
		map[string]string{"accretion-mcp": testDigest}, "prod")
	if err != nil {
		t.Fatalf("a stack with no bot was still asked for the bot's digest: %v", err)
	}
	if slices.Contains(names(withoutBot), config.DiscordIdentity) {
		t.Errorf("the bot kept an identity on a stack that does not deploy it: %v", names(withoutBot))
	}
	if len(withoutBot) == 0 {
		t.Error("excluding the bot emptied the roster, which the chart guard would reject")
	}
}

// Every environment a stack file can actually set must have sizing, and the
// production tier must reach the production stack.
//
// The switch this replaced branched on "production", which no stack has ever
// set, so the production branch was dead and prod silently took development
// sizing. Nothing caught it because the lane's own tests were written against
// "production" as well -- they exercised an environment that does not exist.
// Reading the stack files rather than restating their contents is what keeps
// that from happening again.
func TestSizingCoversEveryStackEnvironment(t *testing.T) {
	stacks, err := filepath.Glob(filepath.Join("..", "..", "stacks", "Pulumi.*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) == 0 {
		t.Fatal("no stack files found; this check would pass vacuously")
	}

	for _, stack := range stacks {
		raw, err := os.ReadFile(stack)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Config map[string]any `yaml:"config"`
		}
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		env, _ := parsed.Config["accretion-infrastructure:environment"].(string)
		if env == "" {
			t.Errorf("%s sets no environment", stack)
			continue
		}
		if _, err := sizingFor(env); err != nil {
			t.Errorf("%s sets environment %q, which has no sizing: %v", stack, env, err)
		}
	}

	// The default config.NewConfig applies when a stack sets nothing.
	if _, err := sizingFor("dev"); err != nil {
		t.Errorf("the default environment has no sizing: %v", err)
	}
}

func TestSizingTiers(t *testing.T) {
	for _, tc := range []struct {
		env         string
		requestCPU  string
		maxReplicas int
		minReplicas int
	}{
		{env: "prod", requestCPU: "500m", minReplicas: 3, maxReplicas: 20},
		{env: "staging", requestCPU: "250m", minReplicas: 2, maxReplicas: 10},
		{env: "dev", requestCPU: "250m", minReplicas: 2, maxReplicas: 10},
	} {
		t.Run(tc.env, func(t *testing.T) {
			sizing, err := sizingFor(tc.env)
			if err != nil {
				t.Fatal(err)
			}
			requests, _ := sizing.apiResources["requests"].(map[string]any)
			if requests[keyCPU] != tc.requestCPU {
				t.Errorf("API CPU request = %v, want %v", requests[keyCPU], tc.requestCPU)
			}
			if sizing.collectorAutoscaling["minReplicas"] != tc.minReplicas {
				t.Errorf("collector minReplicas = %v, want %d", sizing.collectorAutoscaling["minReplicas"], tc.minReplicas)
			}
			if sizing.collectorAutoscaling["maxReplicas"] != tc.maxReplicas {
				t.Errorf("collector maxReplicas = %v, want %d", sizing.collectorAutoscaling["maxReplicas"], tc.maxReplicas)
			}
		})
	}

	// An unknown environment must fail rather than silently take the smallest
	// tier, which is what let prod run on dev sizing.
	if _, err := sizingFor("production"); err == nil {
		t.Error("an environment no stack sets was silently given a sizing tier")
	}
}
