CREATE TABLE idempotency_keys (
    key           TEXT        PRIMARY KEY,
    request_hash  BYTEA       NOT NULL,
    status_code   INTEGER     NOT NULL,
    response_body JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now() + interval '24 hours',

    CONSTRAINT idempotency_keys_hash_is_sha256 CHECK (octet_length(request_hash) = 32),
    CONSTRAINT idempotency_keys_status_code CHECK (status_code BETWEEN 100 AND 599)
);

COMMENT ON TABLE idempotency_keys IS
    'Guards the admin API, not tenant data, which is why it links to nothing else in this schema. A customer signup flow will time out and retry, and a retried tenant creation must return the original tenant rather than making a second one, so the whole response is stored and not just a flag.';
COMMENT ON COLUMN idempotency_keys.request_hash IS
    'The same key arriving with a different body is a bug on the caller side: return 422 loudly rather than silently doing something surprising.';
COMMENT ON COLUMN idempotency_keys.expires_at IS
    'Rows live 24 hours. This is the only table here that accumulates rows nobody will read again.';

CREATE INDEX idempotency_keys_expires_at ON idempotency_keys (expires_at);
