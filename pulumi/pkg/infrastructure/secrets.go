package infrastructure

import (
	"accretion-infrastructure/pkg/config"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/secretmanager"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type SecretsOutputs struct {
	DatabasePassword *secretmanager.Secret
	ApiKey           *secretmanager.Secret
	SessionSecret    *secretmanager.Secret
}

func DeploySecrets(ctx *pulumi.Context, cfg *config.Config) (*SecretsOutputs, error) {
	prefix := cfg.ProjectName + "-" + cfg.Environment

	secretNames := []string{"database-password", "api-key", "session-secret"}
	secrets := make([]*secretmanager.Secret, len(secretNames))

	for i, name := range secretNames {
		s, err := secretmanager.NewSecret(ctx, prefix+"-"+name, &secretmanager.SecretArgs{
			SecretId: pulumi.String(prefix + "-" + name),
			Project:  pulumi.String(cfg.GCP.Project),
			Replication: &secretmanager.SecretReplicationArgs{
				Auto: &secretmanager.SecretReplicationAutoArgs{},
			},
		})
		if err != nil {
			return nil, err
		}
		secrets[i] = s
	}

	return &SecretsOutputs{
		DatabasePassword: secrets[0],
		ApiKey:           secrets[1],
		SessionSecret:    secrets[2],
	}, nil
}
