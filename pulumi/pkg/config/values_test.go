package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo builds a minimal tree that satisfies isRepoRoot: the central values
// file plus a Chart.yaml for every deployed chart.
func writeRepo(t *testing.T, valuesYAML string) string {
	t.Helper()
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ValuesFile), []byte(valuesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, chart := range DeployedCharts {
		dir := filepath.Join(root, chartsDir, chart)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("name: "+chart+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// everyChart is a values file that satisfies both load-time guards whatever
// DeployedCharts happens to hold, so a test about one thing does not trip the
// others. Built from the same constants the guards check against.
func everyChart(t *testing.T, extra map[string]string) string {
	t.Helper()
	var b strings.Builder
	for _, chart := range DeployedCharts {
		b.WriteString(chart + ":\n")
		if body, ok := extra[chart]; ok {
			b.WriteString(body)
			continue
		}
		b.WriteString(validSubtree(chart))
	}
	return b.String()
}

func validSubtree(chart string) string {
	switch chart {
	case ChartAccretionAPI:
		return "  config:\n" +
			"    database:\n" +
			"      existingSecret: " + APISecretName + "\n"
	case ChartAccretionCollector:
		return "  config:\n" +
			"    database:\n" +
			"      existingSecret: " + APISecretName + "\n" +
			"      existingSecretKey: " + DBPasswordKey + "\n" +
			"  redis:\n" +
			"    auth:\n" +
			"      existingSecret: " + RedisSecretName + "\n" +
			"      existingSecretPasswordKey: " + RedisPasswordKey + "\n" +
			"      existingSecretConfKey: " + RedisConfKey + "\n"
	case ChartAccretionDiscord:
		return validDiscordSubtree
	default:
		return "  image:\n    repository: accretion-api\n"
	}
}

const validDiscordSubtree = "  secret:\n" +
	"    create: false\n" +
	"    name: " + DiscordSecretName + "\n" +
	"  api:\n" +
	"    secretName: " + DiscordSecretName + "\n" +
	"    secretKey: api-key-" + DiscordIdentity + "\n"

func TestLoadChartValuesFromRepository(t *testing.T) {
	// Starts in this package's directory, so the walk up to the repository root
	// is the same one a deploy makes from pulumi/.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	vals, err := loadChartValuesFrom(wd)
	if err != nil {
		t.Fatalf("loading the real %s: %v", ValuesFile, err)
	}

	for _, chart := range DeployedCharts {
		path := vals.ChartPath(chart)
		if !filepath.IsAbs(path) {
			t.Errorf("chart path for %s is %q, want an absolute path", chart, path)
		}
		if !isFile(filepath.Join(path, "Chart.yaml")) {
			t.Errorf("chart path for %s does not hold a Chart.yaml: %s", chart, path)
		}
		if len(vals.Values(chart)) == 0 {
			t.Errorf("%s has no values in %s", chart, ValuesFile)
		}
	}
}

// The guard the ticket asks for: the top-level keys must be exactly the charts
// Pulumi deploys, so a typo is a loud failure rather than an empty map.
func TestTopLevelKeyGuard(t *testing.T) {
	t.Run("unknown key", func(t *testing.T) {
		root := writeRepo(t, everyChart(t, nil)+"accretion-collectors:\n  redis:\n    enabled: false\n")
		_, err := loadChartValuesFrom(root)
		if err == nil {
			t.Fatal("a top-level key naming no chart was accepted")
		}
		if !strings.Contains(err.Error(), "accretion-collectors") {
			t.Errorf("error does not name the offending key: %v", err)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		root := writeRepo(t, ChartAccretionAPI+":\n"+validSubtree(ChartAccretionAPI))
		_, err := loadChartValuesFrom(root)
		if err == nil {
			t.Fatal("a values file missing a deployed chart was accepted")
		}
		for _, chart := range DeployedCharts[1:] {
			if !strings.Contains(err.Error(), chart) {
				t.Errorf("error does not name missing chart %s: %v", chart, err)
			}
		}
	})

	t.Run("chart subtree is not a mapping", func(t *testing.T) {
		root := writeRepo(t, everyChart(t, map[string]string{ChartAccretionAPI: "  \"just a string\"\n"}))
		if _, err := loadChartValuesFrom(root); err == nil {
			t.Fatal("a scalar chart subtree was accepted")
		}
	})
}

func TestGuardNoCredentials(t *testing.T) {
	for _, field := range []string{"key", "hash"} {
		t.Run(field, func(t *testing.T) {
			roster := validSubtree(ChartAccretionAPI) +
				"  apiKeys:\n    - name: accretion-mcp\n      permissions: [\"read:memory\"]\n      " +
				field + ": \"deadbeef\"\n"
			root := writeRepo(t, everyChart(t, map[string]string{ChartAccretionAPI: roster}))

			_, err := loadChartValuesFrom(root)
			if err == nil {
				t.Fatalf("an apiKeys entry carrying %q was accepted into the central file", field)
			}
			if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "accretion-mcp") {
				t.Errorf("error names neither the field nor the identity: %v", err)
			}
		})
	}

	t.Run("roster without credentials is accepted", func(t *testing.T) {
		roster := validSubtree(ChartAccretionAPI) +
			"  apiKeys:\n    - name: accretion-mcp\n      permissions: [\"read:memory\"]\n"
		root := writeRepo(t, everyChart(t, map[string]string{ChartAccretionAPI: roster}))
		if _, err := loadChartValuesFrom(root); err != nil {
			t.Fatalf("a name-and-permissions roster was rejected: %v", err)
		}
	})
}

func TestFindRepoRoot(t *testing.T) {
	t.Run("walks up from a subdirectory", func(t *testing.T) {
		root := writeRepo(t, everyChart(t, nil))
		start := filepath.Join(root, "pulumi", "pkg", "config")
		if err := os.MkdirAll(start, 0o750); err != nil {
			t.Fatal(err)
		}

		found, err := findRepoRoot(start)
		if err != nil {
			t.Fatal(err)
		}
		if found != root {
			t.Errorf("found %q, want %q", found, root)
		}
	})

	// The caution that matters: a parent that merely has directories with the
	// right NAMES must not satisfy the walk, or the deploy silently configures
	// charts from some other tree.
	t.Run("a name-only match is not a repository root", func(t *testing.T) {
		decoy := t.TempDir()
		if err := os.MkdirAll(filepath.Join(decoy, "deploy"), 0o750); err != nil {
			t.Fatal(err)
		}
		for _, chart := range DeployedCharts {
			if err := os.MkdirAll(filepath.Join(decoy, chartsDir, chart), 0o750); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := findRepoRoot(decoy); err == nil {
			t.Fatal("a tree with the right directory names but no files was accepted as the root")
		}
	})

	t.Run("the walk is bounded and the error names where it started", func(t *testing.T) {
		start := t.TempDir()
		_, err := findRepoRoot(start)
		if err == nil {
			t.Fatal("the walk succeeded from a directory with no repository above it")
		}
		if !strings.Contains(err.Error(), start) {
			t.Errorf("error does not name the start directory: %v", err)
		}
	})
}

func TestMergeValues(t *testing.T) {
	base := map[string]any{
		"image":  map[string]any{"repository": "accretion-api", "pullPolicy": "Always"},
		"hosts":  []any{"a", "b"},
		"config": map[string]any{"database": map[string]any{"existingSecret": "accretion-api-secrets"}},
	}

	merged := MergeValues(base, map[string]any{
		"image":  map[string]any{"tag": "stable"},
		"hosts":  []any{"c"},
		"config": map[string]any{"database": map[string]any{"host": "postgresql"}},
	})

	image, _ := merged["image"].(map[string]any)
	if image["repository"] != "accretion-api" || image["tag"] != "stable" || image["pullPolicy"] != "Always" {
		t.Errorf("maps did not merge recursively: %v", image)
	}

	// Slices replace: a half-merged list of ingress hosts or identities would be
	// a value nobody wrote.
	if hosts, _ := merged["hosts"].([]any); len(hosts) != 1 || hosts[0] != "c" {
		t.Errorf("slice did not replace wholesale: %v", merged["hosts"])
	}

	db, _ := merged["config"].(map[string]any)["database"].(map[string]any)
	if db["existingSecret"] != "accretion-api-secrets" || db["host"] != "postgresql" {
		t.Errorf("nested map did not merge: %v", db)
	}

	// The loaded document is reused across releases, so merging must not reach
	// back into it.
	if baseImage, _ := base["image"].(map[string]any); baseImage["tag"] != nil {
		t.Error("MergeValues mutated its base")
	}
}

func TestResolveAPIKeyIdentities(t *testing.T) {
	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	roster := []any{
		map[string]any{"name": "accretion-mcp", "permissions": []any{"read:memory"}},
	}

	t.Run("pairs the roster with the configured digest", func(t *testing.T) {
		identities, err := ResolveAPIKeyIdentities(roster, map[string]string{"accretion-mcp": digest}, "prod")
		if err != nil {
			t.Fatal(err)
		}
		if len(identities) != 1 {
			t.Fatalf("got %d identities, want 1", len(identities))
		}
		id, _ := identities[0].(map[string]any)
		if id["hash"] != digest || id["name"] != "accretion-mcp" {
			t.Errorf("identity did not carry its name and digest: %v", id)
		}
		if _, leaked := id["key"]; leaked {
			t.Error("a plaintext key reached the chart values")
		}
	})

	t.Run("the roster is not mutated", func(t *testing.T) {
		if _, err := ResolveAPIKeyIdentities(roster, map[string]string{"accretion-mcp": digest}, "prod"); err != nil {
			t.Fatal(err)
		}
		if entry, _ := roster[0].(map[string]any); entry["hash"] != nil {
			t.Error("the loaded roster was mutated with a digest")
		}
	})

	// A missing digest must fail with the command that fixes it, naming the
	// stack. A placeholder would render a release that authenticates nobody.
	t.Run("a missing digest names the stack and the command", func(t *testing.T) {
		_, err := ResolveAPIKeyIdentities(roster, nil, "staging")
		if err == nil {
			t.Fatal("an identity with no configured digest was accepted")
		}
		for _, want := range []string{"accretion-mcp", "staging", "pulumi config set", "apiKeyHashes.accretion-mcp", "shasum -a 256"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
	})

	t.Run("a digest that is not 64 hex characters is refused", func(t *testing.T) {
		_, err := ResolveAPIKeyIdentities(roster, map[string]string{"accretion-mcp": "not-a-digest"}, "dev")
		if err == nil {
			t.Fatal("a malformed digest was accepted")
		}
		if !strings.Contains(err.Error(), "64 hex") {
			t.Errorf("error does not say what is wrong: %v", err)
		}
	})

	t.Run("an empty roster is refused", func(t *testing.T) {
		if _, err := ResolveAPIKeyIdentities([]any{}, nil, "dev"); err == nil {
			t.Fatal("an empty roster was accepted; the chart's own guard would reject the render")
		}
	})
}

// The exact combination cycle-1 review put into the committed file and watched
// pass: a live bot token as a literal env value, an authentication kill switch,
// and a live Redis password. Each must now be refused on its own.
func TestGuardRefusesCredentialsAndKillSwitches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chart   string
		subtree string
		wants   []string
	}{
		{
			name:  "a literal env value",
			chart: ChartAccretionDiscord,
			subtree: validDiscordSubtree +
				"  env:\n    - name: DISCORD_BOT_TOKEN\n      value: \"a-live-token\"\n",
			wants: []string{"env[0]", "DISCORD_BOT_TOKEN", "valueFrom"},
		},
		{
			name:    "the authentication kill switch",
			chart:   ChartAccretionAPI,
			subtree: validSubtree(ChartAccretionAPI) + "  disableAuthentication: true\n",
			wants:   []string{"disableAuthentication", "fail-closed"},
		},
		{
			name:  "a live Redis password",
			chart: ChartAccretionCollector,
			subtree: validSubtree(ChartAccretionCollector) +
				"      password: \"a-live-password\"\n",
			wants: []string{"redis.auth.password", "credential"},
		},
		{
			name:  "a live database password",
			chart: ChartAccretionAPI,
			subtree: validSubtree(ChartAccretionAPI) +
				"      password: \"a-live-password\"\n",
			wants: []string{"config.database.password", "credential"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeRepo(t, everyChart(t, map[string]string{tc.chart: tc.subtree}))

			_, err := loadChartValuesFrom(root)
			if err == nil {
				t.Fatalf("%s was accepted into the checked-in values file", tc.name)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error is missing %q: %v", want, err)
				}
			}
		})
	}
}

// disableAuthentication absent is the chart default and fine; an explicit false
// is fine too. Only a truthy value is refused.
func TestGuardAllowsExplicitlyDisabledKillSwitch(t *testing.T) {
	subtree := validSubtree(ChartAccretionAPI) + "  disableAuthentication: false\n"
	root := writeRepo(t, everyChart(t, map[string]string{ChartAccretionAPI: subtree}))
	if _, err := loadChartValuesFrom(root); err != nil {
		t.Fatalf("an explicit disableAuthentication: false was rejected: %v", err)
	}
}

// Every Secret name and key that appears in both the central file and
// secrets.go is checked against one constant, so renaming it on either side
// fails the load rather than leaving releases mounting a Secret nothing
// provisions -- #120's Finding 2, reintroduced silently.
func TestGuardSecretReferences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chart   string
		subtree string
		want    string
	}{
		{
			name:    "the API database Secret",
			chart:   ChartAccretionAPI,
			subtree: "  config:\n    database:\n      existingSecret: renamed-in-secrets-go\n",
			want:    APISecretName,
		},
		{
			name:  "the collector's database key",
			chart: ChartAccretionCollector,
			subtree: strings.Replace(validSubtree(ChartAccretionCollector),
				"existingSecretKey: "+DBPasswordKey, "existingSecretKey: password", 1),
			want: DBPasswordKey,
		},
		{
			name:  "the Redis Secret",
			chart: ChartAccretionCollector,
			subtree: strings.Replace(validSubtree(ChartAccretionCollector),
				"existingSecret: "+RedisSecretName, "existingSecret: renamed-in-secrets-go", 1),
			want: RedisSecretName,
		},
		{
			name:  "the bot's API-key entry",
			chart: ChartAccretionDiscord,
			subtree: strings.Replace(validDiscordSubtree,
				"secretKey: api-key-"+DiscordIdentity, "secretKey: api-key-accretion-api-client-keys", 1),
			want: DiscordAPIKeySecretKey(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeRepo(t, everyChart(t, map[string]string{tc.chart: tc.subtree}))

			_, err := loadChartValuesFrom(root)
			if err == nil {
				t.Fatalf("%s was allowed to disagree with the Secret Pulumi provisions", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say what it should be (%q): %v", tc.want, err)
			}
		})
	}
}

// The Redis Secret's two shapes come from one password, because the chart can
// no longer check that for itself once existingSecret is set: a `password` key
// that has drifted from the requirepass directive is a client-server mismatch
// nothing catches.
func TestRedisSecretDataIsSelfConsistent(t *testing.T) {
	data := RedisSecretData("s3cret with a space")

	if data[RedisPasswordKey] != "s3cret with a space" {
		t.Errorf("client password = %q", data[RedisPasswordKey])
	}
	if data[RedisConfKey] != `requirepass "s3cret with a space"` {
		t.Errorf("server directive = %q; a bare password with a space is a boot-time config error",
			data[RedisConfKey])
	}
	if len(data) != 2 {
		t.Errorf("got %d keys, want exactly the two the chart reads", len(data))
	}
}

// A missing chart must be diagnosed as a missing chart, not as a missing
// repository root, which is what it reported before.
func TestMissingChartIsNotReportedAsAMissingRoot(t *testing.T) {
	root := writeRepo(t, everyChart(t, nil))
	if err := os.Remove(filepath.Join(root, chartsDir, ChartAccretionDiscord, "Chart.yaml")); err != nil {
		t.Fatal(err)
	}

	_, err := loadChartValuesFrom(root)
	if err == nil {
		t.Fatal("a deployed chart that is not on disk was accepted")
	}
	if !strings.Contains(err.Error(), ChartAccretionDiscord) {
		t.Errorf("error does not name the missing chart: %v", err)
	}
	if strings.Contains(err.Error(), "could not find the repository root") {
		t.Errorf("a missing chart was misdiagnosed as a missing repository root: %v", err)
	}
}

// The residual class: a leaf the chart DOES declare, whose value is free text.
// api.url renders verbatim into a Deployment env value and the llm base_urls
// render into a ConfigMap, neither of which is a Secret.
func TestGuardRefusesCredentialsInURLs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subtree string
	}{
		{
			name: "a Deployment env value",
			// Inserted INTO the existing api: block. Appending a second one
			// would be a duplicate key and would trip a different guard, which
			// is how a fixture proves something other than what it claims.
			subtree: strings.Replace(validDiscordSubtree,
				"  api:\n", "  api:\n    url: \"https://bot:t0ken@accretion-api.svc:80\"\n", 1),
		},
		{
			name: "a ConfigMap base_url",
			subtree: validDiscordSubtree +
				"  config:\n    llm:\n      anthropic:\n        base_url: \"https://x:sk-live@gateway.example\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeRepo(t, everyChart(t, map[string]string{ChartAccretionDiscord: tc.subtree}))

			_, err := loadChartValuesFrom(root)
			if err == nil {
				t.Fatalf("a credential embedded in a URL was accepted (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), "secretKeyRef") {
				t.Errorf("error does not say what to do instead: %v", err)
			}
		})
	}

	// A URL without userinfo is ordinary configuration and must still load.
	subtree := validDiscordSubtree +
		"  config:\n    llm:\n      openai:\n        base_url: \"https://api.openai.com/v1\"\n"
	root := writeRepo(t, everyChart(t, map[string]string{ChartAccretionDiscord: subtree}))
	if _, err := loadChartValuesFrom(root); err != nil {
		t.Fatalf("an ordinary URL was rejected: %v", err)
	}
}
