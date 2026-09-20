CREATE TABLE tenants (
    id          UUID        PRIMARY KEY,
    external_id TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    metadata    JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,

    CONSTRAINT tenants_external_id_not_blank CHECK (external_id <> '')
);

CREATE UNIQUE INDEX tenants_external_id_key ON tenants (external_id);

COMMENT ON TABLE tenants IS
    'One row per customer of your customer. Never deleted, only disabled: the ledger refers to them and history has to stay readable.';
COMMENT ON COLUMN tenants.external_id IS
    'The tenant id in the customer''s own system. Unique, so a retrying signup flow cannot create two tenants and reconciliation is a plain join.';
COMMENT ON COLUMN tenants.disabled_at IS
    'Set instead of deleting the row. A disabled tenant is rejected at ingress but stays attributable in old ledger rows.';

CREATE TABLE api_keys (
    id              UUID        PRIMARY KEY,
    tenant_id       UUID        NOT NULL REFERENCES tenants (id),
    key_hash        BYTEA       NOT NULL,
    key_prefix      TEXT        NOT NULL,
    model_allowlist TEXT[],
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,

    CONSTRAINT api_keys_hash_is_sha256 CHECK (octet_length(key_hash) = 32),
    CONSTRAINT api_keys_prefix_not_blank CHECK (key_prefix <> '')
);

COMMENT ON TABLE api_keys IS
    'The credentials a tenant puts in an SDK. One per environment or per app.';
COMMENT ON COLUMN api_keys.key_hash IS
    'SHA-256 of the presented key. The key itself is never stored, so a database leak hands nobody a working credential.';
COMMENT ON COLUMN api_keys.key_prefix IS
    'The visible head of the key, for a UI to show gr_a1b2c3... Identification only, never authentication.';
COMMENT ON COLUMN api_keys.model_allowlist IS
    'Aliases this key may request, which is how a customer builds tiers. NULL allows every alias; the empty array allows none.';
COMMENT ON COLUMN api_keys.last_used_at IS
    'Written best-effort and out of band. Answers whether a key is still in use before revoking it, which tolerates being stale.';
COMMENT ON COLUMN api_keys.revoked_at IS
    'A timestamp, not a delete, so a revoked key stays attributable in ledger rows written before it was pulled.';

CREATE UNIQUE INDEX api_keys_key_hash_key ON api_keys (key_hash);
CREATE INDEX api_keys_tenant ON api_keys (tenant_id);

CREATE FUNCTION notify_api_key_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('api_keys_changed', COALESCE(NEW.id, OLD.id)::text);
    RETURN NULL;
END;
$$;

CREATE TRIGGER api_keys_changed
    AFTER INSERT OR UPDATE OR DELETE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION notify_api_key_change();

COMMENT ON FUNCTION notify_api_key_change() IS
    'Authentication reads from an in-memory cache rather than paying a round trip per request. This is how that cache learns it is stale, so a revocation propagates to every gateway process in milliseconds.';
