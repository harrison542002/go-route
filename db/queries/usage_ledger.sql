-- Idempotent on purpose. Records reach this table through the on-disk
-- spool, which delivers at least once: a crash between the commit and
-- the spool deleting its segment replays rows that are already here.
-- COPY would fail the whole batch on the first duplicate primary key,
-- so the batch arrives as one JSON array instead, typed by the table's
-- own row type. The row count lets the caller see how many were new.
--
-- name: InsertUsageLedgerRows :execrows
INSERT INTO usage_ledger (
    id, started_at, tenant_id, key_id, request_id,
    requested_model, chosen_target, status,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
    cost_nanos, token_source, pricing_version, billable,
    ttft_ms, total_ms, metadata, counterfactuals
)
SELECT
    r.id, r.started_at, r.tenant_id, r.key_id, r.request_id,
    r.requested_model, r.chosen_target, r.status,
    r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, r.reasoning_tokens,
    r.cost_nanos, r.token_source, r.pricing_version, r.billable,
    r.ttft_ms, r.total_ms, r.metadata, r.counterfactuals
FROM jsonb_populate_recordset(NULL::usage_ledger, sqlc.arg(rows)::jsonb) AS r
ON CONFLICT DO NOTHING;

-- Deliberately unbounded in time. Bounding by the id's own UUIDv7
-- timestamp would prune partitions, but started_at is the ingress time
-- and nothing guarantees the two agree -- a replayed or backfilled
-- record would silently read as missing. This probes each partition's
-- primary key instead, which retention keeps to a small number.
--
-- name: GetUsageLedgerEntry :one
SELECT * FROM usage_ledger WHERE id = $1;

-- group_by is a bound parameter resolved by a fixed CASE, never SQL
-- pasted into the query text. The metadata key is likewise bound: it is
-- the one grouping whose value comes from the user.
--
-- name: AggregateUsage :many
WITH grouped AS (
    SELECT
        CASE sqlc.arg(group_by)::text
            WHEN 'model'    THEN requested_model
            WHEN 'target'   THEN COALESCE(chosen_target, '(unserved)')
            WHEN 'status'   THEN status
            WHEN 'day'      THEN to_char(started_at, 'YYYY-MM-DD')
            WHEN 'metadata' THEN COALESCE(metadata ->> sqlc.arg(meta_key)::text, '(unset)')
            ELSE ''
        END                                                     AS key,
        count(*)::bigint                                        AS requests,
        COALESCE(SUM(cost_nanos), 0)::bigint                    AS cost_nanos,
        (count(*) FILTER (WHERE cost_nanos IS NULL))::bigint    AS unpriced,
        COALESCE(SUM(input_tokens), 0)::bigint                  AS input_tokens,
        COALESCE(SUM(output_tokens), 0)::bigint                 AS output_tokens,
        COALESCE(SUM(cache_read_tokens), 0)::bigint             AS cache_read_tokens,
        COALESCE(SUM(cache_write_tokens), 0)::bigint            AS cache_write_tokens,
        COALESCE(SUM(reasoning_tokens), 0)::bigint              AS reasoning_tokens,
        (count(*) FILTER (WHERE status = 'ok'))::bigint                 AS ok,
        (count(*) FILTER (WHERE status = 'exhausted'))::bigint          AS failed,
        (count(*) FILTER (WHERE status = 'truncated'))::bigint          AS truncated,
        (count(*) FILTER (WHERE status = 'client_disconnect'))::bigint  AS disconnected,
        -- Percentiles over requests that produced a first token only. An
        -- exhausted request has ttft_ms = 0 by construction, and
        -- including those would make latency look better the more
        -- failures a group has.
        COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY ttft_ms)
                 FILTER (WHERE ttft_ms > 0), 0)::double precision  AS p50_ttft_ms,
        COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
                 FILTER (WHERE ttft_ms > 0), 0)::double precision  AS p95_ttft_ms,
        COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY total_ms)
                 FILTER (WHERE total_ms > 0), 0)::double precision AS p95_total_ms
    FROM usage_ledger
    WHERE tenant_id = sqlc.arg(tenant_id)
      AND started_at >= sqlc.arg(since)
      AND started_at < sqlc.arg(until)
    GROUP BY 1
)
SELECT
    key, requests, cost_nanos, unpriced,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
    ok, failed, truncated, disconnected,
    p50_ttft_ms, p95_ttft_ms, p95_total_ms,
    -- Counted before the LIMIT, so a truncated report can say so.
    (count(*) OVER ())::bigint AS total_groups
FROM grouped
ORDER BY cost_nanos DESC, requests DESC
LIMIT sqlc.arg(row_limit);

-- Kept separate from AggregateUsage: folding this join into it would
-- multiply every other figure by the number of counterfactuals per row.
--
-- name: AggregateCounterfactuals :many
SELECT
    CASE sqlc.arg(group_by)::text
        WHEN 'model'    THEN l.requested_model
        WHEN 'target'   THEN COALESCE(l.chosen_target, '(unserved)')
        WHEN 'status'   THEN l.status
        WHEN 'day'      THEN to_char(l.started_at, 'YYYY-MM-DD')
        WHEN 'metadata' THEN COALESCE(l.metadata ->> sqlc.arg(meta_key)::text, '(unset)')
        ELSE ''
    END                                        AS key,
    (cf.entry ->> 'target')::text              AS target,
    SUM((cf.entry ->> 'cost')::bigint)::bigint AS cost_nanos,
    count(*)::bigint                           AS requests
FROM usage_ledger l
CROSS JOIN LATERAL jsonb_array_elements(
    -- A row with no comparisons holds jsonb null, not an array, and
    -- expanding a scalar is an error.
    CASE WHEN jsonb_typeof(l.counterfactuals) = 'array'
         THEN l.counterfactuals
         ELSE '[]'::jsonb END
) AS cf (entry)
WHERE l.tenant_id = sqlc.arg(tenant_id)
  AND l.started_at >= sqlc.arg(since)
  AND l.started_at < sqlc.arg(until)
  AND cf.entry ->> 'target' IS NOT NULL
GROUP BY 1, 2;
