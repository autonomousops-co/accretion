# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release tags carry a `v` prefix (`v1.2.3`): Go resolves module versions only from
v-prefixed tags, while Helm writes `appVersion` bare. `scripts/version.sh` reports
which release a binary descends from, and `scripts/check-chart-app-version.sh`
refuses a tag whose name disagrees with the `appVersion` of the charts that ship
the accretion binary — so the `appVersion` bump belongs in the same pull request
as the release.

## [Unreleased]

### Added

- The database password is sourced from a Kubernetes Secret built from a
  gitignored `.env`, never from a values file, across `accretion-api`,
  `accretion-collector` and `accretion-db` (#134).
- The Discord bot's token and model API keys are delivered as Secret references
  rather than inline values (#135).
- `accretion generate config` names the secrets it deliberately did not write, so
  an operator can see what is still missing rather than discovering it at
  startup (#129).
- `CODE_OF_CONDUCT.md` — Contributor Covenant 2.1. Conduct reports route to the
  same private inbox as security reports, never a public issue (#133).

### Changed

- The generated config file carries no secret fields at all. Every secret is read
  from the environment (`.env`) or from a Helm-provisioned Secret (#129).
- Pulumi stack configs ship placeholders instead of one organization's GCP
  projects, cluster names and domain. Each file opens with a banner naming the
  three values that must be substituted and what fails if they are not: project
  IDs are globally unique, so the placeholder cannot quietly deploy somewhere
  unintended, and the RFC 2606 domain previews and validates but serves
  nothing (#128).
- `charts/accretion-api` renders its `db-password` Secret as base64 `data`
  rather than `stringData` (#134).
- Session-key errors name `SESSION_KEY` and where the value comes from (#129).
- The chart guard refusing an empty or default database password is
  unconditional. It was previously opt-in, and only `values-prod.yaml` opted in,
  so staging shipped the insecure default by design (#134).

### Fixed

- `pulumi/pkg/config/config.go` no longer defaults `domain` to the project
  owner's real domain. A code default is the one leak a stack-file scrub cannot
  reach: omitting the key silently handed back the original author's
  domain (#128).
- `charts/DKJR.md` no longer presents a private repository as a public one. The
  page now states that the fixture repository is internal, so a reader knows the
  clone and install lines are not for them rather than assuming the link is
  broken (#130).
- `internal/k8s/statusinformers.go` claimed Secrets were among the watched
  streams in one comment while two others said they are not collected at all.
  The watch list now names only what is watched — nine streams per namespace,
  matching the informers the file actually constructs — and the measured
  footprint figures are marked as predating the Secret informer's removal rather
  than presented as current (#132).

### Documentation

- `ResourceTypeSecret` is documented at its declaration as a read-only path,
  retained so rows written before Secrets left monitoring scope stay queryable.
  It previously read as a live resource type, with the explanation two hundred
  lines away (#131).
- `config.example.yaml` contains no secret fields, and the claim that the
  generated config holds a signing key is corrected (#129).

### Security

- `pulumi/Pulumi.*.yaml` is gitignored. `pulumi/scripts/deploy.sh` copies a
  placeholder stack config to that path for an operator to fill in with their own
  project, cluster and domain; the destination was previously committable, so a
  substituted file was one `git add .` away from being published (#128).
- No chart values file carries a database password placeholder that is also a
  working credential (#134).

## [1.0.0] — 2026-09-13

Initial release: Accretion is an observability store that gives agents memory of a
Kubernetes cluster, with a Discord bot and an MCP server over `accretion-api`.

Development history prior to this tag was squashed into a single commit for the
open-source release, so this entry summarises it rather than enumerating it.

### Added

- The memory store: a unified entries table with pgvector embeddings and RAG
  search, partitioned with a retention policy.
- Ingestion: informer-driven status collectors covering pods, deployments,
  replicasets, HPAs, endpointslices, PVCs, ConfigMaps, jobs and cronjobs per
  namespace, plus cluster-scoped nodes and persistent volumes, with a worker
  queue behind them.
- Agent access over MCP, and a Discord bot that renders tabular answers as
  aligned monospace.
- Authentication: per-client scoped API-key identities with hashed storage and
  per-route permission enforcement, OAuth with ID-token verification, and TLS
  terminated in-process on the API listener.
- Helm charts for `accretion-api`, `accretion-collector`, `accretion-db`,
  `accretion-discord` and `otel-collector`; a Tilt and kind local stack; and a
  Pulumi program for GKE.
- CI, release and security workflows; chart-version, credential-plaintext,
  values-matrix and render gates; `make bootstrap` as single-command onboarding.
- `CONTRIBUTING.md`, `SECURITY.md`, `CLA.md`, dual AGPL-3.0 and commercial
  licensing, CODEOWNERS, and issue and pull-request templates.

### Security

- Cluster Secrets are out of monitoring scope entirely: no informer watches them
  and no chart role grants a verb on them.
- No plaintext password in any ConfigMap or container command; API-key plaintext
  moved out of chart values into `.env`.

[Unreleased]: https://github.com/autonomousops-co/accretion/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/autonomousops-co/accretion/releases/tag/v1.0.0
