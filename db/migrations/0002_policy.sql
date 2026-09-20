CREATE TYPE window_kind AS ENUM ('minute', 'hour', 'day', 'month', 'period');

COMMENT ON TYPE window_kind IS
    'How a quota window is bounded. All but period derive their boundary from the clock; period carries explicit dates supplied by the caller, which is what makes anniversary billing work when customers signed up mid-month.';

CREATE TYPE quota_action AS ENUM ('block', 'allow');

COMMENT ON TYPE quota_action IS
    'What happens at the line. block returns 429; allow records the overage and keeps serving, which is how a soft limit is expressed.';

CREATE TABLE quotas (
    tenant_id      UUID         NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    window_kind    window_kind  NOT NULL,

    period_start   TIMESTAMPTZ,
    period_end     TIMESTAMPTZ,

    max_requests   BIGINT,
    max_tokens     BIGINT,
    max_cost_nanos BIGINT,

    on_exceed      quota_action NOT NULL DEFAULT 'block',
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, window_kind),

    CONSTRAINT quotas_period_bounds CHECK (
        (window_kind = 'period') = (period_start IS NOT NULL AND period_end IS NOT NULL)
    ),
    CONSTRAINT quotas_period_order CHECK (
        period_start IS NULL OR period_start < period_end
    ),
    CONSTRAINT quotas_limits_something CHECK (
        max_requests IS NOT NULL OR max_tokens IS NOT NULL OR max_cost_nanos IS NOT NULL
    ),
    CONSTRAINT quotas_limits_non_negative CHECK (
        COALESCE(max_requests, 0) >= 0
        AND COALESCE(max_tokens, 0) >= 0
        AND COALESCE(max_cost_nanos, 0) >= 0
    )
);

COMMENT ON TABLE quotas IS
    'The limits, set entirely over the admin API. go-route does not know what subscription tier produced these numbers; the customer billing system owns that and this enforces whatever it is told. One row per window kind, so a tenant can carry a monthly billing period and a per-minute rate limit at once.';
COMMENT ON COLUMN quotas.max_cost_nanos IS
    'Nanodollars, matching domains.USD.';
COMMENT ON CONSTRAINT quotas_period_bounds ON quotas IS
    'Stated as an equivalence so neither direction can drift: a period row without dates has no window, and a day row with them is a caller misunderstanding worth rejecting.';
COMMENT ON CONSTRAINT quotas_limits_something ON quotas IS
    'A row that limits nothing is a no-op that reads like a limit.';

CREATE TABLE usage_counters (
    tenant_id    UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    window_kind  window_kind NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,

    requests     BIGINT      NOT NULL DEFAULT 0,
    tokens       BIGINT      NOT NULL DEFAULT 0,
    cost_nanos   BIGINT      NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, window_kind, window_start)
);

COMMENT ON TABLE usage_counters IS
    'The durable snapshot behind Redis. Enforcement happens in Redis, because a Postgres write per request would serialise everything through a single writer. This exists because a failover, an eviction or a cold start loses counters, and without it a tenant silently gets a fresh budget. Written as deltas so two instances flushing the same second compose instead of clobbering each other. Its key matches quotas on (tenant_id, window_kind), and that pairing is the enforcement decision itself.';

CREATE INDEX usage_counters_window ON usage_counters (window_kind, window_start);
COMMENT ON INDEX usage_counters_window IS
    'For sweeping every tenant active in a window. Rehydrating one tenant is already served by the primary key.';
