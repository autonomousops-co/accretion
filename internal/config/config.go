package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

// UIConfig controls the browser-facing surface: the web UI, its static assets,
// and the OAuth routes that exist to log a human into it.
//
// It defaults to DISABLED. The primary interfaces are accretion-mcp and the
// Discord bot, both of which authenticate by API key, so the default deployment
// is API-only: /api/* behind those keys plus /api/health for the kubelet. That
// needs no browser reachability, no OAuth client, and no user whitelist.
// Enable it to serve the web interface.
type UIConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	Port    int  `yaml:"port" mapstructure:"port"`
}

// APIConfig controls the machine-facing listener: /api/* behind API keys, plus
// /api/health. This is what accretion-mcp, the Discord bot, and any HTTP client
// talk to. It is a separate listener from the UI so that exposing the API never
// exposes the browser surface — including /auth/*, the only unauthenticated
// non-health routes in the system.
type APIConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	Port    int  `yaml:"port" mapstructure:"port"`
	// WriteTimeout bounds the ENTIRE handler (Go measures it from
	// end-of-request-headers to end-of-response), so it must cover /api/chat's
	// non-streaming RAG flow of sequential LLM calls (embed -> time-window
	// extraction -> answer), which can exceed 15s on a cold local LLM. Empty ->
	// DefaultWriteTimeout. It lives here rather than on the UI, which serves
	// files and needs no such budget.
	WriteTimeout string `yaml:"write_timeout" mapstructure:"write_timeout"`
	// TLS terminates HTTPS on this listener in-process. Disabled by default.
	TLS TLSConfig `yaml:"tls" mapstructure:"tls"`
}

// TLSConfig configures in-process TLS termination on the API listener.
//
// It is fail-closed by construction: when Enabled is true the keypair is loaded
// and validated during server construction, before any listener goroutine
// starts, so a missing or malformed certificate is a startup error rather than
// a fatal inside a goroutine — and never a silent fall back to cleartext.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	CertFile string `yaml:"cert_file" mapstructure:"cert_file"`
	KeyFile  string `yaml:"key_file" mapstructure:"key_file"`
	// CAFile is the PEM bundle a CLIENT verifies this listener against. It is
	// not used to serve: the server needs only the keypair. It lives here
	// because the `healthcheck` subcommand and the in-cluster clients all need
	// the same answer to "what signed the API's certificate", and deriving it
	// from the same block that configures the certificate keeps the two from
	// drifting. Empty means verify against the system pool, which is correct
	// for a publicly-trusted certificate.
	CAFile string `yaml:"ca_file" mapstructure:"ca_file"`
	// MinVersion is the lowest TLS version accepted, as "1.2" or "1.3" (the
	// "TLS" prefix and a "v" are both tolerated). Empty -> DefaultTLSMinVersion.
	// Anything below 1.2 is refused rather than honored: the point of the knob
	// is to raise the floor, not to lower it.
	MinVersion string `yaml:"min_version" mapstructure:"min_version"`
}

// DefaultTLSMinVersion is the minimum TLS version used when api.tls.min_version
// is unset. TLS 1.2 is the floor: 1.0 and 1.1 are deprecated (RFC 8996) and Go
// will not negotiate them by default either.
const DefaultTLSMinVersion = tls.VersionTLS12

// GetMinVersion resolves MinVersion to a crypto/tls version constant. It
// returns an error rather than falling back for an unrecognized or
// below-floor value: silently serving a weaker floor than the operator
// configured is the failure this setting exists to prevent.
func (c *TLSConfig) GetMinVersion() (uint16, error) {
	raw := strings.TrimSpace(c.MinVersion)
	if raw == "" {
		return DefaultTLSMinVersion, nil
	}
	normalized := strings.ToLower(raw)
	normalized = strings.TrimPrefix(normalized, "tls")
	normalized = strings.TrimPrefix(normalized, "v")
	switch normalized {
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	case "1.0", "1.1":
		return 0, fmt.Errorf("api.tls.min_version %q is below the TLS 1.2 floor (RFC 8996 deprecates TLS 1.0 and 1.1)", raw)
	default:
		return 0, fmt.Errorf("api.tls.min_version %q is not a recognized TLS version: use \"1.2\" or \"1.3\"", raw)
	}
}

// ClientTLSConfig builds the *tls.Config a CLIENT should use to reach this
// listener: the configured CA when one is set, the system pool otherwise. It
// never disables verification.
//
// Returns (nil, nil) when TLS is disabled, so callers can hand the result
// straight to a transport and get cleartext behavior unchanged.
func (c *TLSConfig) ClientTLSConfig() (*tls.Config, error) {
	if !c.Enabled {
		return nil, nil
	}
	out := &tls.Config{MinVersion: DefaultTLSMinVersion}
	if c.CAFile == "" {
		return out, nil
	}
	pool, err := CertPoolFromFile(c.CAFile)
	if err != nil {
		return nil, err
	}
	out.RootCAs = pool
	return out, nil
}

// CertPoolFromFile reads a PEM bundle into a certificate pool.
//
// A file that exists but holds no certificate is an error, not an empty pool:
// an empty pool trusts nothing, so it would surface as an unrelated-looking
// handshake failure at the first request rather than as "the CA you configured
// is not a CA".
func CertPoolFromFile(path string) (*x509.CertPool, error) {
	// #nosec G304 -- path is the operator-supplied api.tls.ca_file. Reading a
	// configured file path is the whole purpose of this function, and the
	// content is parsed as PEM rather than executed or served.
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading CA file %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA file %q contains no PEM certificate", path)
	}
	return pool, nil
}

// Validate loads the configured keypair and resolves the minimum version,
// returning the *tls.Config the listener should use. Callers run this before
// starting any listener so that a bad keypair aborts startup.
//
// A disabled TLSConfig returns (nil, nil): no certificate is read and the
// listener stays cleartext.
func (c *TLSConfig) Validate() (*tls.Config, error) {
	if !c.Enabled {
		return nil, nil
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, fmt.Errorf("api.tls.enabled is true but cert_file and/or key_file is empty")
	}
	minVersion, err := c.GetMinVersion()
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS keypair (cert_file=%q key_file=%q): %w", c.CertFile, c.KeyFile, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
	}, nil
}

// ServerConfig holds settings shared by both listeners.
type ServerConfig struct {
	// Host is the bind address for the UI and API listeners alike.
	Host string `yaml:"host" mapstructure:"host"`
}

// DebugEnvVar opts into the configuration-loading debug output on stdout.
const DebugEnvVar = "ACCRETION_CONFIG_DEBUG"

// Debugf prints configuration-loading diagnostics, but only when
// ACCRETION_CONFIG_DEBUG is set.
//
// These lines used to print unconditionally on every config load. That was
// tolerable when the only loader was a long-lived server starting once; it is
// not now that `accretion-api healthcheck` loads the config on every liveness
// and readiness probe. At readiness/5s plus liveness/10s that was six lines per
// probe, about one line per second per pod, forever — and one of them restates
// the OAuth client ID in full, so a credential-adjacent value was being written
// to pod logs continuously.
//
// Opt-in rather than merely quiet-on-the-probe-path: the value of these lines is
// in diagnosing a config problem, which is a thing you go looking for, and
// nothing should have to print an identifier that often to be useful.
func Debugf(format string, args ...any) {
	if os.Getenv(DebugEnvVar) == "" {
		return
	}
	fmt.Printf(format, args...)
}

// DefaultWriteTimeout is the response write deadline used when api.write_timeout
// is unset. It must accommodate /api/chat's non-streaming RAG flow (embed ->
// time-window extraction -> answer) on a cold local LLM. The functional test client
// reads the same value (env ACCRETION_API_WRITE_TIMEOUT, else this default) so the
// client and server timeouts always agree.
const DefaultWriteTimeout = 120 * time.Second

// GetWriteTimeout returns the HTTP server write timeout, falling back to
// DefaultWriteTimeout when unset or invalid.
func (c *APIConfig) GetWriteTimeout() time.Duration {
	if c.WriteTimeout == "" {
		return DefaultWriteTimeout
	}
	d, err := time.ParseDuration(c.WriteTimeout)
	if err != nil || d <= 0 {
		return DefaultWriteTimeout
	}
	return d
}

// Config contains configuration for the server service
type Config struct {
	Server    ServerConfig    `yaml:"server" mapstructure:"server"`
	UI        UIConfig        `yaml:"ui" mapstructure:"ui"`
	API       APIConfig       `yaml:"api" mapstructure:"api"`
	Database  DatabaseConfig  `yaml:"database" mapstructure:"database"`
	LLM       LLMConfig       `yaml:"llm" mapstructure:"llm"`
	K8s       K8sConfig       `yaml:"k8s" mapstructure:"k8s"`
	Logging   LoggingConfig   `yaml:"logging" mapstructure:"logging"`
	Auth      AuthConfig      `yaml:"auth" mapstructure:"auth"`
	Tempo     TempoConfig     `yaml:"tempo" mapstructure:"tempo"`
	Telemetry TelemetryConfig `yaml:"telemetry" mapstructure:"telemetry"`
}

// DiscordConfig contains configuration for the Discord bot service
type DiscordConfig struct {
	Token     string          `yaml:"token" mapstructure:"token"`
	GuildID   string          `yaml:"guild_id" mapstructure:"guild_id"`
	Database  DatabaseConfig  `yaml:"database" mapstructure:"database"`
	LLM       LLMConfig       `yaml:"llm" mapstructure:"llm"`
	Logging   LoggingConfig   `yaml:"logging" mapstructure:"logging"`
	Tempo     TempoConfig     `yaml:"tempo" mapstructure:"tempo"`
	Telemetry TelemetryConfig `yaml:"telemetry" mapstructure:"telemetry"`
	API       APIClientConfig `yaml:"api" mapstructure:"api"`
}

// APIClientConfig contains connection details for reaching accretion-api. The
// Discord bot routes all cluster data access through this HTTP API rather than
// holding its own database connection.
//
// Distinct from APIConfig, which configures the API listener this dials.
type APIClientConfig struct {
	URL string `yaml:"url" mapstructure:"url"`
	Key string `yaml:"key" mapstructure:"key"`
	// CAFile verifies the API's certificate against a private CA. Empty means
	// the system pool, which is correct for a publicly-trusted certificate.
	CAFile string `yaml:"ca_file" mapstructure:"ca_file"`
	// InsecureSkipVerify is refused for a non-loopback URL by the client that
	// consumes it; see accretion.NewClientWithTLS.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" mapstructure:"insecure_skip_verify"`
}

// CollectorConfig contains configuration for the collector service
type CollectorConfig struct {
	LLM                    LLMConfig         `yaml:"llm" mapstructure:"llm"`
	Logging                LoggingConfig     `yaml:"logging" mapstructure:"logging"`
	WorkerConfig           WorkerConfig      `yaml:"worker" mapstructure:"worker"`
	ControllerWorkerPeriod string            `yaml:"controller_worker_period" mapstructure:"controller_worker_period"`
	Database               DatabaseConfig    `yaml:"database" mapstructure:"database"`
	K8s                    K8sConfig         `yaml:"k8s" mapstructure:"k8s"`
	Queue                  QueueConfig       `yaml:"queue" mapstructure:"queue"`
	Tempo                  TempoConfig       `yaml:"tempo" mapstructure:"tempo"`
	TraceWorker            TraceWorkerConfig `yaml:"trace_worker" mapstructure:"trace_worker"`

	// Namespaces is the unified include/exclude namespace-scoping model consumed
	// by every data source. It supersedes the per-source k8s.namespaces and
	// trace_worker.namespaces lists, which are still honored as back-compat
	// fallbacks by K8sNamespaceSelector / TraceNamespaceSelector.
	Namespaces NamespaceScopeConfig `yaml:"namespaces" mapstructure:"namespaces"`
	LogFilter  LogFilterConfig      `yaml:"log_filter" mapstructure:"log_filter"`
	Telemetry  TelemetryConfig      `yaml:"telemetry" mapstructure:"telemetry"`

	// Collectors selects which collectors the consolidated `status` and
	// `data-collector` processes start. It replaces "is this worker's Deployment
	// present?" as the way a kind is turned off, now that several kinds share
	// one process (#91, #92).
	Collectors CollectorsConfig `yaml:"collectors" mapstructure:"collectors"`

	ControllerWorkerCount int `yaml:"controller_worker_count" mapstructure:"controller_worker_count"`
}

// CollectorsConfig turns individual collectors on and off inside a shared
// process. Every kind defaults to enabled, so a config written before
// consolidation keeps collecting exactly what it collected before.
type CollectorsConfig struct {
	// Status collectors: the low-volume object-state kinds that share the
	// `status` process.
	PodStatus        CollectorToggle `yaml:"pod_status" mapstructure:"pod_status"`
	NodeStatus       CollectorToggle `yaml:"node_status" mapstructure:"node_status"`
	DeploymentStatus CollectorToggle `yaml:"deployment_status" mapstructure:"deployment_status"`
	HPAStatus        CollectorToggle `yaml:"hpa_status" mapstructure:"hpa_status"`
	EndpointHealth   CollectorToggle `yaml:"endpoint_health" mapstructure:"endpoint_health"`
	PVCStatus        CollectorToggle `yaml:"pvc_status" mapstructure:"pvc_status"`
	ConfigMap        CollectorToggle `yaml:"configmap" mapstructure:"configmap"`
	JobStatus        CollectorToggle `yaml:"job_status" mapstructure:"job_status"`

	// Data collectors: the higher-volume kinds that share the `data-collector`
	// process, any of which can be split back out into its own Deployment.
	Log    CollectorToggle `yaml:"log" mapstructure:"log"`
	Metric CollectorToggle `yaml:"metric" mapstructure:"metric"`
	Event  CollectorToggle `yaml:"event" mapstructure:"event"`
	Trace  CollectorToggle `yaml:"trace" mapstructure:"trace"`
}

// CollectorToggle is one collector's on/off switch.
//
// Enabled is a *bool rather than a bool so "absent" and "explicitly false" stay
// distinguishable: the zero value of a bool is false, which would silently
// disable every kind for any config written before these keys existed. A nil
// Enabled means "not configured", and IsEnabled treats that as on.
type CollectorToggle struct {
	Enabled *bool `yaml:"enabled" mapstructure:"enabled"`
}

// IsEnabled reports whether the collector should run. Absent means enabled.
func (c CollectorToggle) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// LogFilterConfig configures ingestion-side filtering of log lines, applied by
// the log worker BEFORE the (expensive) embedding call. High-frequency
// repetitive lines such as health checks, readiness/liveness probes and access
// logs are distinct events that dedup cannot bound, so they otherwise dominate
// embedding CPU and storage.
//
// Every field defaults to a no-op, so with the zero value the worker embeds and
// stores every non-empty line exactly as before — filtering is strictly opt-in.
type LogFilterConfig struct {
	// MinLevel drops any line whose parsed level is below this threshold.
	// Recognized values (case-insensitive), ordered by severity:
	// debug < info < warn < error. Empty ("") disables level filtering. Lines
	// whose level cannot be determined are never dropped by the threshold
	// (fail open).
	MinLevel string `yaml:"min_level" mapstructure:"min_level"`
	// Denylist is a list of regular expressions matched against the raw log
	// message; a line matching any pattern is dropped. Patterns are compiled
	// once at worker construction. Empty disables denylist filtering.
	Denylist []string `yaml:"denylist" mapstructure:"denylist"`
	// SampleRate probabilistically keeps only a fraction of the lines that pass
	// the denylist and level checks, in the open interval (0,1) — e.g. 0.1 keeps
	// ~10%. The decision is deterministic per line occurrence. Values <= 0 or
	// >= 1 disable sampling (keep everything).
	SampleRate float64 `yaml:"sample_rate" mapstructure:"sample_rate"`
}

// TraceWorkerConfig contains configuration for the trace worker.
type TraceWorkerConfig struct {
	ListenAddr string `yaml:"listen_addr" mapstructure:"listen_addr"`
	// Namespaces is the legacy allowlist of workload namespaces whose traces are
	// ingested into RAG.
	//
	// Deprecated: use the unified namespaces.traces.include instead. This field
	// is still honored as a back-compat fallback by TraceNamespaceSelector when
	// namespaces.traces.include is empty, keeping the legacy single-namespace
	// default working.
	Namespaces []string `yaml:"namespaces" mapstructure:"namespaces"`
}

// Kubernetes client rate-limit defaults. See K8sConfig.QPS.
const (
	DefaultK8sQPS   float32 = 50
	DefaultK8sBurst int     = 100
)

// RateLimits returns the configured client-go QPS and burst, falling back to
// the defaults when unset.
func (k K8sConfig) RateLimits() (float32, int) {
	qps, burst := k.QPS, k.Burst
	if qps <= 0 {
		qps = DefaultK8sQPS
	}
	if burst <= 0 {
		burst = DefaultK8sBurst
	}
	return qps, burst
}

type DatabaseConfig struct {
	Host     string `yaml:"host" mapstructure:"host"`
	User     string `yaml:"user" mapstructure:"user"`
	Password string `yaml:"password" mapstructure:"password"`
	Database string `yaml:"database" mapstructure:"database"`
	SSLMode  string `yaml:"sslmode" mapstructure:"sslmode"`
	Port     int    `yaml:"port" mapstructure:"port"`
}

type LLMConfig struct {
	Provider       string          `yaml:"provider" mapstructure:"provider"`
	OpenAI         OpenAIConfig    `yaml:"openai" mapstructure:"openai"`
	Ollama         OllamaConfig    `yaml:"ollama" mapstructure:"ollama"`
	Anthropic      AnthropicConfig `yaml:"anthropic" mapstructure:"anthropic"`
	EmbeddingModel string          `yaml:"embedding_model" mapstructure:"embedding_model"`
	ChatModel      string          `yaml:"chat_model" mapstructure:"chat_model"`
}

type OpenAIConfig struct {
	APIKey  string `yaml:"api_key" mapstructure:"api_key"`
	BaseURL string `yaml:"base_url" mapstructure:"base_url"`
}

type OllamaConfig struct {
	BaseURL string `yaml:"base_url" mapstructure:"base_url"`
}

type AnthropicConfig struct {
	APIKey  string `yaml:"api_key" mapstructure:"api_key"`
	BaseURL string `yaml:"base_url" mapstructure:"base_url"`
}

type K8sConfig struct {
	ConfigPath string `yaml:"config_path" mapstructure:"config_path"`
	// PollInterval is the poll period of the collectors that still poll (the
	// event collector) and, since #96, the informer resync period of the eight
	// status collectors: at least once per interval every watched object is
	// re-delivered from the informer cache and re-evaluated, which is the
	// guarantee a poll gave, while a change between resyncs is written as it
	// happens. The key kept its name so existing values files and
	// ACCRETION_K8S_POLL_INTERVAL keep meaning something.
	PollInterval string `yaml:"poll_interval" mapstructure:"poll_interval"`
	// Namespaces is the legacy list of namespaces polled sources iterate.
	//
	// Deprecated: use the unified namespaces.include instead. This field is still
	// honored as a back-compat fallback by K8sNamespaceSelector when
	// namespaces.include is empty.
	Namespaces []string `yaml:"namespaces" mapstructure:"namespaces"`

	// QPS and Burst are the client-side rate limits on calls to the Kubernetes
	// API server.
	//
	// These matter far more since consolidation (#91, #92). client-go's own
	// defaults are 5 QPS / 10 burst PER CLIENT, which was ~60 QPS aggregate
	// when twelve worker pods each had their own client. One shared client at
	// the library default would be a 12x cut in API throughput. The status
	// collectors no longer poll (#96: one LIST and one WATCH per resource, then
	// near-zero steady state), but the event and metric collectors still do,
	// the log and metric work runs through this client, and every informer's
	// initial LIST and re-LIST after a dropped watch draws on the same budget.
	//
	// 50/100 is the usual controller default and is what these fall back to
	// when unset. Zero or negative means "use the default", not "no limit".
	QPS   float32 `yaml:"qps" mapstructure:"qps"`
	Burst int     `yaml:"burst" mapstructure:"burst"`
}

type QueueConfig struct {
	// Mode selects how log and metric work reaches the collectors:
	//
	//   "inprocess" -- the default. The dispatcher runs inside the collector
	//     process and hands work to its own collectors over channels. No Redis,
	//     no separate controller Deployment.
	//   "redis" -- work is published to Redis and consumed by workers that may
	//     be in other pods. This is what sharding across horizontally scaled
	//     collectors requires, and the chart selects it automatically once any
	//     collector is scaled beyond one replica or has autoscaling on (#93).
	//
	// An empty value means "inprocess": the controller and Redis exist to shard
	// work across scaled workers, and a default install has nothing to shard.
	Mode          string `yaml:"mode" mapstructure:"mode"`
	RedisAddr     string `yaml:"redis_addr" mapstructure:"redis_addr"`
	RedisPassword string `yaml:"redis_password" mapstructure:"redis_password"`
	RedisDB       int    `yaml:"redis_db" mapstructure:"redis_db"`
}

// Queue modes. See QueueConfig.Mode.
const (
	QueueModeInProcess = "inprocess"
	QueueModeRedis     = "redis"
)

// UsesRedis reports whether work is routed through Redis rather than delivered
// in-process. Absent means in-process.
func (q QueueConfig) UsesRedis() bool { return q.Mode == QueueModeRedis }

// Validate rejects a queue mode that is neither recognized value, so a typo
// fails at startup rather than silently selecting the in-process path and
// leaving a scaled fleet with a dispatcher per pod.
func (q QueueConfig) Validate() error {
	switch q.Mode {
	case "", QueueModeInProcess, QueueModeRedis:
		return nil
	default:
		return fmt.Errorf("unknown queue mode %q; valid values are %q and %q",
			q.Mode, QueueModeInProcess, QueueModeRedis)
	}
}

type WorkerConfig struct {
	MetricsPollInterval string `yaml:"metrics_poll_interval" mapstructure:"metrics_poll_interval"`
}

type LoggingConfig struct {
	Level             string `yaml:"level" mapstructure:"level"`
	Format            string `yaml:"format" mapstructure:"format"`
	FrontendLevel     string `yaml:"frontend_level" mapstructure:"frontend_level"`
	DisableCaller     bool   `yaml:"disable_caller" mapstructure:"disable_caller"`
	DisableStacktrace bool   `yaml:"disable_stacktrace" mapstructure:"disable_stacktrace"`
}

type TelemetryConfig struct {
	Endpoint    string  `yaml:"endpoint" mapstructure:"endpoint"`
	ServiceName string  `yaml:"service_name" mapstructure:"service_name"`
	SampleRate  float64 `yaml:"sample_rate" mapstructure:"sample_rate"`
	Enabled     bool    `yaml:"enabled" mapstructure:"enabled"`
	Insecure    bool    `yaml:"insecure" mapstructure:"insecure"`
	LogsEnabled bool    `yaml:"logs_enabled" mapstructure:"logs_enabled"`
}

// TempoConfig contains configuration for the Tempo trace backend.
type TempoConfig struct {
	BaseURL string `yaml:"base_url" mapstructure:"base_url"`
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
}

type AuthConfig struct {
	OAuth          OAuthConfig `yaml:"oauth" mapstructure:"oauth"`
	SessionKey     string      `yaml:"session_key" mapstructure:"session_key"`
	CookieDomain   string      `yaml:"cookie_domain" mapstructure:"cookie_domain"`
	AdminEmails    []string    `yaml:"admin_emails" mapstructure:"admin_emails"`
	AdminDomains   []string    `yaml:"admin_domains" mapstructure:"admin_domains"`
	AllowedEmails  []string    `yaml:"allowed_emails" mapstructure:"allowed_emails"`
	AllowedDomains []string    `yaml:"allowed_domains" mapstructure:"allowed_domains"`
	// APIKeyIdentities are named, scoped API keys identified by the SHA-256 hash
	// of the key (the plaintext key is never stored in config). This is the only
	// API-key mechanism: every caller has its own entry, so requests are
	// attributable and each key is revocable on its own. Delivered through the
	// mounted config.yaml — viper has no env binding for a struct list.
	APIKeyIdentities []APIKeyIdentity `yaml:"api_key_identities" mapstructure:"api_key_identities"`
	CookieSecure     bool             `yaml:"cookie_secure" mapstructure:"cookie_secure"`
}

// APIKeyIdentity is a named, scoped API key. Only the hex-encoded SHA-256 hash
// of the key is configured; callers present the raw key as a Bearer token.
type APIKeyIdentity struct {
	Name        string   `yaml:"name" mapstructure:"name"`
	Hash        string   `yaml:"hash" mapstructure:"hash"`
	Roles       []string `yaml:"roles" mapstructure:"roles"`
	Permissions []string `yaml:"permissions" mapstructure:"permissions"`
}

type OAuthConfig struct {
	Provider     string   `yaml:"provider" mapstructure:"provider"`
	ClientID     string   `yaml:"client_id" mapstructure:"client_id"`
	ClientSecret string   `yaml:"client_secret" mapstructure:"client_secret"`
	RedirectURL  string   `yaml:"redirect_url" mapstructure:"redirect_url"`
	AuthURL      string   `yaml:"auth_url" mapstructure:"auth_url"`
	TokenURL     string   `yaml:"token_url" mapstructure:"token_url"`
	UserInfoURL  string   `yaml:"user_info_url" mapstructure:"user_info_url"`
	Scopes       []string `yaml:"scopes" mapstructure:"scopes"`
}

// Load loads configuration for the server service
func Load() (*Config, error) {
	// Set defaults
	setDefaults()

	// Bind environment variables
	bindEnvVars()

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Debug logging for auth configuration
	Debugf("DEBUG: Auth config loaded - SessionKey present=%v (len=%d)\n",
		cfg.Auth.SessionKey != "", len(cfg.Auth.SessionKey))
	Debugf("DEBUG: OAuth config - Provider=%s, ClientID=%s, RedirectURL=%s\n",
		cfg.Auth.OAuth.Provider, cfg.Auth.OAuth.ClientID, cfg.Auth.OAuth.RedirectURL)
	Debugf("DEBUG: Cookie config - Domain=%s, Secure=%v\n",
		cfg.Auth.CookieDomain, cfg.Auth.CookieSecure)

	return &cfg, nil
}

// LoadCollector loads configuration for the collector service
func LoadCollector() (*CollectorConfig, error) {
	// Set defaults
	setDefaults()

	// Bind environment variables
	bindEnvVars()

	var cfg CollectorConfig
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal collector config: %w", err)
	}

	return &cfg, nil
}

// LoadDiscord loads configuration for the Discord bot service
func LoadDiscord() (*DiscordConfig, error) {
	setDefaults()
	bindEnvVars()

	var cfg DiscordConfig
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal discord config: %w", err)
	}

	if token := os.Getenv("DISCORD_BOT_TOKEN"); token != "" {
		cfg.Token = token
	}
	if apiURL := os.Getenv("ACCRETION_API_URL"); apiURL != "" {
		cfg.API.URL = apiURL
	}
	if apiKey := os.Getenv("ACCRETION_API_KEY"); apiKey != "" {
		cfg.API.Key = apiKey
	}

	return &cfg, nil
}

func setDefaults() {
	viper.SetDefault("ui.enabled", false)
	viper.SetDefault("ui.port", 8080)
	viper.SetDefault("api.enabled", true)
	viper.SetDefault("api.port", 41337)
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("api.write_timeout", DefaultWriteTimeout.String())

	// TLS on the API listener is ON by default. See TLSConfig: enabling it
	// without a loadable keypair is a startup error, never a fall back to
	// cleartext. Set api.tls.enabled=false for an environment that terminates
	// TLS elsewhere.
	viper.SetDefault("api.tls.enabled", true)
	viper.SetDefault("api.tls.cert_file", "")
	viper.SetDefault("api.tls.key_file", "")
	viper.SetDefault("api.tls.ca_file", "")
	viper.SetDefault("api.tls.min_version", "1.2")

	viper.SetDefault("database.host", "localhost")
	viper.SetDefault("database.port", 5432)
	viper.SetDefault("database.user", "postgres")
	// No default for database.password (#129). A well-known default is a
	// credential that works, so an install that forgot to set one connects
	// silently instead of failing. Absent ACCRETION_DATABASE_PASSWORD the value
	// stays empty and the connection fails loudly, which is the outcome we want.
	viper.SetDefault("database.database", "accretion")
	viper.SetDefault("database.sslmode", "disable")

	viper.SetDefault("llm.provider", "ollama")
	viper.SetDefault("llm.openai.base_url", "https://api.openai.com/v1")

	viper.SetDefault("llm.ollama.base_url", "http://localhost:11434")

	viper.SetDefault("llm.embedding_model", "nomic-embed-text:latest")
	viper.SetDefault("llm.chat_model", "llama3:8b")

	viper.SetDefault("k8s.config_path", "")
	viper.SetDefault("k8s.namespaces", []string{"default"})
	viper.SetDefault("k8s.poll_interval", "30s")
	// See K8sConfig.QPS: client-go's own 5/10 default is far too low now that
	// one client serves every collector in a process.
	viper.SetDefault("k8s.qps", DefaultK8sQPS)
	viper.SetDefault("k8s.burst", DefaultK8sBurst)

	// The legacy trace_worker.namespaces key has NO default. Both the
	// accretion-collector chart and config.example.yaml set
	// namespaces.traces.include, which TraceNamespaceSelector prefers, so the
	// legacy key is only read by a hand-written config that omits both. With no
	// default, Include is then empty, which Allows reads as every namespace --
	// the same scope the shipped configs request with "*". The self namespace is
	// still excluded by TraceNamespaceSelector, so the anti-feedback-loop control
	// holds. An operator who wants a narrower trace scope must set it explicitly.

	// Unified namespace scoping. Include/exclude default to empty so the legacy
	// k8s.namespaces / trace_worker.namespaces lists above remain the effective
	// source unless operators opt into the new keys.
	//
	// self_namespace is accretion's own namespace; it is excluded from the log
	// selector only (LogNamespaceSelector), never from K8sNamespaceSelector, so
	// metrics/status can observe it while the log worker never ingests its own
	// log lines (the amplification loop). The default is "" (self-exclusion off):
	// accretion's release namespace is only known at deploy time, so the chart injects
	// it via {{ .Release.Namespace }}. A hardcoded default here would target the
	// wrong namespace on a legacy-only deployment in a different release
	// namespace. See issue #145.
	viper.SetDefault("namespaces.self_namespace", "")

	// How often label/"*" scoping is re-resolved so namespaces created after
	// startup are picked up (#152). 5m trades a modest LIST rate for scope that
	// tracks namespace churn within one interval. Name-only configs resolve
	// nothing and ignore this entirely; "0" pins the startup snapshot.
	viper.SetDefault("namespaces.refresh_interval", "5m")

	// Log filter defaults are a no-op so existing behavior (embed and store
	// every non-empty line) is unchanged unless explicitly opted in.
	viper.SetDefault("log_filter.min_level", "")
	viper.SetDefault("log_filter.denylist", []string{})
	viper.SetDefault("log_filter.sample_rate", 0.0)

	viper.SetDefault("logging.level", "info")
	viper.SetDefault("logging.format", "console")
	viper.SetDefault("logging.frontend_level", "info")
	viper.SetDefault("logging.disable_caller", false)
	viper.SetDefault("logging.disable_stacktrace", true)

	// Telemetry defaults
	viper.SetDefault("telemetry.enabled", false)
	viper.SetDefault("telemetry.endpoint", "localhost:4317")
	viper.SetDefault("telemetry.service_name", "accretion-api")
	viper.SetDefault("telemetry.sample_rate", 1.0)
	viper.SetDefault("telemetry.insecure", true)
	viper.SetDefault("telemetry.logs_enabled", true)

	// Tempo defaults
	viper.SetDefault("tempo.base_url", "http://localhost:3200")
	viper.SetDefault("tempo.enabled", false)

	// Auth defaults
	viper.SetDefault("auth.oauth.provider", "google")
	viper.SetDefault("auth.oauth.client_id", "")
	viper.SetDefault("auth.oauth.client_secret", "")
	viper.SetDefault("auth.oauth.redirect_url", "http://localhost:8080/auth/callback")
	viper.SetDefault("auth.oauth.scopes", []string{"openid", "profile", "email"})
	viper.SetDefault("auth.session_key", "")
	viper.SetDefault("auth.cookie_secure", true)
	viper.SetDefault("auth.cookie_domain", "")
	viper.SetDefault("auth.admin_emails", []string{})
	viper.SetDefault("auth.admin_domains", []string{})
	viper.SetDefault("auth.allowed_emails", []string{})
	viper.SetDefault("auth.allowed_domains", []string{})
	viper.SetDefault("auth.api_key_identities", []APIKeyIdentity{})
}

func bindEnvVars() {
	viper.SetEnvPrefix("ACCRETION")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	viper.AutomaticEnv()

	// Bind specific env vars
	// These binds are required, not decorative — AutomaticEnv alone does not reach
	// Unmarshal for a key with no default, so dropping them silently empties the
	// keys. See TestLLMKeysRequireExplicitBind.
	viper.BindEnv("llm.openai.api_key", "ACCRETION_LLM_OPENAI_API_KEY")
	viper.BindEnv("llm.anthropic.api_key", "ACCRETION_LLM_ANTHROPIC_API_KEY")

	// Same reasoning applies here (#112): the accretion-collector chart no
	// longer writes database.password or queue.redis_password into the
	// rendered ConfigMap. Both arrive only through these env vars, sourced
	// from the chart's postgresql-secret / redis-secret via secretKeyRef. See
	// TestCollectorSecretsRequireExplicitBind.
	viper.BindEnv("database.password", "ACCRETION_DATABASE_PASSWORD")
	viper.BindEnv("queue.redis_password", "ACCRETION_QUEUE_REDIS_PASSWORD")

	// Bind logging env vars
	viper.BindEnv("logging.frontend_level", "ACCRETION_LOGGING_FRONTEND_LEVEL")

	// Bind OAuth env vars
	viper.BindEnv("auth.oauth.client_id", "OAUTH_CLIENT_ID")
	viper.BindEnv("auth.oauth.client_secret", "OAUTH_CLIENT_SECRET")
	viper.BindEnv("auth.oauth.redirect_url", "ACCRETION_AUTH_OAUTH_REDIRECT_URL")
	viper.BindEnv("auth.session_key", "SESSION_KEY")
	viper.BindEnv("auth.admin_emails", "ACCRETION_AUTH_ADMIN_EMAILS")
	viper.BindEnv("auth.admin_domains", "ACCRETION_AUTH_ADMIN_DOMAINS")
	viper.BindEnv("auth.allowed_emails", "ACCRETION_AUTH_ALLOWED_EMAILS")
	viper.BindEnv("auth.allowed_domains", "ACCRETION_AUTH_ALLOWED_DOMAINS")
	viper.BindEnv("auth.cookie_secure", "ACCRETION_AUTH_COOKIE_SECURE")
	viper.BindEnv("auth.cookie_domain", "ACCRETION_AUTH_COOKIE_DOMAIN")

	// Bind Discord env vars
	viper.BindEnv("token", "DISCORD_BOT_TOKEN")
	viper.BindEnv("guild_id", "DISCORD_GUILD_ID")

	// Bind accretion-api client env vars (used by the Discord bot)
	viper.BindEnv("api.url", "ACCRETION_API_URL")
	viper.BindEnv("api.key", "ACCRETION_API_KEY")
	viper.BindEnv("api.ca_file", "ACCRETION_API_CA_FILE")
	viper.BindEnv("api.insecure_skip_verify", "ACCRETION_API_INSECURE_SKIP_VERIFY")
}

// OmittedSecret names one secret GenerateDefault deliberately does not write,
// and the environment variable that supplies it instead.
type OmittedSecret struct {
	// Key is the dotted config key left empty, e.g. "auth.session_key".
	Key string
	// EnvVar is the variable the value arrives in, e.g. "SESSION_KEY".
	EnvVar string
}

// OmittedSecrets is every secret-bearing key GenerateDefault leaves empty. It
// exists so `accretion generate config` can name the variables to set rather
// than leaving the operator to discover them from a failed startup, and it
// lives next to GenerateDefault because a list kept anywhere else drifts from
// the struct literal it describes. TestGenerateDefaultOmitsSecrets walks the
// generated YAML and fails if any key here comes out non-empty, so adding a
// secret field to Config without adding it here is caught rather than shipped.
var OmittedSecrets = []OmittedSecret{
	{Key: "auth.session_key", EnvVar: "SESSION_KEY"},
	{Key: "auth.oauth.client_secret", EnvVar: "OAUTH_CLIENT_SECRET"},
	{Key: "database.password", EnvVar: "ACCRETION_DATABASE_PASSWORD"},
	{Key: "llm.openai.api_key", EnvVar: "ACCRETION_LLM_OPENAI_API_KEY"},
	{Key: "llm.anthropic.api_key", EnvVar: "ACCRETION_LLM_ANTHROPIC_API_KEY"},
}

// googleOAuthTokenURL is Google's public OAuth token endpoint. It is a
// documented URL every OAuth client redirects through, not a credential.
//
// G101 flags it only because the name it is assigned to contains "token".
//
// It is hoisted out of the OAuthConfig literal below so the suppression sits on
// one line holding one public URL, where it can be checked at a glance, rather
// than on the struct that used to hold ClientSecret. The annotation cannot be
// narrowed any further in place: gosec attaches #nosec to the ENCLOSING node,
// so written on the TokenURL field line it silences G101 for the whole
// composite literal. Measured, not assumed.
//
// This buys scope, not detection. G101 does not report a hardcoded ClientSecret
// in that literal either way — also measured: it stays silent with no
// annotation present at all. What actually guards the omission is
// TestGenerateDefaultOmitsSecrets, which reads the generated YAML back and
// fails on any non-empty secret-named field. Keeping the struct free of a
// blanket suppression only means a future G101 finding there is not
// pre-silenced.
const googleOAuthTokenURL = "https://oauth2.googleapis.com/token" // #nosec G101 -- public endpoint, not a credential

// GenerateDefault writes a starting config to outputFile.
//
// It carries no secret (#129). Every key in OmittedSecrets is written empty and
// sourced from the environment instead — the gitignored .env locally, a
// Kubernetes Secret read through secretKeyRef in-cluster — because those binds
// already exist, so this removes a second, weaker source rather than adding
// work. #112 made exactly this move for the collector's rendered ConfigMap.
//
// Keeping secrets hidden outranks a config that runs unmodified: ease of use is
// about non-secret configuration — ports, namespaces, providers, structure —
// not a pre-populated credential. A file born with a live session signing key
// is one `git add -A` away from being committed, and that key signs and
// encrypts the OAuth session cookie, so leaking it is an auth bypass.
//
// Do NOT "fix" the empty session key with a fake or fixed placeholder. A
// predictable signing key is worse than an absent one, because it would work.
// Absent a key, NewAuthManager fails closed and names the variable to set,
// which is the correct outcome, not a defect to paper over.
func GenerateDefault(outputFile string) error {
	cfg := &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
		},
		// API on, UI off: the default deployment is machine-facing.
		API: APIConfig{Enabled: true, Port: 41337},
		UI:  UIConfig{Enabled: false, Port: 8080},
		// Password is deliberately absent; it arrives as
		// ACCRETION_DATABASE_PASSWORD. See OmittedSecrets.
		Database: DatabaseConfig{
			Host:     "localhost",
			Port:     5432,
			User:     "postgres",
			Database: "accretion",
			SSLMode:  "disable",
		},
		LLM: LLMConfig{
			Provider: "ollama",
			OpenAI: OpenAIConfig{
				BaseURL: "https://api.openai.com/v1",
			},
			Ollama: OllamaConfig{
				BaseURL: "http://localhost:11434",
			},
			EmbeddingModel: "nomic-embed-text:latest",
			ChatModel:      "llama3:8b",
		},
		K8s: K8sConfig{
			ConfigPath:   "",
			Namespaces:   []string{"default"},
			PollInterval: "30s",
		},
		Logging: LoggingConfig{
			Level:             "info",
			Format:            "json",
			FrontendLevel:     "info",
			DisableCaller:     false,
			DisableStacktrace: true,
		},
		Telemetry: TelemetryConfig{
			Enabled:     false,
			Endpoint:    "localhost:4317",
			ServiceName: "accretion-api",
			SampleRate:  1.0,
			Insecure:    true,
		},
		// ClientSecret and SessionKey are deliberately absent; they arrive as
		// OAUTH_CLIENT_SECRET and SESSION_KEY. See OmittedSecrets. ClientID stays:
		// an OAuth client id is visible in every browser redirect, so it is
		// ordinary configuration, not a credential.
		Auth: AuthConfig{
			OAuth: OAuthConfig{
				Provider:    "google",
				ClientID:    "your-google-client-id",
				RedirectURL: "http://localhost:8080/auth/callback",
				Scopes:      []string{"openid", "profile", "email"},
				AuthURL:     "https://accounts.google.com/o/oauth2/auth",
				TokenURL:    googleOAuthTokenURL,
				UserInfoURL: "https://www.googleapis.com/oauth2/v2/userinfo",
			},
			CookieDomain:   "",
			CookieSecure:   true,
			AdminEmails:    []string{"admin@example.com"},
			AdminDomains:   []string{"yourdomain.com"},
			AllowedEmails:  []string{"user1@example.com", "user2@contractor.com"},
			AllowedDomains: []string{"alloweddomain.com"},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// The file no longer leaves here secret-bearing, but it is the file an
	// operator edits, and the first thing edited into a config is usually the
	// value that did not come with it. Staying 0600 keeps that copy unreadable
	// by other users on the machine rather than depending on them not doing it.
	// Write it 0600 via a temp file in the same directory and rename into place:
	// every failure before the rename leaves the target untouched with the temp
	// file cleaned up, so there is no half-written config to reason about, and
	// the rename replaces a pre-existing file's mode along with its contents.
	tmp, err := os.CreateTemp(filepath.Dir(outputFile), ".accretion-config-*")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	defer tmp.Close()           // no-op once closed below

	// CreateTemp already creates 0600; make that a guarantee, not a default.
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("securing temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}
	return os.Rename(tmp.Name(), outputFile)
}

// GetMetricsPollInterval returns the metrics poll interval as a duration
func (c *CollectorConfig) GetMetricsPollInterval() (time.Duration, error) {
	if c.WorkerConfig.MetricsPollInterval == "" {
		return 30 * time.Second, nil // Default 30 seconds
	}
	return time.ParseDuration(c.WorkerConfig.MetricsPollInterval)
}

const sanitizedValue = "***"

// Sanitize returns a copy of the config with sensitive fields masked
func (c *Config) Sanitize() Config {
	sanitized := *c
	sanitized.Database.Password = sanitizedValue
	sanitized.LLM.OpenAI.APIKey = sanitizedValue
	sanitized.LLM.Anthropic.APIKey = sanitizedValue
	sanitized.Auth.OAuth.ClientSecret = sanitizedValue
	sanitized.Auth.SessionKey = sanitizedValue
	return sanitized
}

// Sanitize returns a copy of the discord config with sensitive fields masked
func (c *DiscordConfig) Sanitize() DiscordConfig {
	sanitized := *c
	sanitized.Token = sanitizedValue
	sanitized.Database.Password = sanitizedValue
	sanitized.LLM.OpenAI.APIKey = sanitizedValue
	sanitized.LLM.Anthropic.APIKey = sanitizedValue
	// API.Key is the bot's own accretion-api credential. It was missing here
	// until it was found in a running pod's log: cmd/discord.go logs this at
	// INFO on every startup, so the key was written in plaintext to the pod log
	// on each restart while every other secret beside it showed ***.
	sanitized.API.Key = sanitizedValue
	return sanitized
}

// Sanitize returns a copy of the collector config with sensitive fields masked
func (c *CollectorConfig) Sanitize() CollectorConfig {
	sanitized := *c
	sanitized.Database.Password = sanitizedValue
	sanitized.Queue.RedisPassword = sanitizedValue
	sanitized.LLM.OpenAI.APIKey = sanitizedValue
	sanitized.LLM.Anthropic.APIKey = sanitizedValue
	return sanitized
}
