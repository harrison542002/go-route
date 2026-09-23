CREATE TABLE admin_users (
    id                  UUID        PRIMARY KEY,
    email               TEXT        NOT NULL,
    password_hash       TEXT        NOT NULL,
    role                admin_role  NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at         TIMESTAMPTZ,
    last_login_at       TIMESTAMPTZ,
    password_changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT admin_users_email_not_blank CHECK (email <> ''),
    CONSTRAINT admin_users_email_is_lowercase CHECK (email = lower(email)),
    CONSTRAINT admin_users_password_hash_not_blank CHECK (password_hash <> '')
);

CREATE UNIQUE INDEX admin_users_email_key ON admin_users (email);

COMMENT ON TABLE admin_users IS
    'The people who reach the internal admin API, as against the machines admin_credentials holds. A person logs in with an email and a password and is handed a short-lived JWT; a machine keeps presenting its named token. Both resolve to the same actor-and-role pair, so nothing past authentication has to know which kind it was talking to. The row is keyed by an opaque id rather than by the email, so an identity that arrives later carrying an OIDC subject instead of a password is a column on this table and not a new table with a second set of audit identities.';
COMMENT ON COLUMN admin_users.email IS
    'Who this person is, and the audit identity every mutation they make is recorded under, as user:<email>. Stored lower-cased and compared as stored: a CHECK enforces it rather than an index over lower(email), because an index would still let Ops@example.com and ops@example.com sit in the column and an audit row naming one of them would not say which person acted.';
COMMENT ON COLUMN admin_users.password_hash IS
    'An argon2id PHC string -- parameters, salt and digest -- and nothing else. The password is never stored, so a database leak hands nobody a working login, and the parameters travel with the hash so raising them later does not invalidate the rows written under the old ones.';
COMMENT ON COLUMN admin_users.role IS
    'The same admin_role a machine credential carries, deliberately not a second role type: a permission model that has to be reasoned about twice is a permission model that will disagree with itself. This column, not the role copied into an access token for the dashboard to read, is what the permission check authorises against, so a demotion to readonly takes effect on the next request rather than at the end of a token lifetime.';
COMMENT ON COLUMN admin_users.disabled_at IS
    'A timestamp, not a delete, for the reason tenants and credentials are not deleted: the audit rows this person wrote name them, and history has to stay readable. A disabled person cannot log in, cannot refresh, and cannot use an access token already issued to them: this row is read on every request, so disabling takes effect on the next one, exactly as revoking a machine credential does.';
COMMENT ON COLUMN admin_users.last_login_at IS
    'When this person last exchanged a password for a token. Answers whether an account is still in use before disabling it, which tolerates being stale.';
COMMENT ON COLUMN admin_users.password_changed_at IS
    'When the stored hash was last replaced. It is what an operator reads after a suspected leak to tell a rotated password from one that was never changed.';

CREATE TABLE admin_refresh_tokens (
    id          UUID        PRIMARY KEY,
    user_id     UUID        NOT NULL REFERENCES admin_users (id),
    token_hash  BYTEA       NOT NULL,
    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    replaced_by UUID REFERENCES admin_refresh_tokens (id),

    CONSTRAINT admin_refresh_tokens_hash_is_sha256 CHECK (octet_length(token_hash) = 32),
    CONSTRAINT admin_refresh_tokens_expires_after_issue CHECK (expires_at > issued_at)
);

CREATE UNIQUE INDEX admin_refresh_tokens_token_hash_key ON admin_refresh_tokens (token_hash);
CREATE INDEX admin_refresh_tokens_user_id_idx ON admin_refresh_tokens (user_id);
CREATE INDEX admin_refresh_tokens_expires_at_idx ON admin_refresh_tokens (expires_at);

COMMENT ON TABLE admin_refresh_tokens IS
    'The long-lived half of a person''s session, stored so it can be taken away. Kept deliberately small: it is a hash, an owner and three timestamps, because everything else a session might carry -- who they are, what they may do -- is already on admin_users and would only be a second copy to go stale. A refresh rotates: the presented row is revoked and a new one issued, so a token that travels further than the client it was issued to stops working the moment the real client next refreshes.';
COMMENT ON COLUMN admin_refresh_tokens.token_hash IS
    'SHA-256 of the presented refresh token, the bargain api_keys.key_hash and admin_credentials.token_hash both make. The token has enough entropy on its own that no password-grade hash is warranted here -- unlike admin_users.password_hash, nobody chose it.';
COMMENT ON COLUMN admin_refresh_tokens.expires_at IS
    'When this token stops working. Indexed so expired rows can be swept in bulk: unlike a credential, a refresh token names nothing in the audit log, so deleting a dead one loses no history.';
COMMENT ON COLUMN admin_refresh_tokens.revoked_at IS
    'Set by a logout, by a rotation, or by the whole set being cut when a revoked token is presented again. An unknown token, a revoked one, an expired one and one belonging to a disabled person are all refused with the same 401: a client that cannot refresh must log in again, and which of the four it was is the server''s business.';
COMMENT ON COLUMN admin_refresh_tokens.replaced_by IS
    'The token this one was rotated into, NULL for one that was never refreshed. Presenting a token that has already been replaced means two clients hold the same secret, so the whole set is revoked and user.refresh_reuse is audited -- the cheapest theft detection a rotating token gives away for free.';
