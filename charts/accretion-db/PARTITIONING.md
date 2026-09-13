# Weekly Partitioning

How `unified_entries` is split into weekly partitions, who creates and drops
them, and how to operate or change the scheme.

This is the companion to the [storage &amp; data-model doc](../../internal/database/README.md),
which owns the table itself — its [schema and indexes](../../internal/database/README.md#schema),
[dedup semantics](../../internal/database/README.md#dedup), and
[query/recall paths](../../internal/database/README.md#recalling-memory-vector-search). The
[one-time migration](migrations/001_partition_unified_entries.sql) that converts an existing flat
table to this scheme lives in this chart. This document covers only the partitioning mechanics, and
lives next to the chart that implements them.

## Why partition at all

`unified_entries` is an unbounded, ever-growing corpus — the collector appends
logs, metrics, events, pod/PVC/PV/job/HPA status, and traces continuously, and
nothing in the schema shrinks. Range-partitioning the table by `timestamp`, one
partition per ISO week, buys three things a single flat table cannot:

- **Cheap retention.** Aging data out is a metadata-only `DETACH` + `DROP` of a
  whole week, not a large row-by-row `DELETE` that bloats the table and fights
  autovacuum. Retention is **on by default** (see Configuration).
- **Partition pruning.** A time-bounded query (`WHERE timestamp >= …`) touches
  only the weeks in range; the planner skips the rest.
- **Bounded index maintenance.** The HNSW vector index is built and maintained
  **per partition**, so index size and rebuild cost track one week, not the
  entire history.

## The scheme

`unified_entries` is declared `PARTITION BY RANGE (timestamp)` in the schema SQL
([`templates/schema-configmap.yaml`](templates/schema-configmap.yaml)). Each
child partition covers exactly one ISO week (Monday-start), half-open
`[monday, monday+7d)`:

```
unified_entries                       ← partitioned parent (no rows of its own)
├── unified_entries_20260629          ← FROM ('2026-06-29') TO ('2026-07-06')
├── unified_entries_20260706          ← FROM ('2026-07-06') TO ('2026-07-13')
├── unified_entries_20260713          ← FROM ('2026-07-13') TO ('2026-07-20')
│   …one child per week…
└── unified_entries_default           ← everything outside an explicit week
```

Child partitions are named `unified_entries_YYYYMMDD`, where `YYYYMMDD` is the
week's **Monday**. The 8-digit date suffix is deliberate: it round-trips through a
plain `to_date(…, 'YYYYMMDD')`, which the retention job depends on (parsing an ISO
week *number* back to a date via `to_date(…, 'IYYYIW')` is fragile). The naming
and bounds are derived by a single function so the cadence is defined in exactly
one place.

### The partition-key constraint

PostgreSQL requires the partition key to appear in **every** `PRIMARY KEY` and
`UNIQUE` constraint on a partitioned table. That is why the surrogate primary key
is `(id, timestamp)` rather than just `id`, and why dedup uniqueness is the pair
`(content_hash, timestamp)` rather than `content_hash` alone. This is a mechanical
consequence of partitioning, not a modeling choice.

It has a real consequence for the workers: because `timestamp` is part of the
dedup key, a re-observed record only collapses onto its earlier row if it carries
the *same* timestamp — so every worker stamps a deterministic, content-derived
time rather than `time.Now()`. See [the storage doc → Dedup](../../internal/database/README.md#dedup).

### Indexes

Indexes are declared on the partitioned **parent** and propagate to every current
and future child automatically — a new weekly partition arrives fully indexed
with no extra step. (The index set itself is listed in
[the storage doc → Schema](../../internal/database/README.md#schema).)

### The DEFAULT partition

`unified_entries_default` is a catch-all for any row whose `timestamp` falls
outside every explicit weekly partition — most commonly backfilled history, or a
row whose event time predates the pre-created window. Its purpose is that
**inserts never fail** for want of a partition.

PostgreSQL allows only one default partition, and the schema detects an existing
one by its **partition bound rather than by name** (the migration may have created
it under a different name), so it never tries to create a second.

A row sitting in `DEFAULT` is not stranded: when the partition for its week is
later created, `ensure_unified_entries_partition` moves it across (below).

## How partitions are created

Two mechanisms create partitions; both are idempotent and both funnel through the
same function.

### `ensure_unified_entries_partition(p_date date)`

Defined in [`templates/schema-configmap.yaml`](templates/schema-configmap.yaml),
this is the hand-rolled stand-in for `pg_partman` (which is not in the DB image).
It computes `week_start := date_trunc('week', p_date)` (Postgres weeks start
Monday), derives the child name `unified_entries_<YYYYMMDD>`, and returns
immediately if that child already exists. Otherwise it creates it:

```sql
CREATE TABLE unified_entries_YYYYMMDD
  PARTITION OF unified_entries
  FOR VALUES FROM (week_start) TO (week_start + interval '1 week');
```

**If the `DEFAULT` partition already holds rows for that week, the plain `CREATE`
above is refused** — creating a partition tightens the default's implied
constraint, and PostgreSQL will not silently relocate rows:

```
ERROR:  updated partition constraint for default partition
        "unified_entries_default" would be violated by some row
```

That happens whenever rows arrive before their week exists — a stalled CronJob,
or an event-time timestamp older than the pre-created window. So in that case the
function repairs itself instead of failing: it detaches the default, creates the
partition, moves the matching rows across, and reattaches. The whole body is one
transaction, so a failure rolls back with the default still attached, and the
parent lock blocks — never drops — concurrent writes.

Calling it repeatedly for the same week is a no-op. **This function is the single
source of the weekly cadence** — everything else just decides *which* dates to
call it with.

### On install / upgrade — the schema hook

The schema is applied by a Helm `post-install,post-upgrade` hook Job
([`templates/schema-job.yaml`](templates/schema-job.yaml)) that runs the SQL in
the ConfigMap. When the table is partitioned, a `DO` block in that SQL ensures the
`DEFAULT` partition exists and pre-creates a small buffer around "now"
(last week through four weeks ahead), so a freshly installed or upgraded release
always has somewhere to write.

### Day to day — the maintenance CronJob

[`templates/partition-maintenance-cronjob.yaml`](templates/partition-maintenance-cronjob.yaml)
runs on a schedule (default `17 1 * * *`, daily at 01:17) with
`concurrencyPolicy: Forbid`. Each run:

1. Skips cleanly if `unified_entries` has not yet been migrated to a partitioned
   table (nothing to do).
2. Pre-creates partitions for `this week … +aheadWeeks` by calling
   `ensure_unified_entries_partition` over a `generate_series`.
3. Applies retention (below) — enabled by default.

Creating partitions ahead of time is what keeps writes landing in a bounded
weekly partition instead of falling through to `DEFAULT`.

## How partitions are dropped (retention)

Retention runs inside the same CronJob when `partitionMaintenance.retentionWeeks
> 0` (the default is `4`). For each weekly child whose name matches
`^unified_entries_[0-9]{8}$` and whose Monday is older than the window, it
detaches and then drops the whole partition:

```sql
ALTER TABLE unified_entries DETACH PARTITION "unified_entries_YYYYMMDD";
DROP TABLE IF EXISTS "unified_entries_YYYYMMDD";
```

Two things to know:

- **Whole-partition drop, never row `DELETE`.** Retention is deliberately a
  partition-level operation. This keeps it O(1) in row count and vacuum-free, and
  it is the property any append-only/immutability work must preserve — retention
  removes *weeks*, it does not mutate rows.
- **Plain `DETACH`, not `DETACH CONCURRENTLY`.** PostgreSQL forbids
  `DETACH CONCURRENTLY` while a `DEFAULT` partition exists, which is always the
  case here. Plain `DETACH` takes a brief `ACCESS EXCLUSIVE` lock on the parent,
  so the job is scheduled for a quiet window. The `DEFAULT` partition and every
  partition inside the retention window are left untouched.

The name regex deliberately matches only `unified_entries_YYYYMMDD`, so no default
partition can ever be caught by retention. Note that rows in `DEFAULT` are
therefore never aged out by this job.

## Archiving before drop (issue #143)

By default retention **destroys** each aged-out week. Set
`partitionMaintenance.archive.enabled: true` to instead write a full per-week
backup to a filesystem path *before* the partition is dropped, so aged-out data
leaves a trail of files an operator manages out of band. When
`archive.enabled: false` (the default), behavior is exactly the `DETACH → DROP`
above — archiving is strictly additive and off by default.

### The invariant: detach → export → verify → drop

For each expired week the job does, one partition at a time:

1. `ALTER TABLE unified_entries DETACH PARTITION "unified_entries_YYYYMMDD"` —
   the week becomes a standalone table.
2. `pg_dump -Fc -t "$part" "$PGDATABASE"` to a **temp file**, then `mv` it onto
   the deterministic final name `$ARCHIVE_PATH/unified_entries_YYYYMMDD.dump`.
   Writing temp-then-rename means a partial dump (disk full, killed mid-write)
   never appears as a complete `.dump`.
3. **Verify**: the `pg_dump` exit code is checked, the file must be non-empty,
   and `pg_restore -l` must be able to list its table of contents (this catches a
   truncated or corrupt archive).
4. **Only then** `DROP TABLE`.

If any step fails, the partition is **left detached and is NOT dropped**, an
error line is logged, and the whole run exits non-zero (visible as a failed Job).
The data survives on disk as an attached-then-detached table and is retried on the
next run. This is the core guarantee: **a week is never dropped unless a verified
archive of it exists** (or archiving is disabled).

The `image` is `postgres:16-alpine`, which ships `pg_dump`/`pg_restore`.

### Resumability

A run that dies between DETACH and DROP leaves an orphaned standalone
`unified_entries_YYYYMMDD` table (matches the weekly name pattern but is attached
to no parent). On startup the job's **recovery pass** finds these leftovers and
finishes them (export + verify + drop) before handling the normal expirations.
Because the archive filename is deterministic, a leftover whose verified `.dump`
is already present is simply dropped — re-processing is idempotent.

The recovery pass runs only when **both** `retentionWeeks > 0` **and**
`archive.enabled: true`. With archiving disabled the job keeps exactly its pre-#143
behavior — normal expirations only — and never touches a standalone
`unified_entries_YYYYMMDD` table: only the archive-then-drop path produces such
leftovers, so with archiving off a parentless weekly table is assumed to be
something else (e.g. an operator-restored week) and is left in place rather than
dropped without a backup. See also the restore warning below.

### Configuration

| Value | Default | Meaning |
|---|---|---|
| `archive.enabled` | `false` | Turn on archive-then-drop. Off = today's destroy-on-drop. |
| `archive.path` | `/archive` | Mount point inside the container; exported as `ARCHIVE_PATH`. |
| `archive.format` | `pgdump` | `pgdump` (custom-format `pg_dump -Fc`). `csv` is stubbed and rejected. |
| `archive.existingClaim` | `""` | Mount this PersistentVolumeClaim at `archive.path` (durable; recommended). |
| `archive.hostPath` | `""` | Mount this host directory instead (kind/dev only). |

When `archive.enabled: true`, **exactly one** of `existingClaim` / `hostPath`
must be set (`existingClaim` wins). Rendering **fails** otherwise, on purpose, so
the archive can never silently land on an ephemeral `emptyDir` that would discard
every week it "backs up". Nothing is mounted when archiving is disabled.

### Restoring an archived week

Each `.dump` is a standard custom-format archive.

> **⚠️ A restored `unified_entries_YYYYMMDD` table under its canonical name will be
> dropped by the next maintenance run.** With archiving enabled, each run's
> recovery pass reaps any standalone `unified_entries_YYYYMMDD` table that is
> attached to no parent — treating it as a half-finished expiry to complete. A
> plain `pg_restore` of a `.dump` recreates *exactly* such a parentless table.
> Because its `.dump` is already present and valid, the recovery pass will simply
> **drop it again** — silently undoing your restore, typically within a day.
>
> Two safe ways to restore, in order of preference:
>
> 1. **Restore under a different name (recommended for inspection).** Rewrite the
>    table name so it does not match `^unified_entries_[0-9]{8}$`, and it is
>    invisible to retention:
>
>    ```sh
>    # Retarget the archive to a *_restored name (does not match the weekly regex):
>    pg_restore --no-owner -t unified_entries_20260629 -f - unified_entries_20260629.dump \
>      | sed 's/unified_entries_20260629/unified_entries_20260629_restored/g' \
>      | psql -d "$PGDATABASE"
>    ```
>
>    …or restore into a throwaway scratch database entirely. Query, verify, then
>    drop it when done.
>
> 2. **Restore and immediately re-attach as a live partition (to bring the week
>    back into service).** Do the restore and the `ATTACH` as one uninterrupted
>    step so no maintenance run can fire in between — a re-attached partition has a
>    parent and is no longer a recovery-pass target.

To bring a week back into service (option 2):

```sh
# Inspect what's in it:
pg_restore -l unified_entries_20260629.dump

# Restore the standalone table (it was dropped, so no name clash):
pg_restore -d "$PGDATABASE" unified_entries_20260629.dump
```

That recreates `unified_entries_20260629` as a parentless standalone table. **Do
not leave it in this state** — re-attach it as a partition immediately (safe
precisely because the week was dropped, so there is no bound overlap with any live
partition):

```sql
ALTER TABLE unified_entries
  ATTACH PARTITION unified_entries_20260629
  FOR VALUES FROM ('2026-06-29') TO ('2026-07-06');
```

Once re-attached it has a parent and the recovery pass ignores it. Until then it
is a standalone `unified_entries_YYYYMMDD` table and the next run will drop it.

The `DEFAULT` partition is never archived or dropped (it doesn't match the weekly
name regex), so this path only ever concerns explicit weeks.

### Observability

The per-run summary line gains three fields (still one greppable JSON object):

```json
{"event":"partition_retention","retention_weeks":4,"partitions_dropped":2,
 "rows_dropped":1234,"archived":true,"archive_path":"/archive","archive_bytes":98765}
```

`archived` is the bool for whether the archive path was active this run;
`archive_bytes` is the aggregate size of the `.dump` files written;
`archive_path` is empty when archiving is disabled.

## Configuration

All knobs live in [`values.yaml`](values.yaml) under `partitionMaintenance`:

| Value | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Whether the maintenance CronJob is deployed. |
| `schedule` | `"17 1 * * *"` | Cron schedule; a partition is only physically created once per week, so daily is cheap. |
| `aheadWeeks` | `4` | How many weeks ahead to pre-create. **Must be `>= 1`** — with `0`, next week's writes reach `DEFAULT` before its partition exists, which `ensure_unified_entries_partition` then has to detach/move to repair. |
| `retentionWeeks` | `4` | Drop partitions older than this many weeks. **Enabled by default** so storage stays bounded. `0` disables retention (partitions accumulate). |

The CronJob validates both values as integers before they reach SQL, additionally
requires `aheadWeeks >= 1`, and refuses to run on a malformed value.

To keep a copy of each aged-out week instead of destroying it, see
[Archiving before drop](#archiving-before-drop-issue-143) and the
`partitionMaintenance.archive.*` values.

> **Note — enabling retention deletes data.** With `retentionWeeks: 4` on by
> default, the next maintenance run drops every weekly partition older than four
> weeks. On a database seeded with historical data (e.g. imported test data), that
> first run can remove almost everything. That is intended for a rolling live
> stream; if you are carrying data you want to keep, raise `retentionWeeks` or set
> it to `0` before the CronJob next fires.

## Existing (non-partitioned) databases

The `CREATE TABLE IF NOT EXISTS` in the schema only produces a partitioned table
on a **fresh** database; it does not alter an existing flat `unified_entries`, and
the maintenance CronJob skips such a database entirely. Converting one is a
one-time manual migration — see
[`migrations/001_partition_unified_entries.sql`](migrations/001_partition_unified_entries.sql). (A database already on an
older *monthly* scheme is a special case: its `unified_entries_YYYYMM` partitions
don't match the weekly retention regex, so they would never be dropped — rebuild
it, or re-partition monthly→weekly, before relying on retention.)

## Changing the granularity

The weekly cadence is localized, so switching to, say, daily or monthly
partitions is a bounded change in three spots — all SQL/chart, no Go, since the
application never references a partition:

1. **`ensure_unified_entries_partition`** in `templates/schema-configmap.yaml` —
   the `date_trunc(...)` unit, the child-name format (`YYYYMMDD` → e.g. `YYYYMM`
   for month), and the `FROM … TO …` interval.
2. **Pre-create ranges** — the `generate_series` bounds in both the schema `DO`
   block and the CronJob (interpret `aheadWeeks` in the new unit, or rename it).
3. **Retention matching** in the CronJob — the `^unified_entries_[0-9]{8}$`
   regex, the `right(relname, 8)` slice, and the `to_date(..., 'YYYYMMDD')` /
   interval comparison must all move to the new unit.

Coarser granularity (monthly) means fewer, larger partitions — less planning
overhead but a coarser retention floor. Finer granularity (daily) means more
partitions: more `CREATE`s, more per-partition HNSW indexes, and a longer planning
phase on queries that span many partitions.
