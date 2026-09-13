package main

import (
	"accretion-infrastructure/pkg/config"
	"accretion-infrastructure/pkg/infrastructure"
	k8sresources "accretion-infrastructure/pkg/kubernetes"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/container"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.NewConfig(ctx)

		// Deploy GCP infrastructure (VPC, GKE, DNS, Secrets)
		infraOutputs, err := infrastructure.Deploy(ctx, cfg)
		if err != nil {
			return err
		}

		// Build kubeconfig from GKE cluster
		kubeconfig := buildKubeconfig(infraOutputs.Cluster)

		// Create K8s provider from kubeconfig
		k8sProvider, err := k8sresources.NewProvider(ctx, kubeconfig)
		if err != nil {
			return err
		}

		// Wire infrastructure outputs to kubernetes layer.
		//
		// The NAME of the reserved global address, not its address. This feeds
		// the chart's ingress.globalStaticIP, which becomes the GCE annotation
		// kubernetes.io/ingress.global-static-ip-name -- and GCE resolves that
		// against a named address in the project. Given a literal IP it finds no
		// such address and the Ingress is provisioned with an ephemeral one
		// instead, so the reserved address the DNS records point at goes unused.
		staticIP := infraOutputs.StaticIP.Name
		k8sInfraOutputs := &k8sresources.InfraOutputs{
			Kubeconfig:   kubeconfig,
			StaticIP:     staticIP,
			ClusterName:  infraOutputs.Cluster.Name,
			DatabaseHost: pulumi.String("postgresql").ToStringOutput(),
		}

		// Deploy Kubernetes resources
		err = k8sresources.Deploy(ctx, cfg, k8sProvider, k8sInfraOutputs)
		if err != nil {
			return err
		}

		// Export useful outputs
		ctx.Export("clusterName", infraOutputs.Cluster.Name)
		ctx.Export("clusterEndpoint", infraOutputs.Cluster.Endpoint)
		ctx.Export("staticIP", infraOutputs.StaticIP.Address)
		ctx.Export("namespace", pulumi.String(cfg.App.Namespace))
		ctx.Export("environment", pulumi.String(cfg.Environment))

		return nil
	})
}

func buildKubeconfig(cluster *container.Cluster) pulumi.StringOutput {
	combined := pulumi.All(
		cluster.Name, cluster.Endpoint, cluster.MasterAuth,
	)
	return combined.ApplyT( //nolint:errcheck // type-asserted by caller
		func(args []any) (string, error) {
			name := args[0].(string)
			endpoint := args[1].(string)
			masterAuth := args[2].(container.ClusterMasterAuth)
			caCert := ""
			if masterAuth.ClusterCaCertificate != nil {
				caCert = *masterAuth.ClusterCaCertificate
			}
			return generateKubeconfig(name, endpoint, caCert), nil
		},
	).(pulumi.StringOutput)
}

func generateKubeconfig(clusterName, endpoint, caCert string) string {
	return "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- cluster:\n" +
		"    server: https://" + endpoint + "\n" +
		"    certificate-authority-data: " + caCert + "\n" +
		"  name: " + clusterName + "\n" +
		"contexts:\n- context:\n" +
		"    cluster: " + clusterName + "\n" +
		"    user: " + clusterName + "\n" +
		"  name: " + clusterName + "\n" +
		"current-context: " + clusterName + "\n" +
		"users:\n- name: " + clusterName + "\n" +
		"  user:\n    exec:\n" +
		"      apiVersion: client.authentication.k8s.io/v1beta1\n" +
		"      command: gke-gcloud-auth-plugin\n" +
		"      provideClusterInfo: true\n"
}
