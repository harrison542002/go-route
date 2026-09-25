ALTER TABLE quotas
    DROP CONSTRAINT quotas_limits_something,
    DROP CONSTRAINT quotas_limits_non_negative,
    DROP COLUMN max_requests,
    DROP COLUMN max_tokens;

DELETE FROM quotas WHERE max_cost_nanos IS NULL;

ALTER TABLE quotas
    ALTER COLUMN max_cost_nanos SET NOT NULL,
    ADD CONSTRAINT quotas_cost_non_negative CHECK (max_cost_nanos >= 0);

COMMENT ON TABLE quotas IS
    'Spend caps, set entirely over the admin API. go-route does not know what subscription tier produced these numbers; the customer billing system owns that and this enforces whatever it is told. One row per window kind, so a tenant can carry a monthly billing cap and a tighter daily one at once.';
COMMENT ON COLUMN quotas.max_cost_nanos IS
    'What the tenant may spend in this window, in nanodollars, matching domains.USD. NOT NULL because a quota that caps nothing is a no-op that reads like a limit; 0 is a real cap that allows nothing, which is how a tenant is stopped without being deleted.';
COMMENT ON CONSTRAINT quotas_cost_non_negative ON quotas IS
    'A negative cap would be crossed by the first request and read as a mistyped zero.';
