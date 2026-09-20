-- name: GetTenantByExternalID :one
SELECT * FROM tenants WHERE external_id = $1;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- name: ListTenants :many
SELECT * FROM tenants
WHERE (sqlc.arg(include_disabled)::boolean OR disabled_at IS NULL)
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: CreateTenant :one
INSERT INTO tenants (id, external_id, name, metadata)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: DisableTenant :one
UPDATE tenants SET disabled_at = now()
WHERE id = $1 AND disabled_at IS NULL
RETURNING *;

-- name: EnableTenant :one
UPDATE tenants SET disabled_at = NULL
WHERE id = $1
RETURNING *;
