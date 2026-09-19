package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

// clearAccretionEnv removes all ACCRETION_ prefixed and explicitly bound env vars for test isolation.
func clearAccretionEnv(t *testing.T) {
	t.Helper()
	for _, env := range os.Environ() {
		if idx := strings.IndexByte(env, '='); idx > 0 {
			key := env[:idx]
			if strings.HasPrefix(key, "ACCRETION_") {
				t.Setenv(key, "")
				os.Unsetenv(key)
			}
		}
	}
	for _, key := range []string{"ACCRETION_LLM_OPENAI_API_KEY", "OPENAI_API_KEY", "ACCRETION_LLM_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", "OAUTH_CLIENT_ID", "OAUTH_CLIENT_SECRET", "SESSION_KEY"} {
		if _, ok := os.LookupEnv(key); ok {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}

// TestLoadDefaults tests loading configuration with default values.
func TestLoadDefaults(t *testing.T) {
	// Reset viper and clear env vars to ensure clean state
	viper.Reset()
	clearAccretionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load default config: %v", err)
	}

	// Test server defaults
	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("Expected server host '0.0.0.0', got '%s'", cfg.Server.Host)
	}
	if cfg.API.Port != 41337 {
		t.Errorf("Expected api port 41337, got %d", cfg.API.Port)
	}

	// Test database defaults
	if cfg.Database.Host != "localhost" {
		t.Errorf("Expected database host 'localhost', got '%s'", cfg.Database.Host)
	}
	if cfg.Database.Port != 5432 {
		t.Errorf("Expected database port 5432, got %d", cfg.Database.Port)
	}
	if cfg.Database.User != "postgres" {
		t.Errorf("Expected database user 'postgres', got '%s'", cfg.Database.User)
	}
	if cfg.Database.Database != "accretion" {
		t.Errorf("Expected database name 'accretion', got '%s'", cfg.Database.Database)
	}
	// There is deliberately no default password (#129). A well-known one is a
	// credential that works, so an install that set none would connect silently
	// instead of failing where someone can see it.
	if cfg.Database.Password != "" {
		t.Errorf("Expected no default database password, got %q; it must come from ACCRETION_DATABASE_PASSWORD",
			cfg.Database.Password)
	}

	// Test LLM defaults
	if cfg.LLM.Provider != "ollama" {
		t.Errorf("Expected LLM provider 'ollama', got '%s'", cfg.LLM.Provider)
	}
	if cfg.LLM.OpenAI.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("Expected OpenAI base URL 'https://api.openai.com/v1', got '%s'", cfg.LLM.OpenAI.BaseURL)
	}
	if cfg.LLM.Ollama.BaseURL != "http://localhost:11434" {
		t.Errorf("Expected Ollama base URL 'http://localhost:11434', got '%s'", cfg.LLM.Ollama.BaseURL)
	}

	// Test K8s defaults
	expectedNS := []string{"default"}
	if !reflect.DeepEqual(cfg.K8s.Namespaces, expectedNS) {
		t.Errorf("Expected K8s namespaces %v, got %v", expectedNS, cfg.K8s.Namespaces)
	}
	if cfg.K8s.PollInterval != "30s" {
		t.Errorf("Expected K8s poll interval '30s', got '%s'", cfg.K8s.PollInterval)
	}

	// Test logging defaults
	if cfg.Logging.Level != "info" {
		t.Errorf("Expected logging level 'info', got '%s'", cfg.Logging.Level)
	}
}

// TestLoadCollectorNamespaceDefaults verifies the collector's unified namespace
// scoping resolves correctly from defaults: polled sources fall back to the
// legacy k8s.namespaces (default) list and the trace worker, having no legacy
// default to fall back on, admits every namespace.
func TestLoadCollectorNamespaceDefaults(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)

	cfg, err := LoadCollector()
	if err != nil {
		t.Fatalf("Failed to load collector config: %v", err)
	}

	k8sSel := cfg.K8sNamespaceSelector()
	if !reflect.DeepEqual(k8sSel.Resolve(), []string{"default"}) {
		t.Errorf("Expected polled namespaces [default], got %v", k8sSel.Resolve())
	}

	// No legacy default remains, so an unconfigured trace scope admits every
	// namespace. The self namespace is excluded separately by
	// TraceNamespaceSelector, which is what keeps the feedback loop closed.
	traceSel := cfg.TraceNamespaceSelector()
	if !traceSel.Allows("default") {
		t.Error("Expected trace worker to allow default with no include configured")
	}
	if !traceSel.Allows("some-other-namespace") {
		t.Error("Expected trace worker to allow any namespace with no include configured")
	}
}

// TestLoadCollectorNamespaceBackCompat verifies that existing YAML using the
// legacy k8s.namespaces and trace_worker.namespaces keys keeps working unchanged.
func TestLoadCollectorNamespaceBackCompat(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)

	configContent := `
k8s:
  namespaces:
    - "production"
    - "staging"
  poll_interval: "15s"

trace_worker:
  namespaces:
    - "dkjr"
    - "team-a"
`
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "collector-config.yaml")
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	viper.SetConfigFile(configFile)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	cfg, err := LoadCollector()
	if err != nil {
		t.Fatalf("Failed to load collector config: %v", err)
	}

	if got := cfg.K8sNamespaceSelector().Resolve(); !reflect.DeepEqual(got, []string{"production", "staging"}) {
		t.Errorf("Expected legacy k8s.namespaces honored, got %v", got)
	}

	traceSel := cfg.TraceNamespaceSelector()
	if !traceSel.Allows("dkjr") || !traceSel.Allows("team-a") {
		t.Error("Expected legacy trace_worker.namespaces [dkjr, team-a] to be allowed")
	}
	if traceSel.Allows("other") {
		t.Error("Expected namespace outside the legacy trace allowlist to be rejected")
	}
}

// TestLoadCollectorNamespaceLabelSelectors verifies that the label-selector and
// "*" sentinel fields (#146) deserialize from YAML via mapstructure.
func TestLoadCollectorNamespaceLabelSelectors(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)

	configContent := `
namespaces:
  include:
    - "*"
    - "dkjr"
  exclude:
    - "kube-system"
  include_labels:
    - "env=prod"
    - "team=payments"
  exclude_labels:
    - "tier=infra"
  traces:
    include:
      - "dkjr"
    include_labels:
      - "team=payments"
    exclude_labels:
      - "tier=infra"
`
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "collector-config.yaml")
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	viper.SetConfigFile(configFile)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	cfg, err := LoadCollector()
	if err != nil {
		t.Fatalf("Failed to load collector config: %v", err)
	}

	base := cfg.Namespaces
	if !reflect.DeepEqual(base.Include, []string{"*", "dkjr"}) {
		t.Errorf("base include = %v, want [* dkjr]", base.Include)
	}
	if !reflect.DeepEqual(base.IncludeLabels, []string{"env=prod", "team=payments"}) {
		t.Errorf("base include_labels = %v, want [env=prod team=payments]", base.IncludeLabels)
	}
	if !reflect.DeepEqual(base.ExcludeLabels, []string{"tier=infra"}) {
		t.Errorf("base exclude_labels = %v, want [tier=infra]", base.ExcludeLabels)
	}

	traces := cfg.Namespaces.Traces
	if !reflect.DeepEqual(traces.IncludeLabels, []string{"team=payments"}) {
		t.Errorf("traces include_labels = %v, want [team=payments]", traces.IncludeLabels)
	}
	if !reflect.DeepEqual(traces.ExcludeLabels, []string{"tier=infra"}) {
		t.Errorf("traces exclude_labels = %v, want [tier=infra]", traces.ExcludeLabels)
	}

	// The label selectors flow through the effective selector helpers, so the
	// unified builders carry them into Expand.
	if got := cfg.K8sNamespaceSelector().IncludeLabels; !reflect.DeepEqual(got, []string{"env=prod", "team=payments"}) {
		t.Errorf("K8sNamespaceSelector include_labels = %v, want [env=prod team=payments]", got)
	}
}

// TestLoadWithEnvironmentVariables tests loading configuration with environment variables.
func TestLoadWithEnvironmentVariables(t *testing.T) {
	// Reset viper to ensure clean state
	viper.Reset()

	// Set environment variables
	testEnvVars := map[string]string{
		"ACCRETION_API_PORT":           "9090",
		"ACCRETION_DATABASE_HOST":      "db.example.com",
		"ACCRETION_LLM_PROVIDER":       "openai",
		"ACCRETION_LLM_OPENAI_API_KEY": "test-api-key-123",
		"ACCRETION_K8S_POLL_INTERVAL":  "60s",
		"ACCRETION_LOGGING_LEVEL":      "debug",
	}

	// Set environment variables
	keysToUnset := make([]string, 0, len(testEnvVars))
	for key, value := range testEnvVars {
		os.Setenv(key, value)
		keysToUnset = append(keysToUnset, key)
	}
	defer func() {
		for _, key := range keysToUnset {
			os.Unsetenv(key)
		}
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config with env vars: %v", err)
	}

	// Test that environment variables override defaults
	if cfg.API.Port != 9090 {
		t.Errorf("Expected api port 9090, got %d", cfg.API.Port)
	}
	if cfg.Database.Host != "db.example.com" {
		t.Errorf("Expected database host 'db.example.com', got '%s'", cfg.Database.Host)
	}
	if cfg.LLM.Provider != "openai" {
		t.Errorf("Expected LLM provider 'openai', got '%s'", cfg.LLM.Provider)
	}
	if cfg.LLM.OpenAI.APIKey != "test-api-key-123" {
		t.Errorf("Expected OpenAI API key 'test-api-key-123', got '%s'", cfg.LLM.OpenAI.APIKey)
	}
	if cfg.K8s.PollInterval != "60s" {
		t.Errorf("Expected K8s poll interval '60s', got '%s'", cfg.K8s.PollInterval)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Expected logging level 'debug', got '%s'", cfg.Logging.Level)
	}
}

// TestLoadWithConfigFile tests loading configuration from a YAML file.
func TestLoadWithConfigFile(t *testing.T) {
	// Reset viper and clear env vars to ensure clean state
	viper.Reset()
	clearAccretionEnv(t)

	// Create a temporary config file
	configContent := `
api:
  port: 3000
server:
  host: "127.0.0.1"

database:
  host: "test-db"
  port: 5433
  user: "testuser"
  password: "testpass"
  database: "testdb"
  sslmode: "require"

llm:
  provider: "openai"
  openai:
    api_key: "test-key"
    base_url: "https://api.openai.com/v1"
  embedding_model: "nomic-embed-text:latest"
  chat_model: "llama3:8b"

k8s:
  config_path: "/custom/kubeconfig"
  namespaces:
    - "production"
    - "staging"
  poll_interval: "15s"

logging:
  level: "error"
  format: "json"
`

	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "test-config.yaml")

	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	// Configure viper to read from the test file
	viper.SetConfigFile(configFile)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config from file: %v", err)
	}

	// Test that file values are loaded correctly
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Expected server host '127.0.0.1', got '%s'", cfg.Server.Host)
	}
	if cfg.API.Port != 3000 {
		t.Errorf("Expected api port 3000, got %d", cfg.API.Port)
	}
	if cfg.Database.Host != "test-db" {
		t.Errorf("Expected database host 'test-db', got '%s'", cfg.Database.Host)
	}
	if cfg.Database.SSLMode != "require" {
		t.Errorf("Expected database sslmode 'require', got '%s'", cfg.Database.SSLMode)
	}
	if cfg.LLM.Provider != "openai" {
		t.Errorf("Expected LLM provider 'openai', got '%s'", cfg.LLM.Provider)
	}
	if len(cfg.K8s.Namespaces) != 2 {
		t.Errorf("Expected 2 K8s namespaces, got %d", len(cfg.K8s.Namespaces))
	}
	if cfg.K8s.Namespaces[0] != "production" || cfg.K8s.Namespaces[1] != "staging" {
		t.Errorf("Expected K8s namespaces ['production', 'staging'], got %v", cfg.K8s.Namespaces)
	}
}

// TestEnvironmentVariablePrecedence tests that environment variables take precedence over config files.
func TestEnvironmentVariablePrecedence(t *testing.T) {
	// Reset viper to ensure clean state
	viper.Reset()

	// Create a config file with one value
	configContent := `
server:
  port: 3000
llm:
  provider: "ollama"
`

	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "test-config.yaml")

	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	// Set environment variable that should override the config file
	os.Setenv("ACCRETION_API_PORT", "4000")
	os.Setenv("ACCRETION_LLM_PROVIDER", "openai")
	defer os.Unsetenv("ACCRETION_API_PORT")
	defer os.Unsetenv("ACCRETION_LLM_PROVIDER")

	// Configure viper to read from the test file
	viper.SetConfigFile(configFile)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Environment variable should override config file
	if cfg.API.Port != 4000 {
		t.Errorf("Expected api port 4000 (from env), got %d", cfg.API.Port)
	}
	if cfg.LLM.Provider != "openai" {
		t.Errorf("Expected LLM provider 'openai' (from env), got '%s'", cfg.LLM.Provider)
	}
}

// TestOpenAIAPIKeyOverride tests that ACCRETION_LLM_OPENAI_API_KEY populates the config.
func TestOpenAIAPIKeyOverride(t *testing.T) {
	// Reset viper to ensure clean state
	viper.Reset()
	clearAccretionEnv(t)

	testAPIKey := "sk-test-key-from-env"
	os.Setenv("ACCRETION_LLM_OPENAI_API_KEY", testAPIKey)
	defer os.Unsetenv("ACCRETION_LLM_OPENAI_API_KEY")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.LLM.OpenAI.APIKey != testAPIKey {
		t.Errorf("Expected OpenAI API key '%s', got '%s'", testAPIKey, cfg.LLM.OpenAI.APIKey)
	}
}

// TestLoadAPIKeyIdentitiesFromFile covers issue #41: per-client scoped API-key
// identities are delivered only through the mounted config.yaml (viper has no
// env binding for a struct list), so the ConfigMap -> Load() round trip is the
// whole delivery mechanism and must be pinned by a test.
func TestLoadAPIKeyIdentitiesFromFile(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)

	configContent := `
auth:
  api_key_identities:
    - name: "accretion-mcp"
      hash: "0f9d1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7"
      permissions:
        - "read:memory"
        - "chat:query"
    - name: "ci"
      hash: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809"
      roles:
        - "api-key"
      permissions:
        - "read:memory"
`

	configFile := filepath.Join(t.TempDir(), "identities.yaml")
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	viper.SetConfigFile(configFile)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config from file: %v", err)
	}

	want := []APIKeyIdentity{
		{
			Name:        "accretion-mcp",
			Hash:        "0f9d1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7",
			Permissions: []string{"read:memory", "chat:query"},
		},
		{
			Name:        "ci",
			Hash:        "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
			Roles:       []string{"api-key"},
			Permissions: []string{"read:memory"},
		},
	}
	if !reflect.DeepEqual(cfg.Auth.APIKeyIdentities, want) {
		t.Errorf("Expected identities %+v, got %+v", want, cfg.Auth.APIKeyIdentities)
	}
}

// TestGenerateDefault tests the generation of default configuration file.
func TestGenerateDefault(t *testing.T) {
	tempDir := t.TempDir()
	outputFile := filepath.Join(tempDir, "default-config.yaml")

	err := GenerateDefault(outputFile)
	if err != nil {
		t.Fatalf("Failed to generate default config: %v", err)
	}

	// Check that the file was created
	if _, statErr := os.Stat(outputFile); os.IsNotExist(statErr) {
		t.Error("Default config file was not created")
		return
	}

	// Read and parse the generated file
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("Failed to read generated config file: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Failed to unmarshal generated config: %v", err)
	}

	// Verify some key values
	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("Expected server host '0.0.0.0' in generated config, got '%s'", cfg.Server.Host)
	}
	if cfg.Database.Database != "accretion" {
		t.Errorf("Expected database name 'accretion' in generated config, got '%s'", cfg.Database.Database)
	}
	if cfg.LLM.Provider != "ollama" {
		t.Errorf("Expected LLM provider 'ollama' in generated config, got '%s'", cfg.LLM.Provider)
	}
}

// TestConfigValidation tests configuration validation scenarios.
func TestConfigValidation(t *testing.T) {
	tests := []struct {
		validator func(cfg *Config) error
		name      string
		config    Config
		wantError bool
	}{
		{
			name: "valid configuration",
			config: Config{
				Server: ServerConfig{Host: "localhost"},
				Database: DatabaseConfig{
					Host: "localhost", Port: 5432, User: "user",
					Password: "pass", Database: "db", SSLMode: "disable",
				},
				LLM: LLMConfig{Provider: "openai", OpenAI: OpenAIConfig{APIKey: "key"}},
				K8s: K8sConfig{PollInterval: "30s"},
			},
			validator: validatePollInterval,
			wantError: false,
		},
		{
			name: "invalid poll interval",
			config: Config{
				K8s: K8sConfig{PollInterval: "invalid"},
			},
			validator: validatePollInterval,
			wantError: true,
		},
		{
			name: "empty poll interval",
			config: Config{
				K8s: K8sConfig{PollInterval: ""},
			},
			validator: validatePollInterval,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.validator(&tt.config)
			if tt.wantError && err == nil {
				t.Error("Expected validation error but got none")
			}
			if !tt.wantError && err != nil {
				t.Errorf("Unexpected validation error: %v", err)
			}
		})
	}
}

// TestConfigStructure tests that the configuration structure is correctly defined.
func TestConfigStructure(t *testing.T) {
	cfg := &Config{}

	// Test that all required fields exist and have correct types
	cfgType := reflect.TypeOf(cfg).Elem()

	expectedFields := map[string]reflect.Type{
		"Server":   reflect.TypeOf(ServerConfig{}),
		"Database": reflect.TypeOf(DatabaseConfig{}),
		"LLM":      reflect.TypeOf(LLMConfig{}),
		"K8s":      reflect.TypeOf(K8sConfig{}),
		"Logging":  reflect.TypeOf(LoggingConfig{}),
	}

	for fieldName, expectedType := range expectedFields {
		field, found := cfgType.FieldByName(fieldName)
		if !found {
			t.Errorf("Expected field '%s' not found in Config struct", fieldName)
			continue
		}

		if field.Type != expectedType {
			t.Errorf("Field '%s' has type %v, expected %v", fieldName, field.Type, expectedType)
		}

		// Check that yaml and mapstructure tags are present
		yamlTag := field.Tag.Get("yaml")
		mapstructureTag := field.Tag.Get("mapstructure")

		if yamlTag == "" {
			t.Errorf("Field '%s' missing yaml tag", fieldName)
		}
		if mapstructureTag == "" {
			t.Errorf("Field '%s' missing mapstructure tag", fieldName)
		}
	}
}

// TestDatabaseConfigValidation tests database configuration validation.
func TestDatabaseConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config DatabaseConfig
		valid  bool
	}{
		{
			name: "valid config",
			config: DatabaseConfig{
				Host: "localhost", Port: 5432, User: "user",
				Password: "pass", Database: "db", SSLMode: "disable",
			},
			valid: true,
		},
		{
			name: "missing host",
			config: DatabaseConfig{
				Port: 5432, User: "user", Password: "pass",
				Database: "db", SSLMode: "disable",
			},
			valid: false,
		},
		{
			name: "invalid port",
			config: DatabaseConfig{
				Host: "localhost", Port: 0, User: "user",
				Password: "pass", Database: "db", SSLMode: "disable",
			},
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid := validateDatabaseConfig(&tt.config)
			if valid != tt.valid {
				t.Errorf("Expected validation result %v, got %v", tt.valid, valid)
			}
		})
	}
}

// Helper functions for validation tests

// validatePollInterval validates that the poll interval can be parsed as a duration.
func validatePollInterval(cfg *Config) error {
	if cfg.K8s.PollInterval == "" {
		return fmt.Errorf("poll interval cannot be empty")
	}
	_, err := time.ParseDuration(cfg.K8s.PollInterval)
	return err
}

// validateDatabaseConfig validates database configuration.
func validateDatabaseConfig(cfg *DatabaseConfig) bool {
	return cfg.Host != "" && cfg.Port > 0 && cfg.User != "" && cfg.Database != ""
}

// BenchmarkConfigLoad benchmarks configuration loading.
func BenchmarkConfigLoad(b *testing.B) {
	// Set up environment for consistent benchmarking
	os.Setenv("ACCRETION_API_PORT", "41337")
	defer os.Unsetenv("ACCRETION_API_PORT")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		viper.Reset()
		_, err := Load()
		if err != nil {
			b.Fatalf("Config load failed: %v", err)
		}
	}
}

// BenchmarkGenerateDefault benchmarks default config generation.
func BenchmarkGenerateDefault(b *testing.B) {
	tempDir := b.TempDir()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		outputFile := filepath.Join(tempDir, fmt.Sprintf("config-%d.yaml", i))
		err := GenerateDefault(outputFile)
		if err != nil {
			b.Fatalf("Generate default failed: %v", err)
		}
	}
}

// A developer's bare OPENAI_API_KEY — the one the OpenAI SDK, llm, and aider
// read — must not become Accretion's key. Clean break, no fallback (#53).
func TestBareOpenAIAPIKeyIgnored(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)
	os.Setenv("OPENAI_API_KEY", "sk-developers-personal-key")
	defer os.Unsetenv("OPENAI_API_KEY")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.LLM.OpenAI.APIKey != "" {
		t.Errorf("bare OPENAI_API_KEY leaked into config: got %q, want empty", cfg.LLM.OpenAI.APIKey)
	}
}

// Anthropic mirrors the OpenAI convergence (#52): the namespaced name is read,
// and a bare ANTHROPIC_API_KEY -- the one Claude Code and the Anthropic SDK
// read -- is not.
func TestAnthropicAPIKeyFromNamespacedEnv(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)
	const want = "sk-ant-test-key"
	os.Setenv("ACCRETION_LLM_ANTHROPIC_API_KEY", want)
	defer os.Unsetenv("ACCRETION_LLM_ANTHROPIC_API_KEY")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.LLM.Anthropic.APIKey != want {
		t.Errorf("got %q, want %q", cfg.LLM.Anthropic.APIKey, want)
	}
}

func TestBareAnthropicAPIKeyIgnored(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)
	os.Setenv("ANTHROPIC_API_KEY", "sk-ant-developers-personal-key")
	defer os.Unsetenv("ANTHROPIC_API_KEY")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cfg.LLM.Anthropic.APIKey != "" {
		t.Errorf("bare ANTHROPIC_API_KEY leaked into config: got %q, want empty", cfg.LLM.Anthropic.APIKey)
	}
}

// The three post-unmarshal os.Getenv overrides for the LLM keys were deleted in
// #52/#53 as redundant. They are redundant ONLY because bindEnvVars registers
// each key with an explicit single-arg viper.BindEnv. SetEnvPrefix +
// AutomaticEnv alone is NOT sufficient: viper's AutomaticEnv does not surface a
// key to Unmarshal unless the key is already known (via a default, a config
// file entry, or a bind), and neither LLM key has a default.
//
// This test pins both halves. If someone deletes the BindEnv calls believing
// the ACCRETION prefix covers them, the first subtest fails rather than the
// keys silently going empty at runtime.
func TestLLMKeysRequireExplicitBind(t *testing.T) {
	t.Run("AutomaticEnvAloneIsInsufficient", func(t *testing.T) {
		viper.Reset()
		clearAccretionEnv(t)
		os.Setenv("ACCRETION_LLM_OPENAI_API_KEY", "should-not-resolve")
		defer os.Unsetenv("ACCRETION_LLM_OPENAI_API_KEY")

		viper.SetEnvPrefix("ACCRETION")
		viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
		viper.AutomaticEnv()

		var cfg Config
		if err := viper.Unmarshal(&cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.LLM.OpenAI.APIKey != "" {
			t.Fatalf("viper behavior changed: AutomaticEnv now reaches Unmarshal "+
				"for unregistered keys (got %q). The explicit BindEnv calls in "+
				"bindEnvVars may now be removable -- re-check before doing so.",
				cfg.LLM.OpenAI.APIKey)
		}
	})

	t.Run("BindEnvMakesItResolve", func(t *testing.T) {
		viper.Reset()
		clearAccretionEnv(t)
		os.Setenv("ACCRETION_LLM_OPENAI_API_KEY", "resolves")
		defer os.Unsetenv("ACCRETION_LLM_OPENAI_API_KEY")

		var cfg Config
		bindEnvVars()
		if err := viper.Unmarshal(&cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.LLM.OpenAI.APIKey != "resolves" {
			t.Errorf("got %q, want %q", cfg.LLM.OpenAI.APIKey, "resolves")
		}
	})
}

// TestCollectorSecretsRequireExplicitBind guards the analogous gotcha for the
// accretion-collector chart (#112): once database.password and
// queue.redis_password stop being written into the rendered ConfigMap, an env
// var sourced from a Secret is the only place either value can come from, and
// AutomaticEnv alone does not reach Unmarshal for either key without the
// explicit binds in bindEnvVars. See TestLLMKeysRequireExplicitBind for the
// general mechanism this mirrors.
func TestCollectorSecretsRequireExplicitBind(t *testing.T) {
	t.Run("AutomaticEnvAloneIsInsufficient", func(t *testing.T) {
		viper.Reset()
		clearAccretionEnv(t)
		os.Setenv("ACCRETION_DATABASE_PASSWORD", "should-not-resolve")
		os.Setenv("ACCRETION_QUEUE_REDIS_PASSWORD", "should-not-resolve")
		defer os.Unsetenv("ACCRETION_DATABASE_PASSWORD")
		defer os.Unsetenv("ACCRETION_QUEUE_REDIS_PASSWORD")

		viper.SetEnvPrefix("ACCRETION")
		viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
		viper.AutomaticEnv()

		var cfg CollectorConfig
		if err := viper.Unmarshal(&cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.Database.Password != "" {
			t.Fatalf("viper behavior changed: AutomaticEnv now reaches Unmarshal "+
				"for unregistered keys (got %q for database.password). The "+
				"explicit BindEnv calls in bindEnvVars may now be removable -- "+
				"re-check before doing so.", cfg.Database.Password)
		}
		if cfg.Queue.RedisPassword != "" {
			t.Fatalf("viper behavior changed: AutomaticEnv now reaches Unmarshal "+
				"for unregistered keys (got %q for queue.redis_password). The "+
				"explicit BindEnv calls in bindEnvVars may now be removable -- "+
				"re-check before doing so.", cfg.Queue.RedisPassword)
		}
	})

	t.Run("BindEnvMakesItResolve", func(t *testing.T) {
		viper.Reset()
		clearAccretionEnv(t)
		os.Setenv("ACCRETION_DATABASE_PASSWORD", "db-resolves")
		os.Setenv("ACCRETION_QUEUE_REDIS_PASSWORD", "redis-resolves")
		defer os.Unsetenv("ACCRETION_DATABASE_PASSWORD")
		defer os.Unsetenv("ACCRETION_QUEUE_REDIS_PASSWORD")

		var cfg CollectorConfig
		bindEnvVars()
		if err := viper.Unmarshal(&cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.Database.Password != "db-resolves" {
			t.Errorf("database.password: got %q, want %q", cfg.Database.Password, "db-resolves")
		}
		if cfg.Queue.RedisPassword != "redis-resolves" {
			t.Errorf("queue.redis_password: got %q, want %q", cfg.Queue.RedisPassword, "redis-resolves")
		}
	})
}

// TestGenerateDefaultOmitsSecrets asserts the generated config carries no
// non-empty secret (#129). It walks the YAML actually written rather than
// inspecting the Config struct, because the file is what gets committed by
// accident.
//
// It asserts twice, because the two checks fail on different mistakes:
//
//   - Every key in OmittedSecrets comes out empty. This ties the list
//     `accretion generate config` prints to what the file actually contains, so
//     a key that is listed as omitted but left populated fails here.
//   - No secret-looking key anywhere in the tree is a non-empty string. The
//     first check cannot see a secret nobody remembered to list, and a list is
//     exactly the thing that gets forgotten when a field is added, so this one
//     is driven off the generated YAML instead.
//
// Empty is the bar, not absent: none of these fields carry omitempty, so
// `session_key: ""` is what a secret-free config looks like. A present-but-blank
// value is not a credential, and env beats config in viper's precedence, so it
// cannot shadow the variable that supplies the real one.
func TestGenerateDefaultOmitsSecrets(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "default-config.yaml")
	if err := GenerateDefault(outputFile); err != nil {
		t.Fatalf("GenerateDefault failed: %v", err)
	}
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("Failed to read generated config file: %v", err)
	}

	var tree map[string]interface{}
	if err := yaml.Unmarshal(data, &tree); err != nil {
		t.Fatalf("Failed to unmarshal generated config: %v", err)
	}

	if len(OmittedSecrets) == 0 {
		t.Fatal("OmittedSecrets is empty, so this test would assert nothing")
	}
	for _, secret := range OmittedSecrets {
		value, found := lookupYAMLPath(tree, strings.Split(secret.Key, "."))
		if !found {
			continue // absent is at least as good as empty
		}
		if s, ok := value.(string); !ok || s != "" {
			t.Errorf("Generated config has %s = %#v, want empty; it is supplied by %s",
				secret.Key, value, secret.EnvVar)
		}
	}

	for _, found := range scanYAMLForSecrets(tree, nil) {
		t.Errorf("Generated config has %s = %q, want empty: a key named like a secret must not carry a "+
			"value. Leave it empty in GenerateDefault and add it to OmittedSecrets so `generate config` "+
			"names the variable that supplies it.", found.key, found.value)
	}
}

// secretKeySubstrings are the name fragments that make a config key
// secret-bearing. Matching on the name is deliberately blunt: it costs a false
// positive on a harmless key, which is a one-line fix, and catches a real
// credential nobody thought to list, which is the failure that matters.
//
// "api_key" also matches auth.api_key_identities, which holds SHA-256 digests
// rather than keys. That is not a problem in practice: the entries are a struct
// list, and scanYAMLForSecrets only reports non-empty strings.
var secretKeySubstrings = []string{"password", "secret", "api_key", "session_key", "token"}

// notSecretKeySuffixes are endpoint-shaped names that "token" would otherwise
// match. auth.oauth.token_url is the provider's public token endpoint, not a
// credential, and every OAuth config has one. The carve-out is on the suffix
// rather than the whole key so a newly added *_url stays exempt without an edit
// here, while a bare `token` or `access_token` is still caught.
var notSecretKeySuffixes = []string{"_url", "_uri", "_endpoint"}

// secretFinding is one non-empty secret-looking value found in a YAML tree.
type secretFinding struct {
	key   string // dotted path, e.g. "auth.session_key"
	value string
}

// scanYAMLForSecrets walks every mapping in a decoded YAML tree and reports the
// non-empty string leaves whose key name matches secretKeySubstrings. prefix is
// the dotted path of the node being walked; pass nil at the root.
func scanYAMLForSecrets(node interface{}, prefix []string) []secretFinding {
	var findings []secretFinding

	visit := func(name string, child interface{}) {
		path := append(append([]string{}, prefix...), name)
		if s, ok := child.(string); ok {
			if s == "" {
				return
			}
			lower := strings.ToLower(name)
			for _, suffix := range notSecretKeySuffixes {
				if strings.HasSuffix(lower, suffix) {
					return
				}
			}
			for _, fragment := range secretKeySubstrings {
				if strings.Contains(lower, fragment) {
					findings = append(findings, secretFinding{key: strings.Join(path, "."), value: s})
					return
				}
			}
			return
		}
		findings = append(findings, scanYAMLForSecrets(child, path)...)
	}

	// yaml.v2 decodes nested mappings as map[interface{}]interface{}; only the
	// value unmarshalled into at the root is map[string]interface{}.
	switch m := node.(type) {
	case map[string]interface{}:
		for name, child := range m {
			visit(name, child)
		}
	case map[interface{}]interface{}:
		for name, child := range m {
			key, ok := name.(string)
			if !ok {
				continue
			}
			visit(key, child)
		}
	case []interface{}:
		for i, child := range m {
			findings = append(findings, scanYAMLForSecrets(child, append(append([]string{}, prefix...), fmt.Sprintf("[%d]", i)))...)
		}
	}
	return findings
}

// lookupYAMLPath walks a dotted config key through a decoded YAML tree. yaml.v2
// decodes nested mappings as map[interface{}]interface{} rather than
// map[string]interface{}, so both shapes are handled.
func lookupYAMLPath(tree map[string]interface{}, path []string) (interface{}, bool) {
	var node interface{} = tree
	for _, segment := range path {
		switch m := node.(type) {
		case map[string]interface{}:
			child, ok := m[segment]
			if !ok {
				return nil, false
			}
			node = child
		case map[interface{}]interface{}:
			child, ok := m[segment]
			if !ok {
				return nil, false
			}
			node = child
		default:
			return nil, false
		}
	}
	return node, true
}

// TestGenerateDefaultFileIsNotWorldReadable checks the mode of the file actually
// written, not the constant in the source. The generated config no longer leaves
// here secret-bearing, but it is the file an operator pastes a real value into,
// so it stays unreadable by other users on the machine.
func TestGenerateDefaultFileIsNotWorldReadable(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "default-config.yaml")
	if err := GenerateDefault(outputFile); err != nil {
		t.Fatalf("GenerateDefault failed: %v", err)
	}

	info, err := os.Stat(outputFile)
	if err != nil {
		t.Fatalf("Failed to stat generated config file: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("Generated config file mode is %04o, want no group or other bits (0600)", mode)
	}
}

// TestGenerateDefaultTightensExistingFileMode covers the case WriteFile alone
// does not: when the output file already exists, its mode is left untouched, so
// regenerating over a world-readable file would silently keep it readable.
func TestGenerateDefaultTightensExistingFileMode(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "default-config.yaml")
	if err := os.WriteFile(outputFile, []byte("stale: true\n"), 0o644); err != nil {
		t.Fatalf("Failed to seed pre-existing config file: %v", err)
	}

	if err := GenerateDefault(outputFile); err != nil {
		t.Fatalf("GenerateDefault failed: %v", err)
	}

	info, err := os.Stat(outputFile)
	if err != nil {
		t.Fatalf("Failed to stat generated config file: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("Pre-existing config file left at mode %04o, want no group or other bits (0600)", mode)
	}
}

// The functional test client reads ACCRETION_API_WRITE_TIMEOUT to match the
// server's deadline exactly. That coupling is by convention — nothing links the
// two — and it silently broke once when the setting moved from server to api, so
// this pins the env var the server actually honors.
func TestAPIWriteTimeoutEnvVar(t *testing.T) {
	viper.Reset()
	t.Setenv("ACCRETION_API_WRITE_TIMEOUT", "37s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.API.GetWriteTimeout(); got != 37*time.Second {
		t.Errorf("ACCRETION_API_WRITE_TIMEOUT=37s gave %v, want 37s — the functional "+
			"test client reads this variable and would drift from the server", got)
	}
}

// api.tls.* must resolve from the environment the same way api.write_timeout
// does — via a registered default plus AutomaticEnv, with no explicit BindEnv.
// If viper's behavior changes, this fails here rather than in a cluster where
// ACCRETION_API_TLS_ENABLED silently does nothing and the API stays cleartext.
func TestAPITLSEnvVars(t *testing.T) {
	viper.Reset()
	t.Setenv("ACCRETION_API_TLS_ENABLED", "true")
	t.Setenv("ACCRETION_API_TLS_CERT_FILE", "/certs/tls.crt")
	t.Setenv("ACCRETION_API_TLS_KEY_FILE", "/certs/tls.key")
	t.Setenv("ACCRETION_API_TLS_MIN_VERSION", "1.3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.API.TLS.Enabled {
		t.Error("ACCRETION_API_TLS_ENABLED=true did not reach api.tls.enabled")
	}
	if cfg.API.TLS.CertFile != "/certs/tls.crt" {
		t.Errorf("cert_file = %q, want /certs/tls.crt", cfg.API.TLS.CertFile)
	}
	if cfg.API.TLS.KeyFile != "/certs/tls.key" {
		t.Errorf("key_file = %q, want /certs/tls.key", cfg.API.TLS.KeyFile)
	}
	got, err := cfg.API.TLS.GetMinVersion()
	if err != nil {
		t.Fatalf("GetMinVersion: %v", err)
	}
	if got != tls.VersionTLS13 {
		t.Errorf("min_version = %#x, want TLS 1.3", got)
	}
}

// TLS is on by default. A deployment that terminates TLS elsewhere opts out
// explicitly; there is no configuration that quietly serves cleartext.
func TestAPITLSDefaultsOn(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.API.TLS.Enabled {
		t.Error("api.tls.enabled defaults to false; it must default to true")
	}
	got, err := cfg.API.TLS.GetMinVersion()
	if err != nil {
		t.Fatalf("GetMinVersion: %v", err)
	}
	if got != tls.VersionTLS12 {
		t.Errorf("default min_version = %#x, want TLS 1.2", got)
	}

	// On by default with no keypair configured must be a startup ERROR, not a
	// silent cleartext listener. This is the pairing that makes the default
	// safe: turning TLS on by default is only defensible because failing to
	// serve it is loud.
	tlsConfig, err := cfg.API.TLS.Validate()
	if err == nil {
		t.Fatalf("Validate accepted the default config with no keypair, returning %+v", tlsConfig)
	}
	if tlsConfig != nil {
		t.Errorf("a failed Validate returned a tls.Config: %+v", tlsConfig)
	}
}

// Opting out is explicit and complete: disabled means no certificate is read
// and the listener is cleartext.
func TestAPITLSCanBeDisabled(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)
	t.Setenv("ACCRETION_API_TLS_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.TLS.Enabled {
		t.Fatal("ACCRETION_API_TLS_ENABLED=false did not disable TLS")
	}
	tlsConfig, err := cfg.API.TLS.Validate()
	if err != nil {
		t.Fatalf("Validate on a disabled config: %v", err)
	}
	if tlsConfig != nil {
		t.Errorf("a disabled config produced a tls.Config: %+v", tlsConfig)
	}
}

// The Discord bot's CA path arrives through viper.BindEnv alone. An explicit
// os.Getenv in LoadDiscord used to shadow this; removing it is only safe if the
// bind genuinely resolves, so pin that rather than assume it.
func TestDiscordAPITLSEnvVars(t *testing.T) {
	viper.Reset()
	clearAccretionEnv(t)
	t.Setenv("ACCRETION_API_URL", "https://accretion-api.accretion.svc.cluster.local:80")
	t.Setenv("ACCRETION_API_CA_FILE", "/etc/accretion/tls/ca.crt")
	t.Setenv("ACCRETION_API_INSECURE_SKIP_VERIFY", "true")

	cfg, err := LoadDiscord()
	if err != nil {
		t.Fatalf("LoadDiscord: %v", err)
	}
	if cfg.API.CAFile != "/etc/accretion/tls/ca.crt" {
		t.Errorf("api.ca_file = %q; the viper.BindEnv no longer resolves it", cfg.API.CAFile)
	}
	if !cfg.API.InsecureSkipVerify {
		t.Error("api.insecure_skip_verify did not resolve from the environment")
	}
	if cfg.API.URL != "https://accretion-api.accretion.svc.cluster.local:80" {
		t.Errorf("api.url = %q", cfg.API.URL)
	}
}
