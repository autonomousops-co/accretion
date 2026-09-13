# `mcp` — agent access via the Model Context Protocol

How an AI agent reaches the memory store. Two independent MCP servers, both over **stdio**, built on
[`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go):

- `mcp/accretion` — tools for querying cluster memory (backs the `accretion-mcp` binary).
- `mcp/secrets` — shared credential resolution (env or Kubernetes Secret).

The accretion MCP server is a **thin HTTP proxy**: it holds no database logic and calls the
[`accretion-api`](../server/README.md) REST endpoints. All retrieval/RAG happens server-side.

## `accretion-mcp` tools (`mcp/accretion`)

Server name `accretion` v1.0.0. Registered in `server.go`; the HTTP client is `client.go`.

**Core**

| Tool | Params | Returns | API call |
|---|---|---|---|
| `accretion_ask` | `question` *(required)* | RAG answer grounded in the store (embeds → vector search → LLM) | `POST /api/chat` |
| `accretion_logs` | `namespace` (=`default`), `limit` (=50) | recent log entries | `GET /api/logs` |
| `accretion_health` | — | API reachability/health | `GET /api/health` |

**Resource snapshots** (all also take `limit`, default 20 / max 100; back `GET /api/resources`)

| Tool | Params | Covers (`types=`) |
|---|---|---|
| `accretion_workloads` | `namespace`, `name`, `resource_type` (deployment/job/cronjob/hpa) | `deployment_status,job_status,cronjob_status,hpa_status` |
| `accretion_pods` | `namespace`, `name`, `include_metrics` | `pod_status` (+`metrics`) |
| `accretion_events` | `namespace`, `event_type` (Normal/Warning), `reason` | `event` |
| `accretion_nodes` | `name` | `node_status` |
| `accretion_networking` | `namespace`, `service` | `endpoint_health` |
| `accretion_storage` | `namespace`, `name`, `resource_type` (pvc/pv) | `pvc_status,pv_status` |
| `accretion_config` | `namespace`, `name`, `resource_type` (configmap/secret) | `configmap,secret` |
| `accretion_stats` | — | counts by resource type & namespace |

**Distributed traces** (query the Tempo backend)

| Tool | Params | API call |
|---|---|---|
| `accretion_traces` | `service`, `operation`, `minDuration`, `maxDuration`, `limit` (=20), `start`/`end` (RFC3339) | `GET /api/traces` |
| `accretion_trace` | `trace_id` *(required)* | `GET /api/traces/{id}` |

`accretion_ask` is the high-level entry point (let the server do RAG); the resource tools are for an
agent that wants to drive its own retrieval and reason over structured results.

## Launch & credentials

`cmd/accretion-mcp/main.go` resolves two keys — `ACCRETION_API_URL` (required) and
`ACCRETION_API_KEY` — then serves `NewServer(NewClientWithTLS(url, key, opts))` over stdio.

`mcp/secrets` picks the source:
- If `ACCRETION_MCP_SECRET_NAME=namespace/name` is set → `ResolveFromK8s`: reads the named Secret via
  in-cluster config (falling back to kubeconfig). **Matching env vars still override Secret values.**
- Otherwise → `ResolveFromEnv`: straight from the environment.

The client sends `Authorization: Bearer <ACCRETION_API_KEY>` on every request except health. The key
must be the plaintext of one of the server's named `auth.api_key_identities` entries, and that
identity's permissions bound what the tools can reach — `accretion_ask` needs `chat:query`, the
resource tools need `read:memory` (see [../auth](../server/README.md)). In-cluster the chart
publishes it as `accretion-mcp-secrets`, which is what `ACCRETION_MCP_SECRET_NAME` resolves.

Minimal client config (see repo-root [`.mcp.json`](../../.mcp.json)):

```jsonc
{ "mcpServers": { "accretion": {
  "command": "accretion-mcp",
  "env": {
    "ACCRETION_API_URL": "https://localhost:41337",
    "ACCRETION_API_CA_FILE": "/abs/path/to/accretion/.tls/ca.crt",
    "ACCRETION_API_KEY": "..."
  }
}}}
```

The API listener speaks TLS by default, so the URL is `https://` and the client has to trust the
certificate. `ACCRETION_API_CA_FILE` (a path) and `ACCRETION_API_CA_PEM` (the bundle inline) both
work. The `ACCRETION_MCP_SECRET_NAME` form above needs neither: that Secret carries a *pointer* to
the CA, which the MCP follows with the same kubeconfig it used to read the credential — nothing to
place on disk, nothing to re-copy on renewal.

Two pointers exist, and the difference is a privilege one. `ACCRETION_API_CA_CONFIGMAP` names a
ConfigMap of public CA material. `ACCRETION_API_CA_SECRET` names the serving Secret — which also
holds the API listener's **private key**, because Kubernetes RBAC cannot narrow a read to one key of
an object. The chart defaults to the Secret only because it is the one object the chart can be sure
exists; publish a CA-only ConfigMap (cert-manager's `trust-manager` does this) and set
`ACCRETION_API_CA_CONFIGMAP` in your `.env`, and MCP users need no access to the key at all.

The chart used to own this Secret and offered the same choice as `mcpSecret.caConfigMap`; #122 moved
the credential out of chart values into `.env`, so `scripts/make-secret.sh` builds the Secret and
reads that variable.

`ACCRETION_API_INSECURE_SKIP_VERIFY` exists but is refused for any non-loopback host.

## The Discord bot is not an MCP server

The conversational bot that answers cluster questions in Discord lives in
[`../discord`](../discord) and is not exposed over MCP. It is not a direct-DB reader either: its
in-process `search_cluster`/`get_*` tools drive the same [`mcp/accretion`](accretion) HTTP `Client`
as `accretion-mcp`, so both tool surfaces read through
[`accretion-api`](../server/README.md). Its `search_cluster` tool calls `GET /api/search`.
