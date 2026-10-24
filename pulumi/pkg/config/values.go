package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Chart names, one constant per chart this program deploys. helm.go builds its
// releases from these and DeployedCharts below is keyed on the same list, so
// the guard in loadChartValuesFrom and the code that actually creates the
// Releases can never be talking about different sets of charts.
const (
	ChartAccretionAPI       = "accretion-api"
	ChartAccretionCollector = "accretion-collector"
	ChartAccretionDiscord   = "accretion-discord"
)

// Secrets Pulumi provisions, and the keys inside them.
//
// Every name here appears on BOTH sides: deploy/pulumi.yaml points a chart at
// it, and pulumi/pkg/kubernetes/secrets.go creates it. They are constants so
// the two sides cannot be renamed independently, and guardSecretReferences
// checks the central file against them at load time. Without that, renaming a
// name in secrets.go leaves releases mounting a Secret nothing provisions --
// valid manifests and pods that never start, which is #120's Finding 2
// reintroduced silently.
const (
	// APISecretName holds the database password that BOTH the API and the
	// collector read, under DBPasswordKey. The collector chart's own default
	// key name is "password", so it has to be told this one explicitly.
	APISecretName = "accretion-api-secrets"
	DBPasswordKey = "db-password"

	// RedisSecretName carries the collector queue's password in the TWO shapes
	// that chart needs, because redis-server has no env-var form of
	// requirepass: the raw password for clients, and a config-file directive
	// for the server. secrets.go derives both from one value, which is the
	// consistency burden the chart's existingSecret path transfers to whoever
	// provisions it. Without it the release takes the chart's default password,
	// which is checked into this repository.
	RedisSecretName  = "accretion-collector-redis-secrets"
	RedisPasswordKey = "password"
	RedisConfKey     = "redis.conf"

	// DiscordSecretName carries the bot's credentials, including its own
	// accretion-api key. The chart's default api.secretName points at
	// accretion-api's client-keys Secret, which that chart only emits for
	// identities declared with a PLAINTEXT key; every identity on this path is
	// hash-only, so that Secret never exists here and the deliberately
	// non-optional secretKeyRef would fail the pod at rollout.
	DiscordSecretName = "accretion-discord-secrets"
	DiscordIdentity   = "accretion-discord"
)

// DiscordAPIKeySecretKey is the key inside DiscordSecretName holding the bot's
// own API key. The accretion-api chart names client-key entries
// api-key-<identity>; this follows that convention so both halves read alike.
func DiscordAPIKeySecretKey() string { return "api-key-" + DiscordIdentity }

// DiscordSecretKeys is every key DiscordSecretName carries. ONE list, used by
// both halves: secrets.go builds the Secret from it, and the render check
// asserts the bot's manifests reference exactly these, in both directions.
//
// `make secrets-pulumi-check` covers these too since #135. It used to be unable
// to, for two independent reasons: it compared scripts/make-secret.sh against
// the accretion-api Secret and none of these was a key that script emitted, and
// its extraction matched literal `"key": pulumi.String(...)` pairs, which
// building the Secret from this list deliberately does not produce. The first
// reason ended when #135 gave the script ownership of the bot token and the
// model keys -- the Tiltfile had been passing them to Helm as plaintext --set,
// which put them in the Helm release -- so the gate now reads this list
// directly instead, and the render check below is no longer the only one.
func DiscordSecretKeys() []string {
	return []string{
		"discord-bot-token",
		"anthropic-api-key",
		"openai-api-key",
		DiscordAPIKeySecretKey(),
	}
}

// RedisSecretData is the content of RedisSecretName, both shapes from ONE
// password so they cannot disagree -- the chart can no longer check that for
// itself once existingSecret is set.
func RedisSecretData(password string) map[string]string {
	return map[string]string{
		RedisPasswordKey: password,
		// A config-file directive, not the raw password. %q quotes it the way
		// redis.conf's own string syntax expects.
		RedisConfKey: fmt.Sprintf("requirepass %q", password),
	}
}

// DefaultDatabaseSSLMode is the floor every stack gets unless it raises it.
//
// "require" encrypts the connection without demanding a CA bundle. The stronger
// verify-ca and verify-full additionally authenticate the server, which needs
// sslrootcert -- and NEITHER CHART TEMPLATES THAT KEY today (only sslmode
// reaches the ConfigMap), so a stack set to verify-full would encrypt, fail to
// find a CA, and refuse to connect. Raising the floor further is a chart change
// first, recorded as a follow-up.
const DefaultDatabaseSSLMode = "require"

// encryptedSSLModes is the ALLOWLIST of libpq sslmode values that actually
// guarantee an encrypted connection. Exported through EncryptedSSLModes so the
// render check asserts against this list rather than restating it.
var encryptedSSLModes = []string{"require", "verify-ca", "verify-full"}

// EncryptedSSLModes returns the sslmode values a stack may use.
func EncryptedSSLModes() []string { return slices.Clone(encryptedSSLModes) }

// ValidateDatabaseSSLMode enforces the invariant that no stack sends database
// traffic in the clear.
//
// AN ALLOWLIST, NOT A DENYLIST. It refused "disable" and empty and accepted
// everything else, which let through the two libpq modes that permit plaintext:
// "allow" sends plaintext FIRST and negotiates TLS only if the server refuses,
// and "prefer" falls back to plaintext silently whenever TLS is not offered.
// internal/database/database.go builds a literal libpq DSN, so those semantics
// apply exactly. A typo like "requre", and "REQUIRE" (libpq is case-sensitive),
// also passed and would have failed at connection time instead of deploy time.
//
// "prefer" is the specific trap, and it is why this has to be an allowlist. An
// operator who hits the loud failure this floor produces reaches for the
// setting that makes the error go away -- and that setting restores exactly the
// defect the floor exists to prevent, silently. The error below names it.
func ValidateDatabaseSSLMode(mode string) error {
	if slices.Contains(encryptedSSLModes, mode) {
		return nil
	}

	accepted := strings.Join(encryptedSSLModes, ", ")
	switch mode {
	case "":
		return fmt.Errorf("database sslMode is empty; leave `dbSslMode` unset for the default %q, "+
			"or set it to one of: %s", DefaultDatabaseSSLMode, accepted)
	case "disable":
		return fmt.Errorf("database sslMode is %q, which sends every query, and the password "+
			"authenticating it, in the clear. Set `dbSslMode` to one of: %s", mode, accepted)
	case "prefer", "allow":
		return fmt.Errorf("database sslMode is %q, which does NOT guarantee encryption: %q sends "+
			"plaintext first and upgrades only if the server refuses it, and %q falls back to "+
			"plaintext silently whenever TLS is not offered. If %q will not connect, the database is "+
			"not serving TLS -- fix that rather than reaching for this. Accepted: %s",
			mode, "allow", "prefer", DefaultDatabaseSSLMode, accepted)
	default:
		return fmt.Errorf("database sslMode %q is not a mode that guarantees encryption. libpq is "+
			"case-sensitive and does not accept abbreviations, so a near-miss connects at runtime or "+
			"not at all rather than failing here. Accepted: %s", mode, accepted)
	}
}

// urlUserinfo matches the userinfo form of a URL: scheme://<something>@host.
//
// The residual class neither other guard covers. credentialPaths enumerates
// paths that are credentials, and UndeclaredPaths catches keys the chart never
// declares -- what falls between them is a DECLARED leaf whose value is free
// text. api.url renders verbatim into a Deployment env value and the
// config.llm.*.base_url keys render into a ConfigMap in cleartext, and a
// credential embedded in URL userinfo is a standard way for a token to reach a
// gateway. Matching the shape rather than enumerating the paths means a leaf
// added later is covered without anyone remembering this exists.
var urlUserinfo = regexp.MustCompile(`://[^/@\s]*@`)

// DeployedCharts is the complete set of charts this program knows how to
// deploy, and the exact set of top-level keys ValuesFile must declare.
//
// Deliberately the set it CAN deploy, not the subset a given stack enables: a
// stack that turns the collector off has not made the central file wrong, and
// keying the guard on the enabled subset would mean the file silently stops
// being checked for whatever the stack switched off.
var DeployedCharts = []string{
	ChartAccretionAPI,
	ChartAccretionCollector,
	ChartAccretionDiscord,
}

// ValuesFile is the central Helm values file, relative to the repository root.
// Nested by chart name at the top level; each chart's subtree is passed to that
// chart's release and to no other.
const ValuesFile = "deploy/pulumi.yaml"

const chartsDir = "charts"

// maxRootWalk bounds the search in findRepoRoot. The root is one level above
// the Pulumi project directory, so this is already generous; it exists so a
// program started somewhere unexpected fails with the message below instead of
// walking to / and matching whatever it finds on the way.
const maxRootWalk = 8

// ChartValues is the parsed central values file, plus the repository root it
// was found in — the same root the chart paths are built from, so the values
// and the charts they configure cannot come from different checkouts.
type ChartValues struct {
	charts map[string]map[string]any
	root   string
}

// LoadChartValues finds the repository root from the working directory, reads
// the central values file and checks it declares exactly the charts this
// program deploys.
func LoadChartValues() (*ChartValues, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining the working directory: %w", err)
	}
	return loadChartValuesFrom(wd)
}

func loadChartValuesFrom(start string) (*ChartValues, error) {
	root, err := findRepoRoot(start)
	if err != nil {
		return nil, err
	}
	if chartErr := chartPathsExist(root); chartErr != nil {
		return nil, chartErr
	}

	path := filepath.Join(root, ValuesFile)
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the located repository root
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var doc map[string]any
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	charts, err := chartSubtrees(path, doc)
	if err != nil {
		return nil, err
	}
	if err := guardNoCredentials(path, charts); err != nil {
		return nil, err
	}
	if err := guardSecretReferences(path, charts); err != nil {
		return nil, err
	}

	return &ChartValues{charts: charts, root: root}, nil
}

// chartSubtrees enforces the top-level-key guard: the set of keys in the values
// file must equal DeployedCharts exactly. A key naming no chart is a typo whose
// settings would reach nothing, and a missing key is a chart deploying on bare
// defaults; both are the silent-empty-map failure this file exists to prevent,
// so both are refused by name.
func chartSubtrees(path string, doc map[string]any) (map[string]map[string]any, error) {
	declared := make(map[string]bool, len(doc))
	for k := range doc {
		declared[k] = true
	}

	var missing, unknown []string
	for _, chart := range DeployedCharts {
		if !declared[chart] {
			missing = append(missing, chart)
		}
	}
	for k := range declared {
		if !slices.Contains(DeployedCharts, k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(missing)
	slices.Sort(unknown)

	if len(missing) > 0 || len(unknown) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s: top-level keys must be exactly the charts Pulumi deploys (%s).",
			path, strings.Join(DeployedCharts, ", "))
		if len(missing) > 0 {
			fmt.Fprintf(&b, " Missing: %s.", strings.Join(missing, ", "))
		}
		if len(unknown) > 0 {
			fmt.Fprintf(&b, " Not a chart this program deploys: %s.", strings.Join(unknown, ", "))
		}
		return nil, fmt.Errorf("%s", b.String())
	}

	charts := make(map[string]map[string]any, len(doc))
	for chart, sub := range doc {
		m, ok := sub.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %q must be a mapping of Helm values, got %T", path, chart, sub)
		}
		charts[chart] = m
	}
	return charts, nil
}

// Path segments repeated across the value paths below. Constants only to
// satisfy goconst; they are Helm value keys, not behavior.
const (
	keyAuth     = "auth"
	keyConfig   = "config"
	keyDatabase = "database"
	keyRedis    = "redis"
	keySecret   = "secret"
)

// credentialPaths are value paths that must never appear in the central file,
// checked against every chart because a path absent from a chart costs nothing
// to check and a path added to one later is covered without anybody
// remembering to come back here.
//
// Each is a shape the CHARTS WOULD ACCEPT, which is the only kind worth
// refusing -- a comment saying "no secrets here" stops nobody. In order: the
// database password both charts read, the Redis queue password, and the three
// values the Discord chart renders its own Secret from.
var credentialPaths = [][]string{
	{keyConfig, keyDatabase, "password"},
	{keyRedis, keyAuth, "password"},
	{keySecret, "botToken"},
	{keySecret, "anthropicApiKey"},
	{keySecret, "openaiApiKey"},
}

// guardNoCredentials refuses credential material, and authentication kill
// switches, in the central file.
//
// The file is checked in, so the rule that keeps secrets out of env.example and
// .envrc applies to it unchanged -- and this repository is going public. Cycle
// 1 review demonstrated the previous version was not airtight: a live bot token
// in `env`, a live Redis password, and disableAuthentication all passed at once.
func guardNoCredentials(path string, charts map[string]map[string]any) error {
	for _, chart := range DeployedCharts {
		values := charts[chart]

		for _, credential := range credentialPaths {
			if value, found := lookupPath(values, credential); found && value != "" && value != nil {
				return fmt.Errorf(
					"%s: %s.%s is a credential. It reaches the pod from the Secret Pulumi provisions, "+
						"never from this file",
					path, chart, strings.Join(credential, "."))
			}
		}

		// An `env` entry with a literal `value` renders straight into the
		// Deployment as plaintext. Only valueFrom is acceptable here.
		if err := guardEnvValues(path, chart, values); err != nil {
			return err
		}

		// Any free-text leaf that carries a credential in URL userinfo.
		if err := guardURLUserinfo(path, chart, "", values); err != nil {
			return err
		}

		// Not a credential but strictly worse than one: it passes
		// --disable-authentication to the server AND switches off the chart's
		// fail-closed guard on an empty apiKeys, so it turns the whole
		// authenticated surface off silently. Absent is fine (the chart
		// defaults it false); present must be an explicit false.
		if enabled, found := values["disableAuthentication"]; found && enabled != false {
			return fmt.Errorf(
				"%s: %s.disableAuthentication is %v. It disables authentication outright AND defeats "+
					"the chart's fail-closed guard on an empty apiKeys. Remove it, or set it explicitly false",
				path, chart, enabled)
		}
	}

	return guardAPIKeyRoster(path, charts[ChartAccretionAPI])
}

// guardURLUserinfo walks every string leaf, including inside lists, looking for
// a credential smuggled into a URL.
func guardURLUserinfo(path, chart, prefix string, node any) error {
	switch typed := node.(type) {
	case map[string]any:
		for key, child := range typed {
			where := key
			if prefix != "" {
				where = prefix + "." + key
			}
			if err := guardURLUserinfo(path, chart, where, child); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range typed {
			if err := guardURLUserinfo(path, chart, fmt.Sprintf("%s[%d]", prefix, i), child); err != nil {
				return err
			}
		}
	case string:
		if urlUserinfo.MatchString(typed) {
			return fmt.Errorf(
				"%s: %s.%s embeds credentials in a URL. These values render verbatim into a Deployment "+
					"or a ConfigMap, neither of which is a Secret -- supply the credential by "+
					"valueFrom.secretKeyRef instead",
				path, chart, prefix)
		}
	}
	return nil
}

func guardEnvValues(path, chart string, values map[string]any) error {
	entries, _ := values["env"].([]any)
	for i, entry := range entries {
		variable, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if _, found := variable["value"]; found {
			return fmt.Errorf(
				"%s: %s.env[%d] (%v) sets a literal `value`, which renders into the Deployment in "+
					"plaintext. Use valueFrom.secretKeyRef against the Secret Pulumi provisions",
				path, chart, i, variable["name"])
		}
	}
	return nil
}

// guardAPIKeyRoster keeps the roster to names and permissions.
//
// `key` is a plaintext credential. `hash` is not itself secret -- the chart
// renders it into a ConfigMap -- but it is per-stack material whose single
// source is stack config, and two sources for one digest is how a verifier and
// its client drift apart.
func guardAPIKeyRoster(path string, api map[string]any) error {
	roster, ok := api["apiKeys"].([]any)
	if !ok {
		return nil
	}
	for i, entry := range roster {
		identity, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"key", "hash"} {
			if _, found := identity[field]; !found {
				continue
			}
			return fmt.Errorf(
				"%s: %s.apiKeys[%d] (%v) carries %q. The central file declares the identity ROSTER only "+
					"(name and permissions); set the digest with "+
					"`pulumi config set --secret --path 'apiKeyHashes.%v' <sha256>`",
				path, ChartAccretionAPI, i, identity["name"], field, identity["name"])
		}
	}
	return nil
}

// lookupPath walks a dotted path of map keys, reporting whether it exists.
func lookupPath(values map[string]any, path []string) (any, bool) {
	var current any = values
	for _, key := range path {
		node, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = node[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// guardSecretReferences checks every chart is pointed at the Secret Pulumi
// actually provisions.
//
// Each of these is a name that must agree across two files, and a disagreement
// renders perfectly valid manifests that simply name something absent, so the
// pod only fails at rollout.
//
// Rendering is not blind to all of it -- the bot's Secret KEYS are caught by
// the render check's bidirectional comparison against DiscordSecretKeys, which
// review demonstrated. What rendering cannot see is whether a referenced Secret
// is one Pulumi creates, since nothing in the manifests says so; that is what
// this guard adds, and for the database Secret nothing else covers it at all
// (`make secrets-pulumi-check` compares scripts/make-secret.sh against the API
// Secret's keys and never looks at what the charts reference).
func guardSecretReferences(path string, charts map[string]map[string]any) error {
	// The database password, on both charts that read it. The collector also
	// has to name the key, because its own default is "password".
	for _, chart := range []string{ChartAccretionAPI, ChartAccretionCollector} {
		if err := wantValue(path, chart, charts[chart],
			[]string{keyConfig, keyDatabase, "existingSecret"}, APISecretName); err != nil {
			return err
		}
	}
	if err := wantValue(path, ChartAccretionCollector, charts[ChartAccretionCollector],
		[]string{keyConfig, keyDatabase, "existingSecretKey"}, DBPasswordKey); err != nil {
		return err
	}

	// The Redis queue password. Both key names are stated rather than left to
	// the chart's defaults, so a default change cannot quietly repoint them.
	collector := charts[ChartAccretionCollector]
	for _, want := range []struct {
		value string
		path  []string
	}{
		{RedisSecretName, []string{keyRedis, keyAuth, "existingSecret"}},
		{RedisPasswordKey, []string{keyRedis, keyAuth, "existingSecretPasswordKey"}},
		{RedisConfKey, []string{keyRedis, keyAuth, "existingSecretConfKey"}},
	} {
		if err := wantValue(path, ChartAccretionCollector, collector, want.path, want.value); err != nil {
			return err
		}
	}

	// The bot: its own API key, and the Secret it and its model keys come from.
	discord := charts[ChartAccretionDiscord]
	secretValues, _ := discord[keySecret].(map[string]any)
	if create, ok := secretValues["create"].(bool); !ok || create {
		return fmt.Errorf(
			"%s: %s.secret.create must be false. Pulumi provisions %s itself; letting the chart create "+
				"its own Secret would require the bot token to travel as a value in this file",
			path, ChartAccretionDiscord, DiscordSecretName)
	}
	for _, want := range []struct {
		value string
		path  []string
	}{
		{DiscordSecretName, []string{keySecret, "name"}},
		{DiscordSecretName, []string{"api", "secretName"}},
		{DiscordAPIKeySecretKey(), []string{"api", "secretKey"}},
	} {
		if err := wantValue(path, ChartAccretionDiscord, discord, want.path, want.value); err != nil {
			return err
		}
	}
	return nil
}

func wantValue(path, chart string, values map[string]any, valuePath []string, want string) error {
	got, _ := lookupPath(values, valuePath)
	if got != want {
		return fmt.Errorf("%s: %s.%s is %v, want %q -- the Secret Pulumi provisions",
			path, chart, strings.Join(valuePath, "."), got, want)
	}
	return nil
}

// RosterExcluding drops identities from the central roster by name.
//
// The roster declares every caller of accretion-api, but a caller that is not
// deployed is not a caller. Requiring a digest for the Discord bot on a stack
// that does not run it would make an operator configure a credential that
// authenticates nothing, which is precisely how placeholder digests find their
// way into stack config.
func RosterExcluding(roster any, names ...string) any {
	entries, ok := roster.([]any)
	if !ok {
		return roster
	}
	kept := make([]any, 0, len(entries))
	for _, entry := range entries {
		id, ok := entry.(map[string]any)
		if ok {
			if name, _ := id["name"].(string); slices.Contains(names, name) {
				continue
			}
		}
		kept = append(kept, entry)
	}
	return kept
}

// findRepoRoot walks up from start looking for the repository root.
//
// The root is identified by content, not by name: it must hold the central
// values FILE and a charts directory. Matching on directory names alone would
// let the walk succeed in some parent that happens to contain a "charts" or
// "deploy" directory, which is the silent-wrong-answer this whole file exists
// to eliminate. The walk is bounded by maxRootWalk so a program started
// somewhere unexpected reports the failure below rather than climbing to the
// filesystem root.
//
// Deliberately does NOT require a Chart.yaml per deployed chart. It did, which
// meant a missing or misnamed chart was reported as a missing repository root
// -- the wrong diagnosis for the wrong problem. Chart presence is checked
// separately, by chartPathsExist, which can name the chart.
//
// This is what replaced the CWD-relative "./charts/accretion-api" chart path.
// That path resolved against the working directory of whoever invoked pulumi —
// pulumi/, since scripts/deploy.sh cds there — and there is no pulumi/charts,
// so no release could ever render. Absolute paths built from this root do not
// care where the program was started.
func findRepoRoot(start string) (string, error) {
	dir := start
	for range maxRootWalk + 1 {
		if isRepoRoot(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf(
		"could not find the repository root: searched %s and up to %d parent directories for one "+
			"holding both %s and a %s/ directory",
		start, maxRootWalk, ValuesFile, chartsDir)
}

func isRepoRoot(dir string) bool {
	if !isFile(filepath.Join(dir, ValuesFile)) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, chartsDir))
	return err == nil && info.IsDir()
}

// chartPathsExist reports a chart this program deploys that is not on disk,
// separately from locating the root so the error names the chart.
func chartPathsExist(root string) error {
	for _, chart := range DeployedCharts {
		manifest := filepath.Join(root, chartsDir, chart, "Chart.yaml")
		if !isFile(manifest) {
			return fmt.Errorf("chart %q is deployed by this program but %s does not exist", chart, manifest)
		}
	}
	return nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// ChartPath is the absolute path to a chart's directory. Absolute deliberately:
// see findRepoRoot.
func (v *ChartValues) ChartPath(chart string) string {
	return filepath.Join(v.root, chartsDir, chart)
}

// Values is the chart's subtree of the central file, deep-copied so a caller
// merging its deploy-time values into it cannot mutate the loaded document and
// leak those values into another release.
func (v *ChartValues) Values(chart string) map[string]any {
	return deepCopyMap(v.charts[chart])
}

// setDigestCommand is the command an operator runs to configure one identity's
// digest. Shared by both failure paths below so the instruction cannot drift
// from itself.
const setDigestCommand = "    pulumi config set --stack %s --secret --path 'apiKeyHashes.%s' " +
	"\"$(printf '%%s' \"$KEY\" | shasum -a 256 | cut -d' ' -f1)\""

// ChartDefaults reads a chart's own values.yaml: the value surface the chart
// declares.
func (v *ChartValues) ChartDefaults(chart string) (map[string]any, error) {
	path := filepath.Join(v.ChartPath(chart), "values.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the located repository root
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var declared map[string]any
	if err := yaml.Unmarshal(raw, &declared); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return declared, nil
}

// UndeclaredPaths returns the dotted paths in values that the chart's own
// values.yaml does not declare.
//
// THE SILENT-KEY PROBLEM, stated once here because several checks exist to
// catch it and they all refer back to this: Helm accepts unknown value keys
// without complaint, so a key that was renamed, or was never right, configures
// nothing and reports nothing. There is no error, no warning, and no diff --
// the release simply takes the chart's defaults. The collector's values map
// named logWorker and metricWorker after those Deployments had been
// consolidated away, and every deploy quietly took chart defaults until someone
// noticed by eye.
//
// Two checks stand against it, because they catch different halves: this one
// compares the keys a release SETS against the keys its chart DECLARES, and the
// render check (pulumi/pkg/kubernetes/render_test.go) asserts the settings
// actually arrive in the manifests.
//
// Descends into maps only. A list's elements are data, not a declared schema,
// and a key the chart declares as an empty map is the chart saying "free-form"
// (annotations, nodeSelector), so neither is descended into.
func UndeclaredPaths(values, declared map[string]any) []string {
	var out []string
	undeclaredPathsInto(&out, "", values, declared)
	slices.Sort(out)
	return out
}

func undeclaredPathsInto(out *[]string, prefix string, values, declared map[string]any) {
	for key, value := range values {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}

		declaredValue, ok := declared[key]
		if !ok {
			*out = append(*out, path)
			continue
		}

		sub, isMap := value.(map[string]any)
		declaredSub, declaredIsMap := declaredValue.(map[string]any)
		if isMap && declaredIsMap && len(declaredSub) > 0 {
			undeclaredPathsInto(out, path, sub, declaredSub)
		}
	}
}

// MergeValues returns a deep copy of base with override merged into it. Maps
// merge recursively; every other kind of value, slices included, replaces
// wholesale — a partially merged list of ingress hosts or API-key identities
// would be a value nobody wrote.
func MergeValues(base, override map[string]any) map[string]any {
	merged := deepCopyMap(base)
	for k, v := range override {
		if sub, ok := v.(map[string]any); ok {
			if existing, ok := merged[k].(map[string]any); ok {
				merged[k] = MergeValues(existing, sub)
				continue
			}
		}
		merged[k] = deepCopyValue(v)
	}
	return merged
}

// ResolveAPIKeyIdentities pairs each identity in the central roster with its
// SHA-256 digest from stack config, producing the `apiKeys` list the chart
// expects.
//
// The roster (name and permissions) is the same for every stack and lives in
// the central file; the digest is per-stack and lives in stack config, so no
// credential material is checked in. An identity with no digest configured is
// refused HERE, with the command to fix it, rather than passed on: the chart
// would reject a missing hash with a message about a values file the operator
// never wrote, and a placeholder digest would render a release that
// authenticates nobody while looking healthy.
func ResolveAPIKeyIdentities(roster any, hashes map[string]string, stack string) ([]any, error) {
	entries, ok := roster.([]any)
	if !ok || len(entries) == 0 {
		return nil, fmt.Errorf(
			"%s: %s.apiKeys is empty. The chart refuses to render an authenticated deployment with no "+
				"identities, so every caller (MCP client, Discord bot, CI) must be declared there",
			ValuesFile, ChartAccretionAPI)
	}

	identities := make([]any, 0, len(entries))
	for i, entry := range entries {
		id, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %s.apiKeys[%d] must be a mapping, got %T", ValuesFile, ChartAccretionAPI, i, entry)
		}
		name, _ := id["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("%s: %s.apiKeys[%d] has no name", ValuesFile, ChartAccretionAPI, i)
		}

		hash := hashes[name]
		if hash == "" {
			return nil, fmt.Errorf(
				"no API-key digest configured for identity %q on stack %q. Set it with:\n"+setDigestCommand,
				name, stack, stack, name)
		}
		if !isHexDigest(hash) {
			return nil, fmt.Errorf(
				"the API-key digest configured for identity %q on stack %q is not 64 hex characters. "+
					"It must be the SHA-256 of the key itself, not the key:\n"+setDigestCommand,
				name, stack, stack, name)
		}

		resolved := deepCopyMap(id)
		resolved["hash"] = hash
		identities = append(identities, resolved)
	}
	return identities, nil
}

func isHexDigest(s string) bool {
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == sha256.Size
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}
