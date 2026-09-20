CREATE TABLE usage_ledger (
    id                 UUID        NOT NULL,
    started_at         TIMESTAMPTZ NOT NULL,

    tenant_id          UUID        NOT NULL,
    key_id             UUID,
    request_id         TEXT,

    requested_model    TEXT        NOT NULL,
    chosen_target      TEXT,
    status             TEXT        NOT NULL,

    input_tokens       INTEGER     NOT NULL DEFAULT 0,
    output_tokens      INTEGER     NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER     NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER     NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER     NOT NULL DEFAULT 0,

    cost_nanos         BIGINT,
    token_source       TEXT        NOT NULL DEFAULT 'provider',
    pricing_version    TEXT,
    billable           BOOLEAN     NOT NULL DEFAULT true,

    ttft_ms            INTEGER,
    total_ms           INTEGER,

    metadata           JSONB       NOT NULL DEFAULT '{}',
    counterfactuals    JSONB       NOT NULL DEFAULT '[]',

    PRIMARY KEY (id, started_at),

    CONSTRAINT usage_ledger_token_source CHECK (token_source IN ('provider', 'estimate')),
    CONSTRAINT usage_ledger_tokens_non_negative CHECK (
        input_tokens >= 0 AND output_tokens >= 0 AND cache_read_tokens >= 0
        AND cache_write_tokens >= 0 AND reasoning_tokens >= 0
    )
) PARTITION BY RANGE (started_at);

COMMENT ON TABLE usage_ledger IS
    'Append-only, one row per request, and the source of truth for money. A figure is corrected by writing another row, never by updating this one. Range-partitioned by month so retention is a DROP of an old partition rather than a DELETE that runs for an hour; that forces the partition column into the primary key, hence (id, started_at). There is no foreign key to tenants or api_keys on purpose: this is written in batches of a second of traffic, and an FK check per row would tax every insert to re-verify a tenant the gateway just authenticated.';
COMMENT ON COLUMN usage_ledger.id IS
    'Shared with the audit_log row for the same request, so explain is two point lookups rather than a join between two partitioned tables.';
COMMENT ON COLUMN usage_ledger.requested_model IS
    'The alias the client asked for. Here rather than in audit_log because it is a dimension spend is sliced by.';
COMMENT ON COLUMN usage_ledger.cost_nanos IS
    'Nanodollars, matching domains.USD. NULL means not priced, which is a different fact from a computed zero and is counted separately in reports.';
COMMENT ON COLUMN usage_ledger.token_source IS
    'Whether the token counts came back from the provider or from our own estimator. The first thing anyone asks for when a customer disputes an invoice.';
COMMENT ON COLUMN usage_ledger.pricing_version IS
    'Pins which price table produced cost_nanos, because provider prices change and a retrospectively recomputed cost is a support nightmare.';
COMMENT ON COLUMN usage_ledger.billable IS
    'Whether this row reaches an invoice. Distinct from cost_nanos = 0, which says the request was free: a row may have cost real money that someone decided not to charge for. Changed by writing a correcting row, not by an UPDATE.';
COMMENT ON COLUMN usage_ledger.counterfactuals IS
    'What the same traffic would have cost on other targets. A money question, so it lives with the money.';

CREATE INDEX usage_ledger_tenant_time ON usage_ledger (tenant_id, started_at DESC);
CREATE INDEX usage_ledger_metadata ON usage_ledger USING GIN (metadata);
CREATE INDEX usage_ledger_target_time ON usage_ledger (chosen_target, started_at DESC)
    WHERE chosen_target IS NOT NULL;
CREATE INDEX usage_ledger_key_time ON usage_ledger (key_id, started_at DESC)
    WHERE key_id IS NOT NULL;

COMMENT ON INDEX usage_ledger_metadata IS
    'Metadata keys vary per deployment, so GIN beats guessing at columns: WHERE metadata @> {"feature":"auto-tag"} stays index-backed.';
