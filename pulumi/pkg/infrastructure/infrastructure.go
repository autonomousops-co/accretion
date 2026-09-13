package infrastructure

import (
	"accretion-infrastructure/pkg/config"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/container"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type Outputs struct {
	Cluster        *container.Cluster
	NodePool       *container.NodePool
	Network        *compute.Network
	NodeSubnet     *compute.Subnetwork
	StaticIP       *compute.GlobalAddress
	VpcOutputs     *VpcOutputs
	GkeOutputs     *GkeOutputs
	DnsOutputs     *DnsOutputs
	SecretsOutputs *SecretsOutputs
}

func Deploy(ctx *pulumi.Context, cfg *config.Config) (*Outputs, error) {
	vpcOutputs, err := DeployVpc(ctx, cfg)
	if err != nil {
		return nil, err
	}

	gkeOutputs, err := DeployGke(ctx, cfg, vpcOutputs)
	if err != nil {
		return nil, err
	}

	dnsOutputs, err := DeployDns(ctx, cfg)
	if err != nil {
		return nil, err
	}

	secretsOutputs, err := DeploySecrets(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Export kubeconfig
	kubeconfigOutput := pulumi.All( //nolint:errcheck // ApplyT type-asserted below
		gkeOutputs.Cluster.Name,
		gkeOutputs.Cluster.Endpoint,
		gkeOutputs.Cluster.MasterAuth,
	).ApplyT(func(args []any) string {
		name := args[0].(string)
		endpoint := args[1].(string)
		masterAuth := args[2].(container.ClusterMasterAuth)
		caCert := ""
		if masterAuth.ClusterCaCertificate != nil {
			caCert = *masterAuth.ClusterCaCertificate
		}
		return generateKubeconfig(name, endpoint, caCert)
	}).(pulumi.StringOutput)
	ctx.Export("kubeconfig", kubeconfigOutput)

	return &Outputs{
		Cluster:        gkeOutputs.Cluster,
		NodePool:       gkeOutputs.NodePool,
		Network:        vpcOutputs.Network,
		NodeSubnet:     vpcOutputs.NodeSubnet,
		StaticIP:       dnsOutputs.StaticIP,
		VpcOutputs:     vpcOutputs,
		GkeOutputs:     gkeOutputs,
		DnsOutputs:     dnsOutputs,
		SecretsOutputs: secretsOutputs,
	}, nil
}

func generateKubeconfig(clusterName, endpoint, caCert string) string {
	return `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://` + endpoint + `
    certificate-authority-data: ` + caCert + `
  name: ` + clusterName + `
contexts:
- context:
    cluster: ` + clusterName + `
    user: ` + clusterName + `
  name: ` + clusterName + `
current-context: ` + clusterName + `
users:
- name: ` + clusterName + `
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      installHint: Install gke-gcloud-auth-plugin for kubectl
      provideClusterInfo: true
`
}
