package kubernetes

import (
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func NewProvider(ctx *pulumi.Context, kubeconfig pulumi.StringOutput) (*k8s.Provider, error) {
	provider, err := k8s.NewProvider(ctx, "gke-provider", &k8s.ProviderArgs{
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}
