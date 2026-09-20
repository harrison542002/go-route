-- Deltas, not absolutes, so two instances flushing the same second
-- compose instead of clobbering each other.
--
-- name: AddUsageCounterDelta :exec
INSERT INTO usage_counters (tenant_id, window_kind, window_start, requests, tokens, cost_nanos)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, window_kind, window_start) DO UPDATE SET
    requests   = usage_counters.requests + EXCLUDED.requests,
    tokens     = usage_counters.tokens + EXCLUDED.tokens,
    cost_nanos = usage_counters.cost_nanos + EXCLUDED.cost_nanos,
    updated_at = now();

-- name: GetUsageCounter :one
SELECT * FROM usage_counters
WHERE tenant_id = $1 AND window_kind = $2 AND window_start = $3;

-- name: ListUsageCountersForTenant :many
SELECT * FROM usage_counters
WHERE tenant_id = $1 AND window_start >= sqlc.arg(window_start)
ORDER BY window_kind, window_start DESC;

-- Warms Redis in one pass after a cold start, rather than per tenant.
--
-- name: ListUsageCountersInWindow :many
SELECT * FROM usage_counters
WHERE window_kind = $1 AND window_start = $2
ORDER BY tenant_id;

-- name: DeleteExpiredUsageCounters :exec
DELETE FROM usage_counters
WHERE window_kind = $1 AND window_start < $2;
