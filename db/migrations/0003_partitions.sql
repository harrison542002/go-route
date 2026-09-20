CREATE FUNCTION ensure_month_partition(parent regclass, month timestamptz)
    RETURNS void
    LANGUAGE plpgsql AS $$
DECLARE
    start_ts timestamptz := date_trunc('month', month);
    end_ts   timestamptz := date_trunc('month', month) + interval '1 month';
    child    text := format('%s_%s', parent::text, to_char(start_ts, 'YYYYMM'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
        child, parent::text, start_ts, end_ts
    );
END;
$$;

COMMENT ON FUNCTION ensure_month_partition(regclass, timestamptz) IS
    'Creates the partition covering month for parent, if missing. Idempotent and safe to call on every startup.';

CREATE FUNCTION ensure_record_partitions(from_ts timestamptz, months integer)
    RETURNS void
    LANGUAGE plpgsql AS $$
DECLARE
    n integer;
BEGIN
    FOR n IN -1 .. GREATEST(months, 1) LOOP
        PERFORM ensure_month_partition('usage_ledger'::regclass, from_ts + (n || ' month')::interval);
        PERFORM ensure_month_partition('audit_log'::regclass,    from_ts + (n || ' month')::interval);
    END LOOP;
END;
$$;

COMMENT ON FUNCTION ensure_record_partitions(timestamptz, integer) IS
    'Pre-creates monthly partitions for both record tables, starting one month back so a clock stepped backwards or a late-arriving batch still lands somewhere. There is deliberately no DEFAULT partition: one would catch inserts that fall outside every month, but rows landing in it make the later CREATE for that month fail, turning a loud error now into a stuck partition schedule later.';
