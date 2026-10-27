# `charts/` — deployment (Helm)

Accretion runs as a set of Kubernetes Deployments, one per role, packaged as Helm charts.
Local development is driven by [Tilt](https://tilt.dev/) on a [kind](https://kind.sigs.k8s.io/)
cluster; see the [root Quick Start](../README.md#quick-start-local).

## Charts

| Chart | Deploys | Namespace (local) |
|---|---|---|
| [`accretion-db`](accretion-db) | PostgreSQL + pgvector, the `unified_entries` schema (post-install Job), and the partition-maintenance CronJob | `accretion-db` |
| [`accretion-collector`](accretion-collector) | The collectors: `-data-collector` (log, metric, event), `-trace-collector` (the OTLP receiver), `-status-collector` (the eight object-state kinds, informer-driven), plus the `collector` controller and a Redis instance under scale-out only | `accretion-core` |
| [`accretion-api`](accretion-api) | The REST/RAG API server (+ optional Ollama) | `accretion-core` |
| [`accretion-discord`](accretion-discord) | The conversational Discord bot | `accretion-core` |
| `otel-collector` | Observability backend — OpenTelemetry Collector, Grafana, Tempo, Prometheus, Loki | `obs` |

The collectors run as three Deployments: `-data-collector` runs log, metric and event,
`-trace-collector` runs the OTLP trace receiver alone (split for isolation: the unauthenticated
listener must not share the log collector's `pods/log` identity, #100), and `-status-collector`
runs the eight object-state kinds. Every kind is still switched on and off individually
(`dataCollector.kinds.<name>.enabled`, `statusCollector.kinds.<name>.enabled`), and any data
collector can be given its own scaled Deployment with `dataCollector.kinds.<name>.split=true`.
The `-controller` and `-redis-master` coordinate sharding across scaled collectors.

Cross-namespace wiring: workers and the API reach Postgres at
`accretion-db-postgresql.accretion-db.svc.cluster.local` and the DB name is `accretion`.

## Configuration

- Non-secret settings come from each chart's `values.yaml`, plus whatever the deploy path layers on
  top: [`deploy/tilt.yaml`](../deploy/tilt.yaml) for the Tilt/kind stack and
  [`deploy/pulumi.yaml`](../deploy/pulumi.yaml) for the Pulumi path. Each is one file nested by
  chart name, and the set of top-level keys must match the set of charts that path deploys exactly —
  Helm ignores an unknown value key silently, so a key naming no chart fails the deploy by name
  instead. Application config is passed as `ACCRETION_*` env vars / a mounted config.
- Only `accretion-api` still ships per-environment files (`values-dev.yaml`, `values-staging.yaml`,
  `values-prod.yaml`, plus a standalone `values-example.yaml`). `accretion-collector`,
  `accretion-discord` and `accretion-db` ship none, so a bare `helm install` of one of them gets
  chart defaults and nothing else. The collector's and the bot's per-deploy settings live in the two
  files above; `accretion-db` runs on chart defaults plus the two Secret names #134 gave it in
  `deploy/tilt.yaml` (its subtree there was an empty map until then, carried only so the key set
  still matches the chart set exactly). `otel-collector`'s `values-minimal.yaml` is a profile, not
  an environment.
- **None of `accretion-api`'s three environment files render standalone**, by design: all three
  declare `apiKeys: []` and the chart refuses to render an authenticated deployment with no
  identities, so each needs deploy-time `--set-json apiKeys=[...]` (hashes only —
  `security.forbidPlaintextAPIKeys` rejects a plaintext `key` anyway, since it defaults to `true`
  chart-wide; staging and prod also set it explicitly, redundantly and harmlessly, and dev inherits
  it rather than opting out). **`values-prod.yaml` and `values-staging.yaml` need more than the
  identities**: since #134 the database-password guard is unconditional — it used to be opt-in via
  `security.enforceStrongDbPassword`, which only prod turned on, so dev and staging rendered with a
  checked-in `postgres` by design and that flag is now gone — so each refuses to render again until
  the application database password is a real one (`--set-string config.database.password=...`, or a
  `config.database.existingSecret`). The `accretion-api` chart no longer has a second,
  `postgresql.auth.postgresPassword` refusal: #134's review removed that guard here, because the
  chart stands up no PostgreSQL of its own (the key fed a check on a component that is not there).
  The refusals are raised one at
  a time and each names its own remedy, so supplying the identities alone gets you the next error
  rather than a render. `values-dev.yaml` and `values-example.yaml` do render as-is, and not because
  they carry a password: both name `config.database.existingSecret: accretion-api-secrets`, the
  Secret [`scripts/make-secret.sh`](../scripts/make-secret.sh) builds from
  `ACCRETION_DATABASE_PASSWORD` in your gitignored `.env`. `values-example.yaml` additionally
  carries its own identity, whose all-zeros digest has no preimage, so an unmodified example install
  authenticates nobody.
- Secrets (`ACCRETION_LLM_OPENAI_API_KEY`, OAuth, session key) come from a Kubernetes Secret
  (`accretion-api-secrets`), created by [`scripts/make-secret.sh`](../scripts/make-secret.sh) — the
  one mapping from `.env`'s SHELL_CASE names to the chart's dashed Secret keys. Of the two model
  keys only the OpenAI one reaches the API server here: `ACCRETION_LLM_ANTHROPIC_API_KEY` and the
  Discord token are published to `accretion-discord-secrets`, not `accretion-api-secrets`.
- **Supplying the password as a Helm value is the escape hatch, not the path.** Every guard below
  points here rather than advertising it, because the three charts name one remedy and this file
  carries the caveat once. `--set-string config.database.password=<value>` (and
  `--set-string postgresql.auth.postgresPassword=<value>` for `accretion-db`) renders fine and puts
  the value in your shell history and in the Helm release, where `helm get values` returns it in
  cleartext to anyone who can reach the release — the same defect #135 removed from the Discord
  bot's side. Use it for a throwaway render; use `existingSecret` for anything that runs.
- **No chart carries a database password** (#134). Six checked-in values files shipped `postgres`
  until then, so a default `helm install` stood up a database whose password is public knowledge;
  `accretion-api`, `accretion-collector` and `accretion-db` now refuse to render with neither a
  password nor an `existingSecret`, and none of them generates one. The single value is
  `ACCRETION_DATABASE_PASSWORD` in your gitignored `.env`, and `make-secret.sh` publishes it as
  `db-password` in `accretion-api-secrets` (read by `accretion-api` and every collector) and as
  `db-password` + `postgres-password` in `accretion-db-secrets` in the `accretion-db` namespace
  (read by the schema Job, the partition CronJob, and the Postgres server itself — a Secret does not
  cross a namespace, so the same password is published in both).
- **API-key plaintext is not a chart value at all.** `accretion-api`'s `apiKeys` list carries only
  each identity's SHA-256 `hash`, which the chart renders into `auth.api_key_identities` in the
  ConfigMap; the plaintext lives in your gitignored `.env`
  (`ACCRETION_API_KEY_MCP`, `ACCRETION_API_KEY_DISCORD`, `ACCRETION_API_KEY_CI`). The same
  `make-secret.sh` is the single mapping from there to both halves — `--print-identities` emits the
  `apiKeys` block your `.env` implies, and the script publishes each client's own plaintext in its
  own Secret: `accretion-mcp-secrets` (key `ACCRETION_API_KEY`, which `ACCRETION_MCP_SECRET_NAME`
  resolves — see [../internal/mcp](../internal/mcp/README.md)) and `accretion-discord-secrets`
  (key `api-key-accretion-discord`, which the bot's `api.secretName`/`api.secretKey` are pointed at
  on both the Tilt and Pulumi paths). Because the two halves are now two inputs that can disagree —
  whose only symptom is a 401 with nothing explaining it — `make secrets` refuses to create either
  Secret unless every identity's hash is the digest of its plaintext in `.env`.
- The chart-owned `accretion-api-client-keys` Secret is rendered **only** for an identity declaring
  a plaintext `key`, which `security.forbidPlaintextAPIKeys` forbids by default, so no deploy path
  produces it. The `accretion-discord` chart's `api.secretName` default still names it; every deploy
  path overrides that to `accretion-discord-secrets` above.
- **No secret reaches the Discord bot as a Helm value** (#135). `accretion-discord-secrets` carries
  all four values the bot reads — `api-key-accretion-discord`, `discord-bot-token`,
  `anthropic-api-key` and `openai-api-key` — and `make-secret.sh` builds every one of them from
  `.env`, skipping any it does not set. The chart's own `secret.botToken` / `secret.anthropicApiKey`
  / `secret.openaiApiKey` values still exist for a bare `helm install`, and no path here takes them:
  both `deploy/tilt.yaml` and `deploy/pulumi.yaml` set `secret.create: false`, name that Secret, and
  declare the two model-key env vars as `env` entries pointing at it (the chart wires those two
  itself only when the plaintext is supplied as a value). The Tiltfile passed the token and the
  Anthropic key as plaintext `--set` flags until #135, which put them in the Helm release where
  `helm get values accretion-discord` returns them in cleartext.
- The DB schema is delivered as a ConfigMap + Job; partitioning/retention is tunable under
  `partitionMaintenance` in `accretion-db/values.yaml`.
- **`accretion-collector`'s database and Redis passwords render into chart-owned Secrets when you
  supply them as `config.database.password` / `redis.auth.password`** — and neither has a default
  any more, so that path means supplying a value, not inheriting one. Even `--set-string
  config.database.password=<value>` puts the value in shell history and in the Helm release Secret,
  where `helm get values` returns it in cleartext. **For production**, set `config.database.existingSecret`
  and/or `redis.auth.existingSecret` instead: the chart then renders no Secret of its own for that
  credential and never sees, renders, or interpolates the value anywhere — only a Secret name and key,
  which you provision out of band (`kubectl create secret generic`, External Secrets Operator, Vault, or
  SOPS). The Redis Secret needs **two** keys, not one — `password` (read by every client via
  `secretKeyRef`) and `redis.conf` (the literal `requirepass "<password>"` directive, mounted as a file,
  since `redis-server` has no env-var form of `requirepass`) — both documented and their key *names*
  asserted non-empty at render time in `accretion-collector/values.yaml`. See the `existingSecret*`
  comments there for the full tradeoff, including which validation (an empty or malformed password)
  moves to the operator once the chart stops seeing the value, and the root [README's cluster-deployment
  section](../README.md#quick-start-cluster-deployment) for a worked example.

## Local development (Tilt + kind)

From the repo root:

```bash
make kind-up     # create the `accretion-dev` kind cluster + local registry (scripts/kind-with-registry.sh)
make tilt-up     # build images and deploy all charts (Tiltfile); make tilt-server-only skips the collectors
make tilt-down   # tear down
```

The [Tiltfile](../Tiltfile) needs nothing checked out beside this repo. It points LLM calls at a
host Ollama, and deploys the API with **authentication on** — so a wrong or under-scoped key gets a
real 401/403 locally rather than only in a cluster. It derives the dev identities from your `.env`
at load time (`scripts/make-secret.sh --print-identities`), the same input the Secrets it creates
come from, so the digest the server compares against and the plaintext each client sends cannot
drift. A fresh clone holds no credential by design: generate one per caller with
`openssl rand -hex 32` into `.env` first, or Tilt warns and the `accretion-api` install refuses to
render with the chart's own fail-closed message.

Every static Helm value for the local stack lives in [`deploy/tilt.yaml`](../deploy/tilt.yaml),
nested by chart name and split per chart at load. No digest is written there — it is credential
material derived from a plaintext that exists only in your `.env`, and that file is checked in.

For synthetic workloads that give the collectors something interesting to record, install the DKJR
chaos fixture on demand from its own repo — see [DKJR.md](DKJR.md). It is not part of the default
bring-up.

## Cloud / production

Cluster and cloud resources (GKE, VPC, DNS, secrets) are provisioned with Pulumi — see
[../pulumi/README.md](../pulumi/README.md). The same charts are applied on top.

## See also

- [accretion-db/PARTITIONING.md](accretion-db/PARTITIONING.md) — weekly partitioning, retention, and the one-time migration.
- [DKJR.md](DKJR.md) — the DKJR chaos generator: a dev fixture, installed on demand from its own repo.
