-- name: GetTenantByExternalID :one
SELECT * FROM tenants WHERE external_id = $1;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- Every admin mutation of a tenant, its keys or its quotas takes this
-- first, so two writers for one tenant serialise instead of interleaving
-- a quota replace into a merged set neither of them asked for. NO KEY
-- UPDATE rather than UPDATE because it does not conflict with the KEY
-- SHARE lock a foreign-key check takes: the gateway inserting counter
-- rows for this tenant is never made to wait on an admin call.
--
-- name: LockTenant :one
SELECT * FROM tenants WHERE id = $1 FOR NO KEY UPDATE;

-- name: ListTenants :many
SELECT * FROM tenants
WHERE (sqlc.arg(include_disabled)::boolean OR disabled_at IS NULL)
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);

-- Returns nothing when the external_id is already taken. A signup flow
-- retrying after a timeout races itself, and DO NOTHING waits for the
-- other insert to settle instead of aborting the transaction, so the
-- caller can read back whichever row won and compare it.
--
-- name: CreateTenant :one
INSERT INTO tenants (id, external_id, name, metadata)
VALUES ($1, $2, $3, $4)
ON CONFLICT (external_id) DO NOTHING
RETURNING *;

-- external_id is deliberately not updatable: it is the join key into the
-- customer's own system, and changing it would orphan their records.
--
-- name: UpdateTenant :one
UPDATE tenants SET
    name     = COALESCE(sqlc.narg(name), name),
    metadata = COALESCE(sqlc.narg(metadata), metadata)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DisableTenant :one
UPDATE tenants SET disabled_at = now()
WHERE id = $1 AND disabled_at IS NULL
RETURNING *;

-- Returns nothing when the tenant is already enabled, so a repeated
-- call is visibly a no-op rather than a second change.
--
-- name: EnableTenant :one
UPDATE tenants SET disabled_at = NULL
WHERE id = $1 AND disabled_at IS NOT NULL
RETURNING *;
