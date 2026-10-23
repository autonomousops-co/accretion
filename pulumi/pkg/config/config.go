package config

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

type GCPConfig struct {
	Project string
	Region  string
	Zone    string
}

type NetworkConfig struct {
	VpcName     string
	PodCidr     string
	ServiceCidr string
	NodeCidr    string
}

type GKEConfig struct {
	ClusterName       string
	MachineType       string
	KubernetesVersion string
	DiskSizeGb        int
	MaxNodes          int
	MinNodes          int
	NodeCount         int
}

type DNSConfig struct {
	Domain      string
	ManagedZone string
}

type AppConfig struct {
	Namespace       string
	ImageTag        string
	EnableCollector bool
	// EnableDiscord gates the Discord bot the way EnableCollector gates the
	// collector. Defaults FALSE, unlike the collector: the chart itself is off
	// by default (#94), the bot needs a token an operator may not have, and
	// Discord allows one session per token, so it is opt-in per stack.
	EnableDiscord bool
}

type DatabaseConfig struct {
	InstanceTier string
	// SSLMode is the libpq sslmode both releases connect with. Per-stack, like
	// the domain and the sizing, so a stack can raise it -- but never to
	// "disable", which ValidateSSLMode refuses. The charts default it to
	// "disable", which is a local-development default; every Pulumi stack talks
	// to a database over a real network.
	SSLMode          string
	DiskSizeGb       int
	HighAvailability bool
}

type Config struct { //nolint:govet // field order is semantic
	ProjectName string
	Environment string
	// Stack is the Pulumi stack name. Carried so the errors that tell an
	// operator to run `pulumi config set` can name the stack they mean.
	Stack string
	// APIKeyHashes maps an identity name from the central roster (see
	// ValuesFile) to the hex SHA-256 of that caller's API key. Per-stack and
	// supplied out of band, so no credential material is checked in.
	APIKeyHashes map[string]string
	GCP          GCPConfig
	Network      NetworkConfig
	GKE          GKEConfig
	DNS          DNSConfig
	App          AppConfig
	Database     DatabaseConfig
}

func NewConfig(ctx *pulumi.Context) *Config {
	cfg := config.New(ctx, "")
	gcpCfg := config.New(ctx, "gcp")

	env := cfg.Get("environment")
	if env == "" {
		env = "dev"
	}

	return &Config{
		ProjectName:  ctx.Project(),
		Environment:  env,
		Stack:        ctx.Stack(),
		APIKeyHashes: getAPIKeyHashes(cfg),
		GCP: GCPConfig{
			Project: gcpCfg.Get("project"),
			Region:  getStringOrDefault(gcpCfg, "region", "us-central1"),
			Zone:    getStringOrDefault(gcpCfg, "zone", "us-central1-a"),
		},
		Network: NetworkConfig{
			VpcName:     getStringOrDefault(cfg, "vpcName", "accretion-api-vpc"),
			PodCidr:     getStringOrDefault(cfg, "podCidr", "10.16.0.0/14"),
			ServiceCidr: getStringOrDefault(cfg, "serviceCidr", "10.20.0.0/20"),
			NodeCidr:    getStringOrDefault(cfg, "nodeCidr", "10.0.0.0/22"),
		},
		GKE: GKEConfig{
			ClusterName:       getStringOrDefault(cfg, "clusterName", "accretion-"+env),
			NodeCount:         getIntOrDefault(cfg, "nodeCount", 1),
			MinNodes:          getIntOrDefault(cfg, "minNodes", 1),
			MaxNodes:          getIntOrDefault(cfg, "maxNodes", 3),
			MachineType:       getStringOrDefault(cfg, "machineType", "e2-medium"),
			DiskSizeGb:        getIntOrDefault(cfg, "diskSizeGb", 50),
			KubernetesVersion: getStringOrDefault(cfg, "kubernetesVersion", "1.29"),
		},
		DNS: DNSConfig{
			// RFC 2606 reserved, mirroring charts/accretion-api/values-*.yaml
			// (#128). A stack that omits `domain` previews and validates but
			// serves nothing, rather than inheriting the domain of whoever
			// wrote this file. Set it per stack in stacks/Pulumi.<stack>.yaml.
			Domain:      getStringOrDefault(cfg, "domain", "example.com"),
			ManagedZone: getStringOrDefault(cfg, "managedZone", "accretion-api-zone"),
		},
		App: AppConfig{
			Namespace:       getStringOrDefault(cfg, "namespace", "accretion-api"),
			EnableCollector: getBoolOrDefault(cfg, "enableCollector", true),
			EnableDiscord:   getBoolOrDefault(cfg, "enableDiscord", false),
			ImageTag:        getStringOrDefault(cfg, "imageTag", "latest"),
		},
		Database: DatabaseConfig{
			InstanceTier:     getStringOrDefault(cfg, "dbInstanceTier", "db-f1-micro"),
			SSLMode:          getStringOrDefault(cfg, "dbSslMode", DefaultDatabaseSSLMode),
			DiskSizeGb:       getIntOrDefault(cfg, "dbDiskSizeGb", 10),
			HighAvailability: getBoolOrDefault(cfg, "dbHighAvailability", false),
		},
	}
}

// getAPIKeyHashes reads the per-identity API-key digests from stack config.
//
// Read through the SECRET getter so a value set with `pulumi config set
// --secret` decrypts without the plaintext getter's "value is secret" warning
// on every deploy. The returned Output is deliberately discarded rather than
// threaded into the release values: the chart renders each digest into its
// ConfigMap in the clear (that is what the server compares against), so marking
// it secret in Pulumi state would protect nothing while forcing every consumer
// of these values into an apply. What the secret setting does buy is that the
// digest is never committed to a stack file in plaintext.
//
// An unset key is not an error here. Missing digests are reported per identity
// by ResolveAPIKeyIdentities, which knows the roster and can name what is
// missing and the command that fixes it.
func getAPIKeyHashes(cfg *config.Config) map[string]string {
	hashes := map[string]string{}
	if _, err := cfg.TrySecretObject("apiKeyHashes", &hashes); err != nil {
		return map[string]string{}
	}
	return hashes
}

func getStringOrDefault(cfg *config.Config, key, defaultValue string) string {
	v := cfg.Get(key)
	if v == "" {
		return defaultValue
	}
	return v
}

func getIntOrDefault(cfg *config.Config, key string, defaultValue int) int {
	v, err := cfg.TryInt(key)
	if err != nil {
		return defaultValue
	}
	return v
}

func getBoolOrDefault(cfg *config.Config, key string, defaultValue bool) bool {
	v, err := cfg.TryBool(key)
	if err != nil {
		return defaultValue
	}
	return v
}
