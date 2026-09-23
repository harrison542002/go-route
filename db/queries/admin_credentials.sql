-- Admin traffic is a handful of calls a minute, so authentication pays
-- for a lookup per request rather than a cache. Revocation is then
-- instant everywhere, with no invalidation channel to get wrong.
--
-- The row is returned whether or not it is revoked or expired: the
-- caller decides, so that an unknown token, a revoked one and an expired
-- one take the same path and the response cannot tell them apart. Expiry
-- is deliberately not a predicate here either, for the same reason it is
-- not a sweep: the row has to survive its own expiry so the audit rows
-- naming it stay readable.
--
-- name: GetAdminCredentialByTokenHash :one
SELECT * FROM admin_credentials WHERE token_hash = $1;

-- Accepts either the name an operator knows or the id a listing shows.
-- The id is cast to text rather than the reference to a UUID, so a name
-- that is not a UUID is compared instead of raising.
--
-- name: GetAdminCredentialByRef :one
SELECT * FROM admin_credentials
WHERE name = sqlc.arg(ref) OR id::text = sqlc.arg(ref);

-- name: ListAdminCredentials :many
SELECT * FROM admin_credentials ORDER BY created_at;

-- Returns no row when the name is taken, which the adapter reports as a
-- conflict. Names are the audit identity, so reusing one would make two
-- callers indistinguishable in the log.
--
-- name: CreateAdminCredential :one
INSERT INTO admin_credentials (id, name, token_hash, token_prefix, role, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (name) DO NOTHING
RETURNING *;

-- name: RevokeAdminCredential :one
UPDATE admin_credentials SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING *;

-- Best-effort and out of band, as with api_keys: this answers "is this
-- credential still in use before I revoke it", which tolerates being
-- minutes stale, and must never fail or delay the request it describes.
--
-- name: TouchAdminCredential :exec
UPDATE admin_credentials SET last_used_at = GREATEST(last_used_at, sqlc.arg(used_at))
WHERE id = sqlc.arg(id);
