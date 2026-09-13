package kubernetes

import (
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"accretion-infrastructure/pkg/config"
)

// Labels both Pulumi-provisioned Secrets carry.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelPartOf    = "app.kubernetes.io/part-of"
)

func createSecrets(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
) (*corev1.Secret, error) {
	secret, err := corev1.NewSecret(ctx, config.APISecretName, &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(config.APISecretName),
			Namespace: pulumi.String(cfg.App.Namespace),
			Labels: pulumi.StringMap{
				labelManagedBy: pulumi.String("pulumi"),
				labelPartOf:    pulumi.String("accretion-api"),
			},
		},
		Type: pulumi.String("Opaque"),
		StringData: pulumi.StringMap{
			// Placeholder values — in production these should be sourced
			// from GCP Secret Manager via the infrastructure layer.
			config.DBPasswordKey:  pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"openai-api-key":      pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"oauth-client-id":     pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"oauth-client-secret": pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"session-key":         pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			// Per-client API keys are NOT provisioned here. Each caller has its
			// own named identity in the accretion-api chart's `apiKeys` values
			// list, from which the chart derives both the server's SHA-256 digest
			// and the client's plaintext — one declared input, so the two halves
			// cannot drift apart.
			"accretion-auth-oauth-redirect-url": pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-auth-allowed-emails":     pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-auth-allowed-domains":    pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-auth-admin-emails":       pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-auth-admin-domains":      pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-auth-cookie-secure":      pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-auth-cookie-domain":      pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
			"accretion-logging-frontend-level":  pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF"),
		},
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{ns}))
	if err != nil {
		return nil, err
	}
	return secret, nil
}

// createDiscordSecrets provisions the bot's credentials.
//
// Three of these are secrets the bot cannot start without -- the Discord bot
// token and, for the anthropic provider the chart defaults to, the model keys.
// The fourth is the bot's own accretion-api key, which Pulumi has to provision
// because the accretion-api chart only publishes client keys for identities
// declared with a plaintext key, and every identity on this path is hash-only.
//
// Same placeholder pattern as createSecrets above: in production these come
// from GCP Secret Manager via the infrastructure layer. Created only when the
// stack enables the bot, so a stack that does not want it gets neither the
// release nor a Secret full of placeholders.
func createDiscordSecrets(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
) (*corev1.Secret, error) {
	secret, err := corev1.NewSecret(ctx, config.DiscordSecretName, &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(config.DiscordSecretName),
			Namespace: pulumi.String(cfg.App.Namespace),
			Labels: pulumi.StringMap{
				labelManagedBy: pulumi.String("pulumi"),
				labelPartOf:    pulumi.String("accretion-api"),
			},
		},
		Type:       pulumi.String("Opaque"),
		StringData: discordPlaceholders(),
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{ns}))
	if err != nil {
		return nil, err
	}
	return secret, nil
}

// discordPlaceholders builds the Secret's contents from config.DiscordSecretKeys,
// so the keys Pulumi provisions and the keys the bot's manifests reference are
// the same list rather than two lists that agree today.
//
// Placeholder values -- in production these should be sourced from GCP Secret
// Manager via the infrastructure layer, the same stub createSecrets above uses.
// The bot's own API key is here too: its SHA-256 is what the accretion-api
// identity `accretion-discord` carries, so this value and
// apiKeyHashes.accretion-discord must be rotated together.
func discordPlaceholders() pulumi.StringMap {
	data := pulumi.StringMap{}
	for _, key := range config.DiscordSecretKeys() {
		data[key] = pulumi.String("REPLACE_WITH_SECRET_MANAGER_REF")
	}
	return data
}

// createRedisSecrets provisions the collector queue's password.
//
// Without it the release takes the chart's default redis.auth.password, which
// is CHECKED INTO THIS REPOSITORY -- and this lane is what made that live, since
// before it no collector release rendered at all. The chart deploys Redis under
// scale-out, which the split log and metric collectors enable, so every
// collector deploy would otherwise run a queue behind a published password.
//
// Two keys, because redis-server has no env-var form of requirepass: the raw
// password for clients, and a config-file directive mounted for the server.
// config.RedisSecretData derives both from one value, so they cannot disagree --
// the consistency the chart guarantees for its own Secret and explicitly hands
// to whoever supplies an existingSecret.
func createRedisSecrets(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
) (*corev1.Secret, error) {
	data := pulumi.StringMap{}
	// Placeholder, like every other Secret here: in production these come from
	// GCP Secret Manager via the infrastructure layer.
	for key, value := range config.RedisSecretData("REPLACE_WITH_SECRET_MANAGER_REF") {
		data[key] = pulumi.String(value)
	}

	secret, err := corev1.NewSecret(ctx, config.RedisSecretName, &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(config.RedisSecretName),
			Namespace: pulumi.String(cfg.App.Namespace),
			Labels: pulumi.StringMap{
				labelManagedBy: pulumi.String("pulumi"),
				labelPartOf:    pulumi.String("accretion-api"),
			},
		},
		Type:       pulumi.String("Opaque"),
		StringData: data,
	}, pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{ns}))
	if err != nil {
		return nil, err
	}
	return secret, nil
}
