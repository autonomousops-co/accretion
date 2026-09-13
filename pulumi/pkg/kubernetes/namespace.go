package kubernetes

import (
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"accretion-infrastructure/pkg/config"
)

func createNamespace(ctx *pulumi.Context, cfg *config.Config, provider *k8s.Provider) (*corev1.Namespace, error) {
	ns, err := corev1.NewNamespace(ctx, cfg.App.Namespace, &corev1.NamespaceArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name: pulumi.String(cfg.App.Namespace),
			Labels: pulumi.StringMap{
				"app.kubernetes.io/managed-by": pulumi.String("pulumi"),
				"environment":                  pulumi.String(cfg.Environment),
			},
		},
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, err
	}
	return ns, nil
}
