CREATE TABLE audit_log (
    id              UUID        NOT NULL,
    ts              TIMESTAMPTZ NOT NULL,

    tenant_id       UUID,
    key_id          UUID,

    actor           TEXT        NOT NULL,
    action          TEXT        NOT NULL,
    request_id      TEXT,

    reason_kind     TEXT,
    reason_detail   TEXT,
    policy_version  INTEGER,

    ladder          JSONB       NOT NULL DEFAULT '[]',
    ladder_attempts JSONB       NOT NULL DEFAULT '[]',

    final_target    TEXT,
    status_code     INTEGER,
    error           TEXT,

    PRIMARY KEY (id, ts),

    CONSTRAINT audit_log_actor_not_blank CHECK (actor <> ''),
    CONSTRAINT audit_log_action_not_blank CHECK (action <> '')
) PARTITION BY RANGE (ts);

COMMENT ON TABLE audit_log IS
    'Different question from usage_ledger, hence a different table. The ledger answers what a tenant spent this month and is read in aggregate over millions of rows; this answers what happened to one request at 14:02 and is read a row at a time, during an incident. One table trying to serve both ends up bad at both. Partitioned and foreign-key-free for the same reasons as the ledger.';
COMMENT ON COLUMN audit_log.id IS
    'Shared with the usage_ledger row for the same request.';
COMMENT ON COLUMN audit_log.tenant_id IS
    'NULL for an admin action taken before any tenant exists.';
COMMENT ON COLUMN audit_log.actor IS
    'gateway for routed traffic, admin:<subject> for a mutation over the admin API, so a quota change is attributable to whoever made it.';
COMMENT ON COLUMN audit_log.ladder IS
    'The targets that were eligible, in the order they would be tried.';
COMMENT ON COLUMN audit_log.ladder_attempts IS
    'Every target actually tried, with status, latency and error. The failover story made debuggable, and the answer to why a request was slow.';
COMMENT ON COLUMN audit_log.error IS
    'The message from the final attempt when it failed. A column rather than only a field inside ladder_attempts because it is what an operator reads first.';

CREATE INDEX audit_log_request_id ON audit_log (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX audit_log_tenant_time ON audit_log (tenant_id, ts DESC) WHERE tenant_id IS NOT NULL;
CREATE INDEX audit_log_action_time ON audit_log (action, ts DESC);

COMMENT ON INDEX audit_log_request_id IS
    'The incident query: someone has a request id from a log line or a response header.';
