package kubernetes

import (
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"accretion-infrastructure/pkg/config"
)

// InfraOutputs holds outputs from the infrastructure layer needed by Kubernetes resources.
// The actual wiring happens in main.go.
type InfraOutputs struct {
	Kubeconfig   pulumi.StringOutput
	StaticIP     pulumi.StringOutput
	ClusterName  pulumi.StringOutput
	DatabaseHost pulumi.StringOutput
}

func Deploy(ctx *pulumi.Context, cfg *config.Config, provider *k8s.Provider, infraOutputs *InfraOutputs) error {
	// The shared, stack-independent Helm values for every chart below, plus the
	// repository root their chart paths are built from. Loaded once: a failure
	// here (no repository root, a top-level key naming no chart, a chart with no
	// key) stops the deploy before a single release is created, rather than
	// letting some charts install on defaults.
	vals, err := config.LoadChartValues()
	if err != nil {
		return err
	}

	// Every "this stack is not deployable" check, in one block, before any
	// release is created. Both are pure functions of the config, so neither has
	// any reason to run lazily at the point of use -- sizingFor did, which meant
	// its error was handled twice and the reader had to find both.
	if sslErr := config.ValidateDatabaseSSLMode(cfg.Database.SSLMode); sslErr != nil {
		return sslErr
	}
	sizing, err := sizingFor(cfg.Environment)
	if err != nil {
		return err
	}

	ns, err := createNamespace(ctx, cfg, provider)
	if err != nil {
		return err
	}

	_, err = createSecrets(ctx, cfg, provider, ns)
	if err != nil {
		return err
	}

	_, err = deployAccretionChart(ctx, cfg, provider, ns, infraOutputs, vals, sizing)
	if err != nil {
		return err
	}

	if cfg.App.EnableCollector {
		redisSecret, secretErr := createRedisSecrets(ctx, cfg, provider, ns)
		if secretErr != nil {
			return secretErr
		}
		if _, err = deployCollectorChart(ctx, cfg, provider, ns, infraOutputs, vals, redisSecret, sizing); err != nil {
			return err
		}
	}

	if cfg.App.EnableDiscord {
		discordSecret, secretErr := createDiscordSecrets(ctx, cfg, provider, ns)
		if secretErr != nil {
			return secretErr
		}
		if _, err = deployDiscordChart(ctx, cfg, provider, ns, discordSecret, vals); err != nil {
			return err
		}
	}

	err = createNetworkPolicies(ctx, cfg, provider, ns)
	if err != nil {
		return err
	}

	return nil
}
