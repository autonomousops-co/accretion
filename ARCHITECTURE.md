# Architecture — Go codebase

Module `github.com/autonomousops-co/accretion`, Go 1.25. The module root is the repository
root (so `internal/database` imports as `github.com/autonomousops-co/accretion/internal/database`).
Dependencies are vendored; the Docker image builds with `-mod=vendor`.

For the product-level picture see the [root README](README.md). This document maps the code.

## Binaries (`cmd/`)

| Command | Entrypoint | What it is |
|---|---|---|
| `accretion-api` | `cmd/accretion-api/main.go` | The main binary. A cobra root command with many subcommands (below). |
| `accretion-mcp` | `cmd/accretion-mcp/main.go` | MCP server over stdio, proxying to the API. See [internal/mcp](internal/mcp/README.md). |

### `accretion-api` subcommands (`internal/cmd/`)

One binary, one process per role — there is no "run everything" command; each subcommand is a
separate Deployment in the cluster.

- **`server`** — the REST API + RAG chat + web UI ([internal/server](internal/server/README.md)).
- **`collector`** — watches Pods and dispatches log/metric work ([internal/collector](internal/collector)).
- **`data-collector`** — the log, metric, event and trace collectors as supervised goroutines in one
  process. `--only <name>` runs just one of them, which is how the chart splits a collector into its
  own scaled Deployment without a code change (`dataCollector.kinds.<name>.split`). The chart ships
  trace split out, so its unauthenticated OTLP listener never shares the log collector's
  cluster-wide `pods/log` identity (#100).
- **`status`** — one process running every enabled object-state collector (pod, node, deployment,
  HPA, endpoint health, PVC, configmap and job) as supervised goroutines. These were nine separate
  poller Deployments; they produced 1-3 records each against the metric worker's 1,297, so the
  isolation cost ~290Mi of duplicated Go runtime for roughly 2% of the data. Each kind is still
  switched on and off individually through `collectors.<kind>.enabled`, and a panicking collector is
  restarted on its own rather than taking the others down.
- **`discord`** — the conversational Discord bot ([internal/discord](internal/discord)).
- **`generate config`** — writes a default config file. It contains no secret (#129); the command
  names the environment variable that supplies each one it left empty.

The worker fleet is documented in [internal/worker](internal/worker/README.md).

## Packages (`internal/`), by role

**Capture (write path)**
- `collector` — Pod-informer controller + Redis work distribution for logs/metrics.
- `worker` — the worker types; embed-then-insert. The bulk of the ingest logic.
- `queue` — Redis work-dispatch bus + worker health/assignment coordination.
- `k8s` — client-go wrapper: shared informers for the Pod dispatcher and the eight status kinds (#96), List-polling for events; metrics-server client.
- `nsscope` — which namespaces are in scope (static or label/`*` selectors), with the self-ingestion exclusion.

**Store**
- `database` — the pgvector store: `unified_entries`, the `Interface`, vector search. See its [README](internal/database/README.md).

**Recall / serve (read path)**
- `server` — gorilla/mux HTTP API, RAG `chatHandler`, web UI.
- `llm` — provider abstraction (`ollama`/`openai`/`anthropic`) + embeddings + agentic tool-use.
- `mcp/accretion`, `mcp/secrets` — the MCP server and its k8s-Secret resolution.
- `discord` — the conversational bot (mention/DM listener; agentic or RAG).
- `auth` — Google OAuth, API-key auth, sessions, `--disable-authentication`.
- `tempo` — query-side client for the Tempo trace backend (retrieval, not ingest).

**Cross-cutting**
- `config` — viper config, `ACCRETION_` env prefix, defaults.
- `logger` — zap logging, with an OTel log bridge.
- `telemetry` — accretion emitting its own OpenTelemetry traces/logs.
- `resilience` — retry (exponential backoff) + circuit breaker, used around every embed/insert.
- `testutil` — shared test fixtures (including embedding builders).

## Process & data-flow model

```
kube-apiserver ──▶ dispatcher ──(in-process, or Redis when scaled)──▶ log/metric ─┐
kube-apiserver ──────────────────────────────────▶  status/event collectors      ├─ embed ─▶ InsertEntry ─▶ unified_entries (pgvector)
OTLP traces    ──────────────────────────────────▶  trace collector              ┘                            │
                                                                                                             ▼ (embed query, cosine search)
agent (MCP)  ─▶ accretion-mcp ─(HTTP)─┐
Discord user ─▶ discord bot   ─(HTTP)─┴─▶ accretion-api ────────────────────────────────────────────────────▶ RAG / tool-use answer
```

Two tool surfaces read the same store — the `accretion_*` MCP tools and the Discord bot's
`search_cluster`/`get_*` tools — but both now reach it the same way: HTTP → `accretion-api` → DB (the
bot drives the same `mcp/accretion` client as `accretion-mcp`, e.g. `search_cluster` → `GET /api/search`).
Same memory, one call path.

## Conventions

- Config via viper; keys mirror the struct paths under the `ACCRETION_` env prefix.
- Every ingest side-effect (embed, insert) goes through `resilience` (retry + breaker).
- Interfaces + `mock.go` in `database`, `k8s`, and `llm` keep the pipeline unit-testable without a
  cluster, Redis, or an LLM. Lint config: [`.golangci.yml`](.golangci.yml).
