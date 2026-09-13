package infrastructure

import (
	"accretion-infrastructure/pkg/config"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/dns"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type DnsOutputs struct {
	StaticIP    *compute.GlobalAddress
	ManagedZone *dns.ManagedZone
}

func DeployDns(ctx *pulumi.Context, cfg *config.Config) (*DnsOutputs, error) {
	prefix := cfg.ProjectName + "-" + cfg.Environment

	staticIP, err := compute.NewGlobalAddress(ctx, prefix+"-static-ip", &compute.GlobalAddressArgs{
		Name:    pulumi.String(prefix + "-static-ip"),
		Project: pulumi.String(cfg.GCP.Project),
	})
	if err != nil {
		return nil, err
	}

	managedZone, err := dns.NewManagedZone(ctx, cfg.DNS.ManagedZone, &dns.ManagedZoneArgs{
		Name:        pulumi.String(cfg.DNS.ManagedZone),
		Project:     pulumi.String(cfg.GCP.Project),
		DnsName:     pulumi.String(cfg.DNS.Domain + "."),
		Description: pulumi.Sprintf("Managed zone for %s", cfg.DNS.Domain),
	})
	if err != nil {
		return nil, err
	}

	_, err = dns.NewRecordSet(ctx, prefix+"-a-record", &dns.RecordSetArgs{
		Name:        pulumi.String(cfg.DNS.Domain + "."),
		Project:     pulumi.String(cfg.GCP.Project),
		ManagedZone: managedZone.Name,
		Type:        pulumi.String("A"),
		Ttl:         pulumi.Int(300),
		Rrdatas:     pulumi.StringArray{staticIP.Address},
	})
	if err != nil {
		return nil, err
	}

	ctx.Export("staticIp", staticIP.Address)

	return &DnsOutputs{
		StaticIP:    staticIP,
		ManagedZone: managedZone,
	}, nil
}
