package infrastructure

import (
	"fmt"

	"accretion-infrastructure/pkg/config"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/compute"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type VpcOutputs struct {
	Network       *compute.Network
	NodeSubnet    *compute.Subnetwork
	PodSubnet     *compute.Subnetwork
	ServiceSubnet *compute.Subnetwork
}

func DeployVpc(ctx *pulumi.Context, cfg *config.Config) (*VpcOutputs, error) {
	prefix := fmt.Sprintf("%s-%s", cfg.ProjectName, cfg.Environment)

	network, err := compute.NewNetwork(ctx, cfg.Network.VpcName, &compute.NetworkArgs{
		Name:                  pulumi.String(cfg.Network.VpcName),
		Project:               pulumi.String(cfg.GCP.Project),
		AutoCreateSubnetworks: pulumi.Bool(false),
	})
	if err != nil {
		return nil, err
	}

	subnets, err := deploySubnets(ctx, cfg, prefix, network)
	if err != nil {
		return nil, err
	}

	if err := deployNatGateway(ctx, cfg, prefix, network); err != nil {
		return nil, err
	}

	if err := deployFirewallRules(ctx, cfg, prefix, network); err != nil {
		return nil, err
	}

	ctx.Export("vpcId", network.ID())
	ctx.Export("nodeSubnetId", subnets.NodeSubnet.ID())

	return &VpcOutputs{
		Network:       network,
		NodeSubnet:    subnets.NodeSubnet,
		PodSubnet:     subnets.PodSubnet,
		ServiceSubnet: subnets.ServiceSubnet,
	}, nil
}

type subnetOutputs struct {
	NodeSubnet    *compute.Subnetwork
	PodSubnet     *compute.Subnetwork
	ServiceSubnet *compute.Subnetwork
}

func deploySubnets(
	ctx *pulumi.Context,
	cfg *config.Config,
	prefix string,
	network *compute.Network,
) (*subnetOutputs, error) {
	nodeSubnet, err := compute.NewSubnetwork(ctx, prefix+"-nodes", &compute.SubnetworkArgs{
		Name:                  pulumi.String(prefix + "-nodes"),
		Project:               pulumi.String(cfg.GCP.Project),
		Region:                pulumi.String(cfg.GCP.Region),
		Network:               network.ID(),
		IpCidrRange:           pulumi.String(cfg.Network.NodeCidr),
		PrivateIpGoogleAccess: pulumi.Bool(true),
	})
	if err != nil {
		return nil, err
	}

	podSubnet, err := compute.NewSubnetwork(ctx, prefix+"-pods", &compute.SubnetworkArgs{
		Name:                  pulumi.String(prefix + "-pods"),
		Project:               pulumi.String(cfg.GCP.Project),
		Region:                pulumi.String(cfg.GCP.Region),
		Network:               network.ID(),
		IpCidrRange:           pulumi.String(cfg.Network.PodCidr),
		PrivateIpGoogleAccess: pulumi.Bool(true),
	})
	if err != nil {
		return nil, err
	}

	serviceSubnet, err := compute.NewSubnetwork(ctx, prefix+"-services", &compute.SubnetworkArgs{
		Name:                  pulumi.String(prefix + "-services"),
		Project:               pulumi.String(cfg.GCP.Project),
		Region:                pulumi.String(cfg.GCP.Region),
		Network:               network.ID(),
		IpCidrRange:           pulumi.String(cfg.Network.ServiceCidr),
		PrivateIpGoogleAccess: pulumi.Bool(true),
	})
	if err != nil {
		return nil, err
	}

	return &subnetOutputs{
		NodeSubnet:    nodeSubnet,
		PodSubnet:     podSubnet,
		ServiceSubnet: serviceSubnet,
	}, nil
}

func deployNatGateway(
	ctx *pulumi.Context,
	cfg *config.Config,
	prefix string,
	network *compute.Network,
) error {
	router, err := compute.NewRouter(ctx, prefix+"-router", &compute.RouterArgs{
		Name:    pulumi.String(prefix + "-router"),
		Project: pulumi.String(cfg.GCP.Project),
		Region:  pulumi.String(cfg.GCP.Region),
		Network: network.ID(),
	})
	if err != nil {
		return err
	}

	_, err = compute.NewRouterNat(ctx, prefix+"-nat", &compute.RouterNatArgs{
		Name:                          pulumi.String(prefix + "-nat"),
		Project:                       pulumi.String(cfg.GCP.Project),
		Region:                        pulumi.String(cfg.GCP.Region),
		Router:                        router.Name,
		NatIpAllocateOption:           pulumi.String("AUTO_ONLY"),
		SourceSubnetworkIpRangesToNat: pulumi.String("ALL_SUBNETWORKS_ALL_IP_RANGES"),
	})
	return err
}

func deployFirewallRules(
	ctx *pulumi.Context,
	cfg *config.Config,
	prefix string,
	network *compute.Network,
) error {
	_, err := compute.NewFirewall(ctx, prefix+"-allow-internal", &compute.FirewallArgs{
		Name:    pulumi.String(prefix + "-allow-internal"),
		Project: pulumi.String(cfg.GCP.Project),
		Network: network.SelfLink,
		Allows: compute.FirewallAllowArray{
			&compute.FirewallAllowArgs{
				Protocol: pulumi.String("tcp"),
				Ports:    pulumi.StringArray{pulumi.String("0-65535")},
			},
			&compute.FirewallAllowArgs{
				Protocol: pulumi.String("udp"),
				Ports:    pulumi.StringArray{pulumi.String("0-65535")},
			},
			&compute.FirewallAllowArgs{
				Protocol: pulumi.String("icmp"),
			},
		},
		SourceRanges: pulumi.StringArray{
			pulumi.String(cfg.Network.NodeCidr),
			pulumi.String(cfg.Network.PodCidr),
			pulumi.String(cfg.Network.ServiceCidr),
		},
	})
	if err != nil {
		return err
	}

	_, err = compute.NewFirewall(ctx, prefix+"-allow-health-checks", &compute.FirewallArgs{
		Name:    pulumi.String(prefix + "-allow-health-checks"),
		Project: pulumi.String(cfg.GCP.Project),
		Network: network.SelfLink,
		Allows: compute.FirewallAllowArray{
			&compute.FirewallAllowArgs{
				Protocol: pulumi.String("tcp"),
			},
		},
		SourceRanges: pulumi.StringArray{
			pulumi.String("130.211.0.0/22"),
			pulumi.String("35.191.0.0/16"),
		},
	})
	return err
}
