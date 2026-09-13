# `pulumi/` — cloud infrastructure (GCP/GKE)

Infrastructure-as-Code that provisions the cloud substrate Accretion runs on and then applies its
Kubernetes layer. [Pulumi](https://www.pulumi.com/) with the Go SDK.

This is a **separate Go module** (`accretion-infrastructure`) from the application in
[`../src`](../README.md) — it does not import the app; it stands up the cluster the app's
[Helm charts](../charts/README.md) are deployed onto.

## What it provisions

`main.go` runs two layers in order (`pkg/infrastructure` → `pkg/kubernetes`):

1. **GCP infrastructure** (`pkg/infrastructure`, `infrastructure.Deploy`):
   - `vpc.go` — VPC and networking.
   - `gke.go` — the GKE cluster.
   - `dns.go` — DNS + a reserved global static IP for ingress.
   - `secrets.go` — GCP-side secret material.
2. **Kubernetes resources** (`pkg/kubernetes`), applied via a provider built from the new cluster's
   kubeconfig:
   - `provider.go` — k8s provider from the GKE kubeconfig.
   - `namespace.go` — namespaces.
   - `network_policies.go` — network policies (e.g. Postgres access scoping).
   - `secrets.go` — Kubernetes Secrets (from the GCP secrets).
   - `helm.go` — the Accretion Helm releases, wired to the static IP from layer 1.

`main.go` exports useful outputs (cluster endpoint, kubeconfig, static IP). Config is read via
`pkg/config`.

## Stacks

Per-environment stack configs: `stacks/Pulumi.dev.yaml`, `Pulumi.staging.yaml`, `Pulumi.prod.yaml`.
Project name `accretion-infrastructure` (`Pulumi.yaml`).

## Usage

Wrapper scripts live in `scripts/`:

```bash
cd pulumi
./scripts/deploy.sh     # pulumi up for the selected stack
./scripts/destroy.sh    # pulumi destroy
```

(Or drive Pulumi directly: `pulumi stack select <env>` then `pulumi up`.) Requires the Pulumi CLI,
authenticated `gcloud`, and the stack's config/secrets set.

## Relationship to the rest of the repo

```
pulumi (this module)  ──provisions──▶  GKE cluster + VPC + DNS + static IP + Secrets
                                              │
              charts/ (Helm) ──deploys onto──▶ Accretion workloads (db, collector+workers, api, discord, otel)
```

For local development you don't need Pulumi at all — use `make kind-up && make tilt-up`
([charts/README.md](../charts/README.md)). Pulumi is for real GCP environments.
