-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys WHERE key = $1 AND expires_at > now();

-- Returns nothing when the key is already held. The caller compares
-- request_hash itself: a match replays, a mismatch is 422.
--
-- name: PutIdempotencyKey :one
INSERT INTO idempotency_keys (key, request_hash, status_code, response_body)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key) DO NOTHING
RETURNING *;

-- name: DeleteExpiredIdempotencyKeys :exec
DELETE FROM idempotency_keys WHERE expires_at <= now();
