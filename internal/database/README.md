# `database` — the memory store

The substrate that makes cluster history durable and semantically searchable. Everything Accretion
captures becomes a row in one PostgreSQL table with a vector embedding; recall is a nearest-neighbor
search over those vectors. (This is the canonical storage doc — it fills the role of the `STORAGE.md`
that code comments and `PARTITIONING.md` used to reference.)

- **Backend:** PostgreSQL 16 + [pgvector](https://github.com/pgvector/pgvector), driver
  `github.com/lib/pq`, vectors via `github.com/pgvector/pgvector-go`.
- **Schema DDL:** shipped by Helm in `charts/accretion-db/templates/schema-configmap.yaml`, applied
  by a `post-install/post-upgrade` Job. Partitioning details:
  [charts/accretion-db/PARTITIONING.md](../../charts/accretion-db/PARTITIONING.md).

## Schema

One table, `unified_entries`, discriminated by `resource_type` — not one table per kind.

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE unified_entries (
    id            BIGSERIAL,
    resource_type VARCHAR(50) NOT NULL,   -- one of the 15 kinds below
    namespace     VARCHAR(255),
    resource_name VARCHAR(255),
    data          JSONB NOT NULL,         -- kind-specific payload
    timestamp     TIMESTAMPTZ NOT NULL,   -- the partition key
    embedding     vector(768),            -- embedding of a human-readable summary
    created_at    TIMESTAMPTZ DEFAULT NOW(),
    content_hash  VARCHAR(64),            -- SHA-256 for dedup
    PRIMARY KEY (id, timestamp),
    UNIQUE (content_hash, timestamp)      -- dedup; timestamp required because it's the partition key
) PARTITION BY RANGE (timestamp);
```

**Indexes** (declared on the parent, inherited by every partition):

| Index | Type | Purpose |
|---|---|---|
| `idx_unified_embedding` | HNSW `vector_cosine_ops` | semantic (vector) search |
| `idx_unified_timestamp` | btree | time-window filters |
| `idx_unified_resource_type` | btree | filter by kind |
| `idx_unified_namespace` | btree | filter by namespace |
| `idx_unified_data` | GIN | JSONB key/containment lookups |

**`resource_type` values** (`database.go`): `log`, `metrics`, `event`, `pod_status`,
`node_status`, `deployment_status`, `hpa_status`, `endpoint_health`, `pvc_status`, `pv_status`,
`configmap`, `secret`, `job_status`, `cronjob_status`, `trace`.

The embedding column width tracks the embedding model: the default `nomic-embed-text:latest` emits
768-dimension vectors, hence `vector(768)` above. Swapping embedding models means changing both the
model and this column width to match.

## Go interface

`Interface` (`database.go`) with three implementations: `Client` (real Postgres),
`MockClient` (`mock.go`, in-memory), and `TracingDecorator` (`tracing.go`, OTel spans).

```go
type Interface interface {
    InsertEntry(entry *UnifiedEntry) error
    SearchEntries(queryEmbedding pgvector.Vector, limit int, opts SearchOptions) ([]UnifiedEntry, error)
    QueryResources(query *ResourceQuery) ([]UnifiedEntry, error)
    GetResourceStats() ([]ResourceStat, error)
    // + legacy helpers: GetRecentLogEntries, SearchSimilar[WithOptions], GetLogStats
    Close() error
}
```

- **`UnifiedEntry`** maps 1:1 to a row. Kind-specific fields are not columns — they live in `data`
  (JSONB) and are decoded on read into typed views (`LogEntry`, `MetricsEntry`, `TraceEntry`, the
  various `*StatusEntry`, …) by a `switch resource_type` dispatch.
- **`Client`** wraps a single `*sql.DB` (pool: 25 max / 5 idle / 5-min lifetime) and pings with
  exponential backoff (`waitForDB`) before serving. Built from `config.DatabaseConfig`.

## Writing memory

`InsertEntry` binds the `pgvector.Vector` directly and upserts:

```sql
INSERT INTO unified_entries (...) VALUES (..., $embedding, ...)
ON CONFLICT (content_hash, timestamp) DO NOTHING;
```

### Dedup

Dedup is two-layered: workers skip re-embedding anything whose SHA-256 `content_hash` is already in
an in-memory LRU, and the DB constraint is the backstop. (Metrics intentionally carry no hash —
duplicates are allowed.) See [../worker](../worker/README.md) for who calls this.

## Recalling memory (vector search)

`SearchEntries` is the core semantic query — cosine distance via `<=>`, thresholded and ordered:

```sql
SELECT ..., 1 - (embedding <=> $1) AS similarity
FROM unified_entries
WHERE embedding IS NOT NULL
  AND 1 - (embedding <=> $1) >= 0.3          -- MinSimilarityThreshold, a hard cutoff
  [AND timestamp >= $start] [AND timestamp <= $end]
ORDER BY embedding <=> $1                      -- nearest first
LIMIT $k;
```

The RAG flow (in [`../server`](../server/README.md) and [`../discord`](../discord)) embeds the
user's question, calls `SearchSimilarWithOptions(vec, 10, opts)`, renders the top results per-type
into text, and feeds that as context to the chat model. `QueryResources` is the non-vector path
(exact/contains filters by type, namespace, name, time) that backs the structured MCP resource
tools.

## Partitioning & retention (summary)

`unified_entries` is range-partitioned by `timestamp` into **weekly** children
(`unified_entries_YYYYMMDD`, Monday-start), plus a `unified_entries_default` catch-all so inserts
never fail on an out-of-window timestamp. A maintenance CronJob (in `charts/accretion-db`)
pre-creates partitions ahead of time and drops old ones by whole-partition `DETACH` + `DROP`
(O(1), no row-by-row delete), with an optional archive-before-drop (`detach → export → verify →
drop`). Full detail and the one-time flat→partitioned migration:
[charts/accretion-db/PARTITIONING.md](../../charts/accretion-db/PARTITIONING.md).

## Testing

`MockClient` is a complete in-memory `Interface` (position-based fake similarity, honors time
filters, replicates `ON CONFLICT`) so the ingest and serve paths can be tested without Postgres.
