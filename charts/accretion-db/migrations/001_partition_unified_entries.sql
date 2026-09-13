-- migrations/001_partition_unified_entries.sql
--
-- ONE-TIME, MANUAL migration that converts an EXISTING non-partitioned
-- `unified_entries` table into a weekly RANGE-partitioned table (issues #127, #128).
--
-- This is deliberately NOT wired into the helm schema hook: it rewrites the main
-- table and must be run with human oversight, once, per database.
--
--   psql -h <host> -U postgres -d accretion -v ON_ERROR_STOP=1 -f 001_partition_unified_entries.sql
--
-- Safety / operational notes:
--   * IDEMPOTENT GUARD: no-ops if `unified_entries` is already partitioned.
--   * DOWNTIME: the final RENAME swap takes a brief ACCESS EXCLUSIVE lock
--     (sub-second, metadata only). The bulk backfill runs online.
--   * SNAPSHOT GAP: rows written to the old table *after* the backfill SELECT and
--     *before* the RENAME are not copied. Run during a quiet window OR pause the
--     collector/trace-worker ingestion for the duration (recommended for prod).
--   * DISK: needs headroom for a full second copy of the table until the old one
--     is dropped (see the final manual step).
--   * The old table is renamed to `unified_entries_old`, NOT dropped — verify row
--     counts, then drop it manually.

DO $mig$
DECLARE
    min_ts date;
    max_ts date;
    m date;
    idx text;
    max_id bigint;
    old_count bigint;
    new_count bigint;
BEGIN
    -- Guard: already partitioned → nothing to do.
    IF EXISTS (SELECT 1 FROM pg_partitioned_table pt
               JOIN pg_class c ON c.oid = pt.partrelid
               WHERE c.relname = 'unified_entries') THEN
        RAISE NOTICE 'unified_entries is already partitioned; skipping migration.';
        RETURN;
    END IF;

    RAISE NOTICE 'Creating partitioned twin unified_entries_new...';
    CREATE TABLE unified_entries_new (
        id BIGINT NOT NULL,
        resource_type VARCHAR(50) NOT NULL,
        namespace VARCHAR(255),
        resource_name VARCHAR(255),
        data JSONB NOT NULL,
        timestamp TIMESTAMPTZ NOT NULL,
        embedding vector(768),
        created_at TIMESTAMPTZ DEFAULT NOW(),
        content_hash VARCHAR(64),
        PRIMARY KEY (id, timestamp),
        UNIQUE (content_hash, timestamp)
    ) PARTITION BY RANGE (timestamp);

    -- DEFAULT partition catches out-of-range rows so backfill never fails.
    -- Named unified_entries_default (NOT ..._new_default) so it matches the name
    -- the schema hook expects post-rename; otherwise the next `helm upgrade` would
    -- try to create a second DEFAULT partition and fail.
    CREATE TABLE unified_entries_default PARTITION OF unified_entries_new DEFAULT;

    -- One weekly partition per ISO week (Monday-start) spanning the existing data.
    SELECT date_trunc('week', MIN(timestamp))::date,
           date_trunc('week', MAX(timestamp))::date
      INTO min_ts, max_ts
      FROM unified_entries;
    IF min_ts IS NOT NULL THEN
        m := min_ts;
        WHILE m <= max_ts LOOP
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF unified_entries_new FOR VALUES FROM (%L) TO (%L)',
                'unified_entries_' || to_char(m, 'YYYYMMDD'),
                m::text, (m + interval '1 week')::date::text);
            m := (m + interval '1 week')::date;
        END LOOP;
    END IF;

    RAISE NOTICE 'Backfilling rows (preserving ids)...';
    INSERT INTO unified_entries_new
        (id, resource_type, namespace, resource_name, data, timestamp, embedding, created_at, content_hash)
    SELECT id, resource_type, namespace, resource_name, data, timestamp, embedding, created_at, content_hash
    FROM unified_entries;

    -- Sanity check row counts before the swap.
    SELECT count(*) INTO old_count FROM unified_entries;
    SELECT count(*) INTO new_count FROM unified_entries_new;
    IF old_count <> new_count THEN
        RAISE EXCEPTION 'Row count mismatch (old=%, new=%); aborting before cutover.', old_count, new_count;
    END IF;

    -- Point the new table's id at the existing sequence and advance it past the
    -- backfilled ids so new inserts don't collide. The sequence already exists
    -- (from the original SERIAL); CREATE ... IF NOT EXISTS is only a safety net.
    SELECT COALESCE(MAX(id), 0) INTO max_id FROM unified_entries_new;
    CREATE SEQUENCE IF NOT EXISTS unified_entries_id_seq;
    EXECUTE format('ALTER TABLE unified_entries_new ALTER COLUMN id SET DEFAULT nextval(%L)',
                   'unified_entries_id_seq');
    PERFORM setval('unified_entries_id_seq', max_id + 1, false);

    -- Build indexes on the twin BEFORE the cutover so the live table is never
    -- served without its indexes. (RENAME does not carry indexes across, so they
    -- must exist on unified_entries_new prior to the swap.)
    --
    -- Index names are UNIQUE PER SCHEMA and ALTER TABLE ... RENAME does NOT rename
    -- a table's indexes, so the existing table keeps owning idx_unified_*. Build
    -- under temporary _new names and swap them to canonical after the cutover.
    RAISE NOTICE 'Building indexes on the twin...';
    CREATE INDEX idx_unified_embedding_new     ON unified_entries_new USING hnsw (embedding vector_cosine_ops);
    CREATE INDEX idx_unified_timestamp_new     ON unified_entries_new (timestamp);
    CREATE INDEX idx_unified_resource_type_new ON unified_entries_new (resource_type);
    CREATE INDEX idx_unified_namespace_new     ON unified_entries_new (namespace);
    CREATE INDEX idx_unified_data_new          ON unified_entries_new USING GIN (data);

    RAISE NOTICE 'Cutover: renaming tables...';
    ALTER TABLE unified_entries      RENAME TO unified_entries_old;
    ALTER TABLE unified_entries_new  RENAME TO unified_entries;

    -- Move the canonical index names onto the live table: park the old table's
    -- indexes as *_old, then promote the twin's *_new. This matters beyond
    -- cosmetics — the schema hook recreates these by name with CREATE INDEX IF
    -- NOT EXISTS, so if the OLD table kept the canonical names the hook would
    -- silently skip and leave the live table unindexed (full scans on every
    -- vector search). The *_old indexes are dropped with unified_entries_old.
    RAISE NOTICE 'Swapping index names onto the live table...';
    FOREACH idx IN ARRAY ARRAY['idx_unified_embedding', 'idx_unified_timestamp',
                               'idx_unified_resource_type', 'idx_unified_namespace',
                               'idx_unified_data']
    LOOP
        IF to_regclass('public.' || idx) IS NOT NULL THEN
            EXECUTE format('ALTER INDEX %I RENAME TO %I', idx, idx || '_old');
        END IF;
        EXECUTE format('ALTER INDEX %I RENAME TO %I', idx || '_new', idx);
    END LOOP;

    -- Re-own the sequence to the surviving (renamed) table. Without this it stays
    -- OWNED BY unified_entries_old.id, and the final `DROP TABLE unified_entries_old`
    -- would cascade-drop the sequence and break id generation on the live table.
    ALTER SEQUENCE unified_entries_id_seq OWNED BY unified_entries.id;

    RAISE NOTICE 'Done. % rows migrated. Verify, then: DROP TABLE unified_entries_old;', new_count;
END;
$mig$ LANGUAGE plpgsql;

-- Recreate the maintenance helper + a forward buffer of partitions on the new
-- table (safe to run; the schema hook also does this).
-- Kept in sync with the copy in charts/accretion-db/templates/schema-configmap.yaml.
-- Weekly partitions named unified_entries_YYYYMMDD (the ISO week's Monday). If the
-- DEFAULT partition already holds rows for the target week, PostgreSQL refuses the
-- CREATE, so detach the default, create, move the rows, reattach.
CREATE OR REPLACE FUNCTION ensure_unified_entries_partition(p_date date)
RETURNS void AS $$
DECLARE
    week_start   date := date_trunc('week', p_date)::date;
    week_end     date := (week_start + interval '1 week')::date;
    part_name    text := format('unified_entries_%s', to_char(week_start, 'YYYYMMDD'));
    default_name text;
    has_rows     boolean := false;
BEGIN
    IF to_regclass('public.' || part_name) IS NOT NULL THEN
        RETURN;
    END IF;

    SELECT c.relname INTO default_name
    FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    WHERE i.inhparent = 'unified_entries'::regclass
      AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT';

    IF default_name IS NOT NULL THEN
        EXECUTE format(
            'SELECT EXISTS (SELECT 1 FROM %I WHERE timestamp >= %L AND timestamp < %L)',
            default_name, week_start, week_end) INTO has_rows;
    END IF;

    IF NOT has_rows THEN
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF unified_entries FOR VALUES FROM (%L) TO (%L)',
            part_name, week_start::text, week_end::text);
        RETURN;
    END IF;

    EXECUTE format('ALTER TABLE unified_entries DETACH PARTITION %I', default_name);
    EXECUTE format(
        'CREATE TABLE %I PARTITION OF unified_entries FOR VALUES FROM (%L) TO (%L)',
        part_name, week_start::text, week_end::text);
    EXECUTE format(
        'INSERT INTO unified_entries SELECT * FROM %I WHERE timestamp >= %L AND timestamp < %L',
        default_name, week_start, week_end);
    EXECUTE format(
        'DELETE FROM %I WHERE timestamp >= %L AND timestamp < %L',
        default_name, week_start, week_end);
    EXECUTE format('ALTER TABLE unified_entries ATTACH PARTITION %I DEFAULT', default_name);
END;
$$ LANGUAGE plpgsql;

SELECT ensure_unified_entries_partition((CURRENT_DATE + (n || ' week')::interval)::date)
FROM generate_series(-1, 4) AS g(n);

-- Indexes are built on the twin before the cutover (inside the migration block
-- above), so there is nothing to rebuild here.
