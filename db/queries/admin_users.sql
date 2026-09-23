-- Emails are stored lower-cased and compared as stored, so every lookup
-- here expects the caller to have normalised already. The CHECK on the
-- column is what makes that safe to assume.
--
-- The row is returned whether or not the person is disabled: the caller
-- decides, so that an unknown email and a disabled one take the same
-- path and the response cannot tell them apart.
--
-- name: GetAdminUserByEmail :one
SELECT * FROM admin_users WHERE email = $1;

-- name: GetAdminUserByID :one
SELECT * FROM admin_users WHERE id = $1;

-- name: ListAdminUsers :many
SELECT * FROM admin_users ORDER BY created_at;

-- Returns no row when the email is taken, which the adapter reports as a
-- conflict. The email is the audit identity, so reusing one would make
-- two people indistinguishable in the log.
--
-- name: CreateAdminUser :one
INSERT INTO admin_users (id, email, password_hash, role, password_changed_at)
VALUES ($1, $2, $3, $4, sqlc.arg(password_changed_at))
ON CONFLICT (email) DO NOTHING
RETURNING *;

-- name: DisableAdminUser :one
UPDATE admin_users SET disabled_at = sqlc.arg(disabled_at)
WHERE id = sqlc.arg(id) AND disabled_at IS NULL
RETURNING *;

-- name: SetAdminUserPassword :one
UPDATE admin_users
SET password_hash = sqlc.arg(password_hash), password_changed_at = sqlc.arg(password_changed_at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: TouchAdminUserLogin :exec
UPDATE admin_users SET last_login_at = GREATEST(last_login_at, sqlc.arg(at))
WHERE id = sqlc.arg(id);

-- name: CreateAdminRefreshToken :one
INSERT INTO admin_refresh_tokens (id, user_id, token_hash, issued_at, expires_at)
VALUES ($1, $2, $3, sqlc.arg(issued_at), sqlc.arg(expires_at))
RETURNING *;

-- Joins the person in, because every decision the refresh endpoint makes
-- needs both: the token has to be live and its owner has to still be
-- allowed in, and two round trips would be two chances to answer
-- differently.
--
-- name: GetAdminRefreshTokenByHash :one
SELECT sqlc.embed(t), sqlc.embed(u)
FROM admin_refresh_tokens t
JOIN admin_users u ON u.id = t.user_id
WHERE t.token_hash = $1;

-- Rotation and logout are the same statement: revoked_at always, and
-- replaced_by only when something took this token's place. Returns no
-- row when the token was already revoked, which is what tells a refresh
-- it is looking at a replay rather than a race.
--
-- name: RevokeAdminRefreshToken :one
UPDATE admin_refresh_tokens
SET revoked_at = sqlc.arg(revoked_at), replaced_by = sqlc.narg(replaced_by)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL
RETURNING *;

-- name: RevokeAdminUserRefreshTokens :exec
UPDATE admin_refresh_tokens SET revoked_at = sqlc.arg(revoked_at)
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL;

-- Swept when the person logs in rather than by a background job: a dead
-- refresh token names nothing in the audit log, so deleting it loses no
-- history, and the only rows worth collecting are the ones belonging to
-- somebody who is here anyway.
--
-- name: DeleteExpiredAdminRefreshTokens :exec
DELETE FROM admin_refresh_tokens
WHERE user_id = sqlc.arg(user_id) AND expires_at < sqlc.arg(before);
