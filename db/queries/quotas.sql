-- name: ListQuotas :many
SELECT * FROM quotas WHERE tenant_id = $1 ORDER BY window_kind;

-- name: GetQuota :one
SELECT * FROM quotas WHERE tenant_id = $1 AND window_kind = $2;

-- A PUT replaces the whole set for a tenant. Run this and the upserts in
-- one transaction: deleting the old set and then failing to write the
-- new one would leave a tenant unlimited.
--
-- name: DeleteQuotasForTenant :exec
DELETE FROM quotas WHERE tenant_id = $1;

-- name: DeleteQuota :execrows
DELETE FROM quotas WHERE tenant_id = $1 AND window_kind = $2;

-- name: UpsertQuota :one
INSERT INTO quotas (
    tenant_id, window_kind, period_start, period_end,
    max_cost_nanos, on_exceed, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (tenant_id, window_kind) DO UPDATE SET
    period_start   = EXCLUDED.period_start,
    period_end     = EXCLUDED.period_end,
    max_cost_nanos = EXCLUDED.max_cost_nanos,
    on_exceed      = EXCLUDED.on_exceed,
    updated_at     = now()
RETURNING *;

-- LEFT JOIN because a tenant with a quota and no traffic yet has no
-- counter row, and that must read as zero rather than as no quota.
--
-- name: GetQuotaStatus :many
SELECT
    q.tenant_id,
    q.window_kind,
    q.period_start,
    q.period_end,
    q.max_cost_nanos,
    q.on_exceed,
    COALESCE(c.requests, 0)::bigint   AS used_requests,
    COALESCE(c.tokens, 0)::bigint     AS used_tokens,
    COALESCE(c.cost_nanos, 0)::bigint AS used_cost_nanos
FROM quotas q
LEFT JOIN usage_counters c
       ON c.tenant_id = q.tenant_id
      AND c.window_kind = q.window_kind
      AND c.window_start = sqlc.arg(window_start)
WHERE q.tenant_id = sqlc.arg(tenant_id);
