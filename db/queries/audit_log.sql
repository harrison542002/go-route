-- name: InsertAuditLog :copyfrom
INSERT INTO audit_log (
    id, ts, tenant_id, key_id, actor, action, request_id,
    reason_kind, reason_detail, policy_version,
    ladder, ladder_attempts, final_target, status_code, error
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10,
    $11, $12, $13, $14, $15
);

-- Admin mutations run inside the transaction they describe, so a quota
-- change that rolls back leaves no row claiming it happened.
--
-- name: CreateAuditEntry :one
INSERT INTO audit_log (
    id, ts, tenant_id, key_id, actor, action, request_id,
    ladder, ladder_attempts, status_code, error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetAuditEntry :one
SELECT * FROM audit_log WHERE id = $1;

-- name: ListAuditEntriesByRequestID :many
SELECT * FROM audit_log
WHERE request_id = $1
ORDER BY ts DESC
LIMIT sqlc.arg(row_limit);

-- name: ListAuditEntriesForTenant :many
SELECT * FROM audit_log
WHERE tenant_id = sqlc.arg(tenant_id)
  AND ts >= sqlc.arg(since)
  AND ts < sqlc.arg(until)
ORDER BY ts DESC
LIMIT sqlc.arg(row_limit);
