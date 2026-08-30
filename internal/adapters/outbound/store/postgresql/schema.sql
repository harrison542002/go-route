-- Decision records: one row per routed request.
CREATE TABLE IF NOT EXISTS decisions (
    id                  UUID PRIMARY KEY,
    occurred_at         TIMESTAMPTZ NOT NULL,
    tenant              TEXT        NOT NULL,

    requested_model     TEXT        NOT NULL,
    chosen_target       TEXT,
    status              TEXT        NOT NULL,
    reason_kind         TEXT        NOT NULL,
    reason_detail       TEXT,
    policy_version      INT,

    input_tokens        INTEGER     NOT NULL DEFAULT 0,
    output_tokens       INTEGER     NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER     NOT NULL DEFAULT 0,
    cache_write_tokens  INTEGER     NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER     NOT NULL DEFAULT 0,

    cost_nanos          BIGINT,
    price_table_version TEXT,

    ttft_ms             INTEGER,
    total_ms            INTEGER,

    attempt_count       INTEGER     NOT NULL DEFAULT 0,

    metadata            JSONB       NOT NULL DEFAULT '{}',
    attempts            JSONB       NOT NULL DEFAULT '[]',
    counterfactuals     JSONB       NOT NULL DEFAULT '[]',
    ladder              JSONB       NOT NULL DEFAULT '[]'
);

-- Every report is bounded by a time range within a tenant.
CREATE INDEX IF NOT EXISTS decisions_tenant_time
    ON decisions (tenant, occurred_at DESC);

-- "Spend by feature last month" is the report people actually want.
-- Metadata keys vary per deployment, so a GIN index beats guessing at
-- columns: WHERE metadata @> '{"feature":"auto-tag"}' is index-backed.
CREATE INDEX IF NOT EXISTS decisions_metadata
    ON decisions USING GIN (metadata);

CREATE INDEX IF NOT EXISTS decisions_target_time
    ON decisions (chosen_target, occurred_at DESC)
    WHERE chosen_target IS NOT NULL;