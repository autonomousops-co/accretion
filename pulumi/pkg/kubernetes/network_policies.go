package kubernetes

import (
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"accretion-infrastructure/pkg/config"
)

// keyAppName is the standard Kubernetes name label, used as a pod selector
// key throughout the policies below.
const keyAppName = "app.kubernetes.io/name"

func createNetworkPolicies(
	ctx *pulumi.Context,
	cfg *config.Config,
	provider *k8s.Provider,
	ns *corev1.Namespace,
) error {
	opts := []pulumi.ResourceOption{
		pulumi.Provider(provider),
		pulumi.DependsOn([]pulumi.Resource{ns}),
	}
	namespace := cfg.App.Namespace

	if err := createDenyAllIngressPolicy(ctx, namespace, opts); err != nil {
		return err
	}
	if err := createAccretionPostgresPolicy(ctx, namespace, opts); err != nil {
		return err
	}
	return createCollectorPolicies(ctx, namespace, opts)
}

func createDenyAllIngressPolicy(
	ctx *pulumi.Context,
	namespace string,
	opts []pulumi.ResourceOption,
) error {
	_, err := networkingv1.NewNetworkPolicy(
		ctx, "default-deny-ingress", &networkingv1.NetworkPolicyArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("default-deny-ingress"),
				Namespace: pulumi.String(namespace),
			},
			Spec: &networkingv1.NetworkPolicySpecArgs{
				PodSelector: &metav1.LabelSelectorArgs{},
				PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
			},
		}, opts...)
	return err
}

func createAccretionPostgresPolicy(
	ctx *pulumi.Context,
	namespace string,
	opts []pulumi.ResourceOption,
) error {
	_, err := networkingv1.NewNetworkPolicy(
		ctx, "allow-accretion-api-to-postgresql", &networkingv1.NetworkPolicyArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("allow-accretion-api-to-postgresql"),
				Namespace: pulumi.String(namespace),
			},
			Spec: &networkingv1.NetworkPolicySpecArgs{
				PodSelector: &metav1.LabelSelectorArgs{
					MatchLabels: pulumi.StringMap{
						keyAppName: pulumi.String("postgresql"),
					},
				},
				PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
				Ingress: networkingv1.NetworkPolicyIngressRuleArray{
					&networkingv1.NetworkPolicyIngressRuleArgs{
						From: networkingv1.NetworkPolicyPeerArray{
							&networkingv1.NetworkPolicyPeerArgs{
								PodSelector: &metav1.LabelSelectorArgs{
									MatchLabels: pulumi.StringMap{
										keyAppName: pulumi.String("accretion-api"),
									},
								},
							},
						},
						Ports: networkingv1.NetworkPolicyPortArray{
							&networkingv1.NetworkPolicyPortArgs{
								Port:     pulumi.Int(5432),
								Protocol: pulumi.String("TCP"),
							},
						},
					},
				},
			},
		}, opts...)
	return err
}

func createCollectorPolicies(
	ctx *pulumi.Context,
	namespace string,
	opts []pulumi.ResourceOption,
) error {
	_, err := networkingv1.NewNetworkPolicy(
		ctx, "allow-collector-to-redis", &networkingv1.NetworkPolicyArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("allow-collector-to-redis"),
				Namespace: pulumi.String(namespace),
			},
			Spec: &networkingv1.NetworkPolicySpecArgs{
				PodSelector: &metav1.LabelSelectorArgs{
					MatchLabels: pulumi.StringMap{
						keyAppName: pulumi.String("redis"),
					},
				},
				PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
				Ingress: networkingv1.NetworkPolicyIngressRuleArray{
					&networkingv1.NetworkPolicyIngressRuleArgs{
						From: networkingv1.NetworkPolicyPeerArray{
							&networkingv1.NetworkPolicyPeerArgs{
								PodSelector: &metav1.LabelSelectorArgs{
									MatchLabels: pulumi.StringMap{
										"app.kubernetes.io/component": pulumi.String("collector"),
									},
								},
							},
						},
						Ports: networkingv1.NetworkPolicyPortArray{
							&networkingv1.NetworkPolicyPortArgs{
								Port:     pulumi.Int(6379),
								Protocol: pulumi.String("TCP"),
							},
						},
					},
				},
			},
		}, opts...)
	if err != nil {
		return err
	}

	_, err = networkingv1.NewNetworkPolicy(
		ctx, "allow-collector-to-postgresql", &networkingv1.NetworkPolicyArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("allow-collector-to-postgresql"),
				Namespace: pulumi.String(namespace),
			},
			Spec: &networkingv1.NetworkPolicySpecArgs{
				PodSelector: &metav1.LabelSelectorArgs{
					MatchLabels: pulumi.StringMap{
						keyAppName: pulumi.String("postgresql"),
					},
				},
				PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
				Ingress: networkingv1.NetworkPolicyIngressRuleArray{
					&networkingv1.NetworkPolicyIngressRuleArgs{
						From: networkingv1.NetworkPolicyPeerArray{
							&networkingv1.NetworkPolicyPeerArgs{
								PodSelector: &metav1.LabelSelectorArgs{
									MatchLabels: pulumi.StringMap{
										"app.kubernetes.io/component": pulumi.String("collector"),
									},
								},
							},
						},
						Ports: networkingv1.NetworkPolicyPortArray{
							&networkingv1.NetworkPolicyPortArgs{
								Port:     pulumi.Int(5432),
								Protocol: pulumi.String("TCP"),
							},
						},
					},
				},
			},
		}, opts...)
	return err
}
