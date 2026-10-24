package kubernetes

import (
	"fmt"
	"slices"
	"strings"

	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	helmv3 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"accretion-infrastructure/pkg/config"
)

// Helm values keys repeated across the release argument maps below. They are
// constants only to satisfy goconst; the strings are Helm value paths, not
// behavior.
const (
	keyAutoscaling = "autoscaling"
	keyCPU         = "cpu"
	keyDatabase    = "database"
	keyEnabled     = "enabled"
	keyHost        = "host"
	keyImage       = "image"
	keyMemory      = "memory"
	keySSLMode     = "sslmode"
	keyTag         = "tag"
)

// Everything below builds on ONE rule: the shared, stack-independent settings
// live in deploy/pulumi.yaml and are loaded from there, and the only values
// assembled in Go are the ones that cannot live in a file — deploy-time outputs
// of the infrastructure layer, and per-stack values from the typed config.
//
// The chart paths are ABSOLUTE, built from the repository root the values file
// was found in. They used to be "./charts/<name>", which resolved against the
// working directory of whoever invoked pulumi. scripts/deploy.sh cds into
// pulumi/ first and there is no pulumi/charts, so the chart was never found and
// no release could render. See config.findRepoRoot.

func deployAccretionChart(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
	infraOutputs *InfraOutputs,
	vals *config.ChartValues,
	sizing stackSizing,
) (*helmv3.Release, error) {
	base := vals.Values(config.ChartAccretionAPI)

	// The roster is declared centrally; the digests are per-stack. A missing or
	// malformed digest fails here, naming the identity and the command that
	// fixes it, rather than reaching the chart as an empty apiKeys list -- which
	// is the fail-closed guard that has been rejecting this release all along.
	//
	// A stack that does not deploy the bot does not declare the bot's identity,
	// so it is never asked for a digest that would authenticate nothing.
	roster := base["apiKeys"]
	if !cfg.App.EnableDiscord {
		roster = config.RosterExcluding(roster, config.DiscordIdentity)
	}

	identities, err := config.ResolveAPIKeyIdentities(roster, cfg.APIKeyHashes, cfg.Stack)
	if err != nil {
		return nil, err
	}

	release, err := helmv3.NewRelease(ctx, "accretion-api", &helmv3.ReleaseArgs{
		Name:      pulumi.String("accretion-api"),
		Chart:     pulumi.String(vals.ChartPath(config.ChartAccretionAPI)),
		Namespace: pulumi.String(cfg.App.Namespace),
		Values: pulumi.All(infraOutputs.StaticIP, infraOutputs.DatabaseHost).ApplyT( //nolint:errcheck // type-asserted below
			func(args []any) map[string]any {
				return accretionValues(cfg, base, sizing, identities, args[0].(string), args[1].(string))
			},
		).(pulumi.MapOutput),
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{ns}))
	if err != nil {
		return nil, err
	}
	return release, nil
}

// accretionValues is the complete value set the accretion-api release receives:
// the central file's subtree with everything that cannot live in a file merged
// over it. Pure and separate from the release above so a test can assert every
// key it sets is one the chart declares (see config.UndeclaredPaths).
func accretionValues(
	cfg *config.Config,
	base map[string]any,
	sizing stackSizing,
	identities []any,
	staticIP, dbHost string,
) map[string]any {
	// One domain source. cfg.DNS.Domain is also what the DNS module provisions
	// the managed zone from, so the zone, the Ingress hosts and the managed
	// certificate cannot name different domains -- which is exactly how the
	// charts and Pulumi once drifted onto two different ones.
	domain := cfg.DNS.Domain
	www := fmt.Sprintf("www.%s", domain)

	return config.MergeValues(base, map[string]any{
		keyImage: map[string]any{keyTag: cfg.App.ImageTag},
		"ingress": map[string]any{
			"hosts": []any{ingressHost(domain), ingressHost(www)},
			"managedCertificate": map[string]any{
				"domains": []any{domain, www},
			},
			"globalStaticIP": staticIP,
		},
		"apiKeys":   identities,
		"resources": sizing.apiResources,
		"config": map[string]any{
			keyDatabase: map[string]any{
				keyHost:    dbHost,
				keySSLMode: cfg.Database.SSLMode,
			},
		},
	})
}

func deployCollectorChart(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
	infraOutputs *InfraOutputs,
	vals *config.ChartValues,
	redisSecret *corev1.Secret,
	sizing stackSizing,
) (*helmv3.Release, error) {
	base := vals.Values(config.ChartAccretionCollector)

	release, err := helmv3.NewRelease(ctx, "accretion-collector", &helmv3.ReleaseArgs{
		Name:      pulumi.String("accretion-collector"),
		Chart:     pulumi.String(vals.ChartPath(config.ChartAccretionCollector)),
		Namespace: pulumi.String(cfg.App.Namespace),
		Values: infraOutputs.DatabaseHost.ApplyT( //nolint:errcheck // type-asserted below
			func(dbHost string) map[string]any {
				return collectorValues(cfg, base, sizing, dbHost)
			},
		).(pulumi.MapOutput),
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{ns, redisSecret}))
	if err != nil {
		return nil, err
	}
	return release, nil
}

// collectorValues is the complete value set the accretion-collector release
// receives. Pure for the same reason as accretionValues.
//
// The three collector Deployments share one image, so the tag is merged into
// each of them. Whether these keys land is asserted by rendering rather than by
// reading the map -- see config.UndeclaredPaths for why that distinction
// matters here.
//
// No config.redis.host: the chart derives the Redis address from the release
// name and no template reads a host key (#105).
func collectorValues(cfg *config.Config, base map[string]any, sizing stackSizing, dbHost string) map[string]any {
	// No replicaCount: the chart emits `replicas` only when autoscaling is
	// disabled, and it never is here, so setting it configured nothing.
	perKind := map[string]any{keyAutoscaling: sizing.collectorAutoscaling}
	image := map[string]any{keyImage: map[string]any{keyTag: cfg.App.ImageTag}}

	return config.MergeValues(base, map[string]any{
		"collector":       image,
		"statusCollector": image,
		"dataCollector": map[string]any{
			keyImage: map[string]any{keyTag: cfg.App.ImageTag},
			"kinds": map[string]any{
				"log":    perKind,
				"metric": perKind,
			},
		},
		"config": map[string]any{
			keyDatabase: map[string]any{
				keyHost:    dbHost,
				keySSLMode: cfg.Database.SSLMode,
			},
		},
	})
}

// deployDiscordChart deploys the bot. Gated by the caller on the stack's
// enableDiscord, the same way the collector is gated -- until now Tilt was the
// only thing that installed this chart, so it had no production path at all.
//
// Depends on its Secret as well as the namespace: every credential the bot
// needs arrives by secretKeyRef and none of the refs is optional, so a pod
// created first would only sit failing until the Secret caught up.
func deployDiscordChart(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
	secret *corev1.Secret,
	vals *config.ChartValues,
) (*helmv3.Release, error) {
	values := discordValues(cfg, vals.Values(config.ChartAccretionDiscord))

	release, err := helmv3.NewRelease(ctx, "accretion-discord", &helmv3.ReleaseArgs{
		Name:      pulumi.String("accretion-discord"),
		Chart:     pulumi.String(vals.ChartPath(config.ChartAccretionDiscord)),
		Namespace: pulumi.String(cfg.App.Namespace),
		Values:    pulumi.ToMap(values),
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{ns, secret}))
	if err != nil {
		return nil, err
	}
	return release, nil
}

// discordValues is the complete value set the accretion-discord release
// receives. The image tag is the only thing here that cannot live in the
// central file: the bot reaches accretion-api by in-cluster DNS derived from
// the release namespace, so it needs none of the infrastructure outputs the
// other two releases wait on.
func discordValues(cfg *config.Config, base map[string]any) map[string]any {
	return config.MergeValues(base, map[string]any{
		keyImage: map[string]any{keyTag: cfg.App.ImageTag},
	})
}

func ingressHost(host string) map[string]any {
	return map[string]any{
		keyHost: host,
		"paths": []any{
			map[string]any{"path": "/", "pathType": "Prefix"},
		},
	}
}

// stackSizing is the per-environment sizing, the one thing in a release that
// legitimately differs between stacks.
type stackSizing struct {
	apiResources         map[string]any
	collectorAutoscaling map[string]any
}

// environmentSizing is keyed on the values the stack files actually set:
// pulumi/stacks/Pulumi.{dev,staging,prod}.yaml set environment to dev, staging
// and prod, and config.NewConfig defaults it to dev.
//
// This USED TO BRANCH ON "production", which no stack has ever set. The branch
// was dead, so the production stack silently took development sizing -- API
// requests of 250m/256Mi instead of 500m/512Mi, and collector autoscalers
// capped at 10 replicas instead of 20. It went unnoticed because the lane's own
// tests were written against "production" too, so they exercised an environment
// that does not exist. sizingFor now refuses an unknown environment outright
// rather than quietly handing it the smallest tier.
var environmentSizing = map[string]stackSizing{
	"dev":     developmentSizing(),
	"staging": developmentSizing(),
	"prod":    productionSizing(),
}

func sizingFor(env string) (stackSizing, error) {
	sizing, ok := environmentSizing[env]
	if !ok {
		known := make([]string, 0, len(environmentSizing))
		for name := range environmentSizing {
			known = append(known, name)
		}
		slices.Sort(known)
		return stackSizing{}, fmt.Errorf(
			"no sizing is defined for environment %q (known: %s). Set `environment` in the stack config "+
				"to one of those, or add a tier here -- an unknown environment is not silently given the "+
				"smallest one, because that is how production ran on development sizing",
			env, strings.Join(known, ", "))
	}
	return sizing, nil
}

func productionSizing() stackSizing {
	return stackSizing{
		apiResources: map[string]any{
			"limits":   map[string]any{keyCPU: "1000m", keyMemory: "1Gi"},
			"requests": map[string]any{keyCPU: "500m", keyMemory: "512Mi"},
		},
		collectorAutoscaling: map[string]any{
			keyEnabled:                          true,
			"minReplicas":                       3,
			"maxReplicas":                       20,
			"targetCPUUtilizationPercentage":    70,
			"targetMemoryUtilizationPercentage": 80,
		},
	}
}

func developmentSizing() stackSizing {
	return stackSizing{
		apiResources: map[string]any{
			"limits":   map[string]any{keyCPU: "500m", keyMemory: "512Mi"},
			"requests": map[string]any{keyCPU: "250m", keyMemory: "256Mi"},
		},
		collectorAutoscaling: map[string]any{
			keyEnabled:                          true,
			"minReplicas":                       2,
			"maxReplicas":                       10,
			"targetCPUUtilizationPercentage":    70,
			"targetMemoryUtilizationPercentage": 80,
		},
	}
}
