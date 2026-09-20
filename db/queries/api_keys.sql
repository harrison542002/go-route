-- Joins the tenant because a disabled tenant must reject as firmly as a
-- revoked key.
--
-- name: GetAPIKeyByHash :one
SELECT sqlc.embed(api_keys), sqlc.embed(tenants)
FROM api_keys
JOIN tenants ON tenants.id = api_keys.tenant_id
WHERE api_keys.key_hash = $1;

-- name: ListActiveAPIKeys :many
SELECT sqlc.embed(api_keys), sqlc.embed(tenants)
FROM api_keys
JOIN tenants ON tenants.id = api_keys.tenant_id
WHERE api_keys.revoked_at IS NULL
  AND tenants.disabled_at IS NULL;

-- name: ListAPIKeysByTenant :many
SELECT * FROM api_keys
WHERE tenant_id = $1
ORDER BY created_at DESC;

-- name: CreateAPIKey :one
INSERT INTO api_keys (id, tenant_id, key_hash, key_prefix, model_allowlist)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: RevokeAPIKey :one
UPDATE api_keys SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING *;

-- Best-effort and out of band: this answers "is the key still in use
-- before I revoke it", which tolerates being minutes stale.
--
-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = GREATEST(last_used_at, sqlc.arg(used_at))
WHERE id = sqlc.arg(id);
