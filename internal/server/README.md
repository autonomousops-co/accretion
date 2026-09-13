# `server` — HTTP API, RAG chat & auth

The `accretion-api server` subcommand. **Two independent listeners**, each with its own
`gorilla/mux` router (both carrying `otelmux` tracing and a per-request context logger):

| Listener | Default | Serves | Auth |
| --- | --- | --- | --- |
| `api` | on, `api.port` 41337, **TLS** | `/api/*` — REST, RAG chat | API key (`read:memory`, `chat:query`) |
| `ui` | **off**, `ui.port` 8080 | `/`, `/static/**`, `/auth/*` | Google OAuth + user whitelist |

`GET /api/health` is served on both and needs no auth. The routers are disjoint: nothing on the UI
listener answers `/api/*`, and the API listener serves no browser surface — so running with the UI
off leaves no OAuth endpoint reachable at all. At least one listener must be enabled or startup
fails. The API listener is the surface the [MCP tools](../mcp/README.md) proxy to.

**The API listener terminates TLS in-process** (`api.tls.*`, enabled by default; the UI listener is
always cleartext). `Start` loads and validates the keypair on its own goroutine *before* any
listener goroutine exists, so an unreadable or malformed certificate is a returned startup error —
there is no path that falls back to serving in the clear. `api.tls.min_version` is an enforced
floor, defaulting to TLS 1.2 and refusing 1.0/1.1 rather than honouring them.

Timeouts differ because the workloads do. Both use a 15s read, 60s idle and 10s read-header
deadline; the API's write deadline is `api.write_timeout` (120s by default, sized for the
non-streaming LLM calls behind `/api/chat`), while the UI's is 30s. Graceful shutdown on
SIGINT/SIGTERM drains every listener under one 30s budget. Config from [`../config`](../config);
auth from [`../auth`](../auth).

## Routes

**Health** (no auth) — `GET /api/health` → `{"status":"ok","service":"accretion-api"}`.

**Auth** (`/auth`, no auth) — `GET /auth/login`, `GET /auth/callback`, `GET|POST /auth/logout`,
`GET /auth/user`.

**API** (`/api`, wrapped in `RequireAuth`)

| Route | Handler | Backs MCP tool |
|---|---|---|
| `POST /api/chat` | `chatHandler` — the RAG flow (embed → time-window → vector search → LLM) | `accretion_ask` |
| `GET /api/logs` | `logsHandler` (`namespace`=`default`, `limit`=50) | `accretion_logs` |
| `GET /api/resources` | `resourcesHandler` (`types` CSV, `namespace`, `name`, `limit`, `start_time`/`end_time`, dynamic `df.<key>` exact + `dc.<key>` contains filters) | the resource tools |
| `GET /api/search` | `searchHandler` (`q` *(required)*, `limit`=10, `start_time`/`end_time` RFC3339) — embeds `q` and returns the matched `[]UnifiedEntry` above the similarity threshold | the Discord bot's `search_cluster` |
| `GET /api/stats` | `statsHandler` | `accretion_stats` |
| `GET /api/traces` | `tracesHandler` (Tempo; 503 if Tempo disabled) | `accretion_traces` |
| `GET /api/traces/{traceID}` | `traceHandler` | `accretion_trace` |

**Web UI** — off unless `ui.enabled` is true. When enabled, `GET /static/*` (no auth, no-cache)
serves `web/static/`; `GET /` (auth required)
serves `index.html` with the frontend log level injected.

The RAG `chatHandler` is documented in [`../llm`](../llm/README.md); the underlying search in
[`../database`](../database/README.md).

> **No streaming.** `ChatRequest` has a `Stream` field but it is ignored — `/api/chat` always
> returns a single buffered `{content, usage}` JSON response. There are no SSE endpoints.

## Authentication & authorization ([`../auth`](../auth))

`RequireAuth` accepts two credential types, tried in order:

1. **API key** — `Authorization: Bearer <token>` matched (by SHA-256 digest, constant-time) against
   the named, scoped `api_key_identities` entries in the mounted `config.yaml`. A match yields a
   principal carrying that identity's name and configured permissions, so every request is
   attributable to one client and each key is revocable on its own. There is no unscoped fallback:
   `NewAuthManager` fails closed when auth is enabled with zero identities. **This is the path the
   MCP servers use.**
2. **Session cookie** — set by the Google OAuth flow. `HandleLogin` issues a CSRF `state` and
   redirects to Google; `HandleCallback` verifies state, exchanges the code, fetches userinfo, and
   stores a `User` in a `gorilla/sessions` cookie (`accretion-api-session`, 7-day, HttpOnly,
   `SameSite=Lax`, `Secure` per config). The provider is pluggable (`google` or custom
   `AuthURL`/`TokenURL`).

**Authorization** is a whitelist: `AllowedEmails`/`AllowedDomains` gate access (empty = allow all
authenticated users); `AdminEmails`/`AdminDomains` additionally grant the `admin` role.
Authenticated users who aren't whitelisted get an empty permission set → 403. An **empty** whitelist
is allow-all, which is why `NewAuthManager` fails closed when auth is enabled and all four lists are
empty — otherwise a deployment with OAuth configured and no lists would admit anyone who completes
the flow. Permissions are
matched exactly, with no hierarchy, and the only ones any route requires are `read:memory` and
`chat:query` — the `admin` role therefore carries no extra permissions today; add one here only
alongside the route that requires it.

**`--disable-authentication`** (flag on the `server` command) sets `authEnabled=false`: `RequireAuth`
passes everything through and no OAuth/session store is built. It logs a loud security warning. No
deployment path uses it any more — the Tiltfile runs the dev stack with auth on — and it is for
manual local debugging only. Auth is on by default.
