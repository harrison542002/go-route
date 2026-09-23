CREATE TABLE admin_credentials (
    id           UUID        PRIMARY KEY,
    name         TEXT        NOT NULL,
    token_hash   BYTEA       NOT NULL,
    token_prefix TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,

    CONSTRAINT admin_credentials_name_not_blank CHECK (name <> ''),
    CONSTRAINT admin_credentials_hash_is_sha256 CHECK (octet_length(token_hash) = 32),
    CONSTRAINT admin_credentials_prefix_not_blank CHECK (token_prefix <> '')
);

CREATE UNIQUE INDEX admin_credentials_name_key ON admin_credentials (name);
CREATE UNIQUE INDEX admin_credentials_token_hash_key ON admin_credentials (token_hash);

COMMENT ON TABLE admin_credentials IS
    'The credentials the internal admin API authenticates. One per caller -- a billing sync job, an operator, a support console -- because a single shared token identifies nobody, and an audit row saying only that somebody held the token answers nothing during an incident. Minted by the CLI, never over the API: an endpoint that mints a credential from a credential is a privilege-escalation surface, and is worth designing on purpose rather than by accident.';
COMMENT ON COLUMN admin_credentials.name IS
    'Who this credential is, and the audit identity every mutation made with it is recorded under, as admin:<name>. Unique, so two callers cannot answer to the same name in the log.';
COMMENT ON COLUMN admin_credentials.token_hash IS
    'SHA-256 of the presented token, the same bargain api_keys.key_hash makes: the token itself is never stored, so a database leak hands nobody a working credential.';
COMMENT ON COLUMN admin_credentials.token_prefix IS
    'The visible head of the token, for telling credentials apart in a listing. Identification only, never authentication.';
COMMENT ON COLUMN admin_credentials.last_used_at IS
    'Written best-effort and out of band. Answers whether a credential is still in use before revoking it, which tolerates being stale.';
COMMENT ON COLUMN admin_credentials.revoked_at IS
    'A timestamp, not a delete, so the audit rows a credential wrote stay attributable to the name that wrote them. A revoked credential is refused with the same 401 as an unknown one: the response must not tell a caller whether the token it guessed ever existed.';
