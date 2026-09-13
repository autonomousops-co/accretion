# `worker` — the ingestion pipeline (writing memory)

Workers are the write path. Each turns a class of Kubernetes signal into `unified_entries` rows:
capture → summarize → **embed** → dedup → insert. This doc covers the workers plus the two pieces
that feed them — the [`collector`](../collector) and [`queue`](../queue).

## Two ingest mechanisms, one sink

1. **Queue-driven, pod-scoped** (`logs`, `metrics`): the [collector](../collector) watches Pods via
   a client-go informer and pushes per-pod work items through Redis; log/metric workers consume them.
2. **Watchers, pollers and receivers** (everything else): these don't use Redis. The eight status
   workers (`*-status`, configmap) are driven by client-go shared informers since #96 — one LIST and
   one long-lived WATCH per resource, per watched namespace on a static scope and cluster-wide
   otherwise, with the scope applied per object on both paths, every object re-delivered each resync
   (`k8s.poll_interval`); the event worker still ticks on that interval and lists namespaces
   directly; the trace worker runs an OTLP HTTP receiver.

Both terminate identically: `llm.GenerateEmbedding(summary)` → build `database.UnifiedEntry` →
`db.InsertEntry`. See [../database](../database/README.md) and [../llm](../llm/README.md).

## Worker inventory

Workers are Go types, not processes. Since #91/#92 they run as supervised goroutines inside two
subcommands of `accretion-api` (in `../cmd/`): `data-collector` runs the log, metric, event and
trace collectors, and `status` runs the eight object-state collectors. Any data collector can be
given its own Deployment with `data-collector --only <name>`, which the chart drives from
`dataCollector.kinds.<name>.split`. The worker types themselves are unchanged, so the records they
write are the same.

| Worker (`Type()`) | Mechanism | Kubernetes signal | `resource_type` produced |
|---|---|---|---|
| `logWorker` (`logs`) | queue | streams container logs (`StreamPodLogs`) | `log` |
| `metricWorker` (`metrics`) | queue + poll | `GetPodMetrics` per assigned pod | `metrics` |
| `eventWorker` (`events`) | poll | `ListEvents(ns, since)`, incremental | `event` |
| `podStatusWorker` (`pod-status`) | informer | `WatchPodStatuses` — phase, conditions, restarts, exit codes | `pod_status` |
| `nodeStatusWorker` (`node-status`) | informer | `WatchNodeStatuses` — **cluster-scoped**; conditions, capacity | `node_status` |
| `deploymentStatusWorker` (`deployment-status`) | informer | `WatchDeploymentStatuses` — Deployments joined with owned ReplicaSets | `deployment_status` |
| `hpaStatusWorker` (`hpa-status`) | informer | `WatchHPAStatuses` — current/desired/min/max, metrics | `hpa_status` |
| `endpointHealthWorker` (`endpoint-health`) | informer | `WatchEndpointHealth` — EndpointSlices aggregated per Service | `endpoint_health` |
| `pvcStatusWorker` (`pvc-status`) | informer | `WatchVolumeStatuses` — PVCs (scoped) + PVs (cluster) | `pvc_status`, `pv_status` |
| `configMapWorker` (`configmap`) | informer | `WatchConfigMetadata` — ConfigMaps only (metadata only: name, namespace, uid, labels, resourceVersion, creation time, allowlisted annotations; see below). Secrets are **not** collected | `configmap` |
| `jobStatusWorker` (`job-status`) | informer | `WatchJobStatuses` — Jobs + CronJobs | `job_status`, `cronjob_status` |
| `traceWorker` (`traces`) | OTLP receiver | `POST /v1/traces` (protobuf) | `trace` |

## Anatomy

- **`Worker` interface** (`interface.go`): `Start/Stop/Type/ID/IsHealthy/GetAssignedPods/…`.
- **`BaseWorker`** (`base.go`): a `queue.Consumer`, an `nsscope.Scope`, two `resilience.CircuitBreaker`s
  (`llmBreaker`, `dbBreaker`), and `WorkerStats`. Used by the queue-driven workers (log/metric/trace);
  the status and event workers embed the same fields inline.
- **Run loops** — two shapes:
  - *Queue consumer*: `BRPOP`s the worker's dedicated Redis list (`queue.Consume*WorkForWorker`) and
    runs `handleWork` per item.
  - *Ticker poll* (event worker): an immediate poll, then `for { select { <-ctx.Done() / <-ticker.C } }`
    at the configured interval (`k8s.poll_interval`, default 30s).
  - *Informer* (the status workers, #96): `k8s.Watch<Kind>(ctx, resync, namespaces, scope.Contains,
    emit)` blocks for the life of the unit. `namespaces` is the scope's explicit list when it is
    static (`scope.IsStatic()`), and the k8s package (`internal/k8s/statusinformers.go`) then runs
    one shared informer per resource per namespace, caching only those namespaces; for a label or
    `"*"` scope it is empty and one cluster-wide informer per resource is filtered per object.
    Each object is converted by the one conversion function its kind has (`podStatusEntry`,
    `deploymentStatusEntry`, …), which is what the deleted poll path also called, so the entries,
    hashes and timestamps are byte-identical to what polling wrote; `emit` fires for the initial
    list, every change, and every resync. Failure is loud: a Forbidden or Unauthorized on any LIST
    or WATCH is fatal on sight, a watch that keeps failing with no event delivered is fatal after a
    few attempts, and a LIST that does not complete hits a fixed two-minute timeout — in every case
    the unit returns an error, the supervisor restarts it, and `/ready` reflects it.

    **Kubernetes Secrets are not collected at all**, and no chart role grants any verb on them.
    Kubernetes has no metadata-only read for a Secret — `list`/`watch` return `.data` — so recording
    their metadata would require cluster-wide read of every secret value in the cluster. That trade
    is declined; cluster secrets are out of scope for monitoring. Accretion's own components still
    authenticate normally, from Secrets the kubelet resolves via `secretKeyRef`, which needs no RBAC.

    What the ConfigMap informer retains — in the store, the entry and the row — is name, namespace,
    uid, resourceVersion, creation time, labels, and only the annotations `k8s.SafeAnnotations`
    admits; never `.data` or `.binaryData`, and never a body-carrying annotation such as kubectl's
    `last-applied-configuration`, which holds the applied ConfigMap in plaintext. Every other
    informer strips `managedFields`.
- **Dedup** (`lru.go`): a SHA-256 `contentHash` over the semantic fields, checked against an
  in-memory LRU (10k entries; 50k for traces) before embedding, plus the DB's `ON CONFLICT`. Metrics
  skip hashing (duplicates wanted). Logs can be pre-filtered (`logfilter.go`) to drop probe/health
  noise and sub-threshold levels before embedding.
- **Resilience** ([../resilience](../resilience)): every embed and insert is
  `breaker.Execute(ctx, WithRetry(...))`. A failing LLM endpoint trips `llmBreaker` without taking
  down inserts, and vice-versa.
- **Instrumentation:** each process sets a `ServiceName` (`accretion-data-collector`,
  `accretion-status-collector`, or `accretion-<name>-collector` for a split-out collector) and the
  workers emit their own OTel spans ([../telemetry](../telemetry)).
- **Supervision** ([../collector/supervisor.go](../collector/supervisor.go)): sharing a process
  trades isolation for density, so every collector runs behind a `recover()` and is restarted on its
  own with capped backoff. A collector that keeps failing takes `/ready` to 503 rather than being
  left dead behind a pod that still reports Ready.

## The collector & queue (log/metric dispatch)

The [`collector`](../collector) `Manager`/`Controller` is a stateless controller for **log and metric
work only** (status/event/trace workers self-schedule and aren't managed here):

- A **Pod informer** feeds a rate-limited workqueue on running-pod transitions.
- A `WorkDistributor` reads worker health from Redis, picks the least-loaded healthy worker (ties by
  FNV-1a hash of the pod key), and `LPUSH`es a work descriptor to that worker's queue (or a shared
  fallback). Descriptors are lightweight tickets (`LogCollectionWork`/`MetricsCollectionWork` — pod
  identity + containers), **not** telemetry or embeddings.
- Safety-net tickers reassign pods from dead workers (>60s stale), drain the fallback queue, and
  re-sync unassigned pods.

[`queue`](../queue) (Redis/go-redis) is therefore a **work-dispatch bus + coordination store**
(worker health hashes with TTL, pod assignments, fallback lists) — never an embedding queue.

## Namespace scoping & the self-ingestion guard

[`nsscope`](../nsscope) resolves which namespaces are in scope — a static name list (zero API calls)
or label/`*` selectors refreshed on a background ticker. To avoid a feedback loop, accretion's own
namespace is subtracted from the **log/trace** scope (`nsscope.Derived(...)`): the collector enforces
it when dispatching log work, and the trace worker drops spans whose `k8s.namespace.name` is out of
scope — so accretion never records and re-embeds its own logs/traces.

## Example: the log path end-to-end

```
Pod informer add/update ─▶ workqueue "pod-running/<ns>/<name>"
   ─▶ WorkDistributor ─▶ queue.Client.PublishLogWorkToWorker(<id>)
        default:  in-process channel  (queue.InProcessClient, no Redis)
        scaled:   Redis LPUSH accretion:log_work:worker:<id>
   ─▶ logWorker consume ─▶ collectLogs ─▶ StreamPodLogs
   ─▶ processLog: logfilter ─▶ SHA-256 dedup ─▶ GenerateEmbedding
   ─▶ UnifiedEntry{ResourceType:"log", ...} ─▶ InsertEntry (upsert)
```

The dispatcher and the log/metric collectors are in the SAME process by default (#93), so the
publish is a channel send rather than a network round trip. It is also bounded: a full worker queue
sheds to the fallback queue after a short timeout instead of blocking, because the dispatcher
publishes log work and then metric work on one goroutine and must not stall on either.

The event poller is the same tail without a queue: `ticker ─▶ ListEvents(ns) ─▶ dedup ─▶ embed ─▶ insert`.
The status workers are that tail fed by an informer: `list+watch ─▶ convert ─▶ dedup ─▶ embed ─▶ insert`.
