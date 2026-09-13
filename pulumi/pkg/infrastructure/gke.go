package infrastructure

import (
	"fmt"

	"accretion-infrastructure/pkg/config"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/container"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type GkeOutputs struct {
	Cluster  *container.Cluster
	NodePool *container.NodePool
}

func DeployGke(ctx *pulumi.Context, cfg *config.Config, vpc *VpcOutputs) (*GkeOutputs, error) {
	prefix := fmt.Sprintf("%s-%s", cfg.ProjectName, cfg.Environment)

	cluster, err := container.NewCluster(ctx, cfg.GKE.ClusterName, &container.ClusterArgs{
		Name:       pulumi.String(cfg.GKE.ClusterName),
		Project:    pulumi.String(cfg.GCP.Project),
		Location:   pulumi.String(cfg.GCP.Zone),
		Network:    vpc.Network.SelfLink,
		Subnetwork: vpc.NodeSubnet.SelfLink,

		// Private cluster config
		PrivateClusterConfig: &container.ClusterPrivateClusterConfigArgs{
			EnablePrivateNodes:    pulumi.Bool(true),
			EnablePrivateEndpoint: pulumi.Bool(false),
			MasterIpv4CidrBlock:   pulumi.String("172.16.0.0/28"),
		},

		// Workload Identity
		WorkloadIdentityConfig: &container.ClusterWorkloadIdentityConfigArgs{
			WorkloadPool: pulumi.Sprintf("%s.svc.id.goog", cfg.GCP.Project),
		},

		// Network policy (Calico)
		NetworkPolicy: &container.ClusterNetworkPolicyArgs{
			Enabled:  pulumi.Bool(true),
			Provider: pulumi.String("CALICO"),
		},

		// Release channel
		ReleaseChannel: &container.ClusterReleaseChannelArgs{
			Channel: pulumi.String("REGULAR"),
		},

		// IP allocation policy for VPC-native cluster
		IpAllocationPolicy: &container.ClusterIpAllocationPolicyArgs{
			ClusterSecondaryRangeName:  pulumi.String(prefix + "-pods"),
			ServicesSecondaryRangeName: pulumi.String(prefix + "-services"),
		},

		// Remove default node pool
		RemoveDefaultNodePool: pulumi.Bool(true),
		InitialNodeCount:      pulumi.Int(1),

		DeletionProtection: pulumi.Bool(false),
	})
	if err != nil {
		return nil, err
	}

	nodePool, err := container.NewNodePool(ctx, prefix+"-nodepool", &container.NodePoolArgs{
		Name:     pulumi.String(prefix + "-nodepool"),
		Project:  pulumi.String(cfg.GCP.Project),
		Location: pulumi.String(cfg.GCP.Zone),
		Cluster:  cluster.Name,

		InitialNodeCount: pulumi.Int(cfg.GKE.NodeCount),

		Autoscaling: &container.NodePoolAutoscalingArgs{
			MinNodeCount: pulumi.Int(cfg.GKE.MinNodes),
			MaxNodeCount: pulumi.Int(cfg.GKE.MaxNodes),
		},

		NodeConfig: &container.NodePoolNodeConfigArgs{
			MachineType: pulumi.String(cfg.GKE.MachineType),
			DiskSizeGb:  pulumi.Int(cfg.GKE.DiskSizeGb),
			DiskType:    pulumi.String("pd-standard"),

			OauthScopes: pulumi.StringArray{
				pulumi.String("https://www.googleapis.com/auth/monitoring"),
				pulumi.String("https://www.googleapis.com/auth/logging.write"),
				pulumi.String("https://www.googleapis.com/auth/devstorage.read_only"),
			},

			WorkloadMetadataConfig: &container.NodePoolNodeConfigWorkloadMetadataConfigArgs{
				Mode: pulumi.String("GKE_METADATA"),
			},
		},

		Management: &container.NodePoolManagementArgs{
			AutoRepair:  pulumi.Bool(true),
			AutoUpgrade: pulumi.Bool(true),
		},
	})
	if err != nil {
		return nil, err
	}

	ctx.Export("clusterName", cluster.Name)
	ctx.Export("clusterEndpoint", cluster.Endpoint)

	return &GkeOutputs{
		Cluster:  cluster,
		NodePool: nodePool,
	}, nil
}
