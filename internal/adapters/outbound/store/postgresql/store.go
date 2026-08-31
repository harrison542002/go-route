package postgresql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// Store reads the decision log. It shares a schema with PostgresWriter
// but nothing else: writes are batched appends on a background
// goroutine, reads are ad-hoc aggregates from the Client (such as CLI).
type Store struct {
	pool *pgxpool.Pool
}

var _ ports.DecisionStore = (*Store)(nil)

func NewStore(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

const getQuery = `
SELECT id, occurred_at, tenant,
       requested_model, chosen_target, status, reason_kind, reason_detail, policy_version,
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
       cost_nanos, price_table_version,
       ttft_ms, total_ms,
       metadata, attempts, counterfactuals, ladder
FROM decisions
WHERE id = $1`

func (s *Store) Get(ctx context.Context, id domains.DecisionID) (domains.RoutingDecision, error) {
	var (
		d                                           domains.RoutingDecision
		chosen                                      *string
		reasonKind, reasonDetail                    string
		policyVersion                               *int
		costNanos                                   *int64
		priceTable                                  *string
		metadata, attempts, counterfactuals, ladder []byte
		rawID                                       [16]byte
	)

	err := s.pool.QueryRow(ctx, getQuery, id.UUID()).Scan(
		&rawID, &d.OccurredAt, &d.Tenant,
		&d.Request.RequestedModel, &chosen, &d.Outcome.Status, &reasonKind, &reasonDetail, &policyVersion,
		&d.Outcome.Usage.Input, &d.Outcome.Usage.Output,
		&d.Outcome.Usage.CacheRead, &d.Outcome.Usage.CacheWrite, &d.Outcome.Usage.Reasoning,
		&costNanos, &priceTable,
		&d.Outcome.TTFTMs, &d.Outcome.TotalMs,
		&metadata, &attempts, &counterfactuals, &ladder,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domains.RoutingDecision{}, fmt.Errorf("%w: %s", ports.ErrDecisionNotFound, id)
	}
	if err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("postgres: get %s: %w", id, err)
	}

	d.ID = id

	d.Ladder.Reason = domains.Reason{Kind: domains.ReasonKind(reasonKind)}
	switch d.Ladder.Reason.Kind {
	case domains.ReasonModelAlias:
		d.Ladder.Reason.ModelAlias = reasonDetail
	case domains.ReasonRuleMatch:
		d.Ladder.Reason.RuleName = reasonDetail
	}
	if policyVersion != nil {
		d.Ladder.Reason.PolicyVersion = *policyVersion
	}

	for _, u := range []struct {
		raw  []byte
		into any
	}{
		{metadata, &d.Request.Metadata},
		{attempts, &d.Outcome.Attempts},
		{ladder, &d.Ladder.Targets},
	} {
		if err := json.Unmarshal(u.raw, u.into); err != nil {
			return domains.RoutingDecision{}, fmt.Errorf("postgres: decode %s: %w", id, err)
		}
	}

	if costNanos != nil {
		d.Cost = &domains.CostBreakdown{Actual: domains.USD(*costNanos)}
		if priceTable != nil {
			d.Cost.PriceTableVersion = *priceTable
		}
		if err := json.Unmarshal(counterfactuals, &d.Cost.Counterfactuals); err != nil {
			return domains.RoutingDecision{}, fmt.Errorf("postgres: decode counterfactuals %s: %w", id, err)
		}
	}

	return d, nil
}

func (s *Store) Aggregate(ctx context.Context, spec ports.ReportSpec) (domains.Report, error) {
	if err := spec.Validate(); err != nil {
		return domains.Report{}, err
	}

	limit := spec.Limit
	if limit == 0 {
		limit = ports.DefaultReportLimit
	}

	expr, extra := groupExpr(spec)
	args := append([]any{string(spec.Tenant), spec.Since, spec.Until}, extra...)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return domains.Report{}, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, truncated, err := aggregateRows(ctx, tx, expr, args, limit)
	if err != nil {
		return domains.Report{}, err
	}

	if err := attachComparisons(ctx, tx, expr, args, rows); err != nil {
		return domains.Report{}, err
	}

	return domains.Report{
		Spec:            domains.ReportRange{Tenant: spec.Tenant, Since: spec.Since, Until: spec.Until},
		Rows:            rows,
		Total:           totalise(rows),
		TruncatedGroups: truncated,
	}, nil
}

const aggregateTemplate = `
WITH grouped AS (
    SELECT
        %s                                          AS key,
        count(*)                                    AS requests,
        COALESCE(SUM(cost_nanos), 0)                AS cost_nanos,
        count(*) FILTER (WHERE cost_nanos IS NULL)  AS unpriced,
        COALESCE(SUM(input_tokens), 0)              AS input_tokens,
        COALESCE(SUM(output_tokens), 0)             AS output_tokens,
        COALESCE(SUM(cache_read_tokens), 0)         AS cache_read_tokens,
        COALESCE(SUM(cache_write_tokens), 0)        AS cache_write_tokens,
        COALESCE(SUM(reasoning_tokens), 0)          AS reasoning_tokens,
        count(*) FILTER (WHERE status = 'ok')                 AS ok,
        count(*) FILTER (WHERE status = 'exhausted')          AS failed,
        count(*) FILTER (WHERE status = 'truncated')          AS truncated,
        count(*) FILTER (WHERE status = 'client_disconnect')  AS disconnected,
        -- Percentiles over requests that produced a first token only.
        -- An exhausted request has ttft_ms = 0 by construction, and
        -- including those would make latency look better the more
        -- failures a group has.
        COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY ttft_ms)
                 FILTER (WHERE ttft_ms > 0), 0)     AS p50_ttft,
        COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
                 FILTER (WHERE ttft_ms > 0), 0)     AS p95_ttft,
        COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY total_ms)
                 FILTER (WHERE total_ms > 0), 0)    AS p95_total
    FROM decisions
    WHERE tenant = $1 AND occurred_at >= $2 AND occurred_at < $3
    GROUP BY 1
)
SELECT *, count(*) OVER () AS total_groups
FROM grouped
ORDER BY cost_nanos DESC, requests DESC
LIMIT %d`

func aggregateRows(
	ctx context.Context, tx pgx.Tx, expr string, args []any, limit int,
) ([]domains.ReportRow, int64, error) {
	q := fmt.Sprintf(aggregateTemplate, expr, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: aggregate: %w", err)
	}
	defer rows.Close()

	var (
		out         []domains.ReportRow
		totalGroups int64
	)
	for rows.Next() {
		var (
			r                          domains.ReportRow
			costNanos                  int64
			p50TTFT, p95TTFT, p95Total float64
		)

		if err := rows.Scan(
			&r.Key, &r.Requests, &costNanos, &r.Unpriced,
			&r.Usage.Input, &r.Usage.Output, &r.Usage.CacheRead,
			&r.Usage.CacheWrite, &r.Usage.Reasoning,
			&r.OK, &r.Failed, &r.Truncated, &r.Disconnected,
			&p50TTFT, &p95TTFT, &p95Total,
			&totalGroups,
		); err != nil {
			return nil, 0, fmt.Errorf("postgres: scan aggregate: %w", err)
		}

		r.Cost = domains.USD(costNanos)
		r.P50TTFTMs = int(p50TTFT)
		r.P95TTFTMs = int(p95TTFT)
		r.P95TotalMs = int(p95Total)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var truncated int64
	if totalGroups > int64(len(out)) {
		truncated = totalGroups - int64(len(out))
	}
	return out, truncated, nil
}

// groupExpr returns the SQL expression for a grouping.
//
// The returned string is INTERPOLATED into the query text, so it must
// come only from this fixed switch. Caller-supplied values are bound as
// parameters instead. The metadata key is $4, never part of the
// expression. Any new case must follow that rule or it becomes an
// injection hole.
func groupExpr(spec ports.ReportSpec) (expr string, args []any) {
	switch spec.GroupBy {
	case ports.GroupByModel:
		return "requested_model", nil
	case ports.GroupByTarget:
		return "COALESCE(chosen_target, '(unserved)')", nil
	case ports.GroupByStatus:
		return "status", nil
	case ports.GroupByDay:
		return "to_char(occurred_at, 'YYYY-MM-DD')", nil
	case ports.GroupByMetadata:
		return "COALESCE(metadata->>$4, '(unset)')", []any{spec.MetaKey}
	default:
		return "''", nil
	}
}

func totalise(rows []domains.ReportRow) domains.ReportRow {
	total := domains.ReportRow{Comparisons: map[string]domains.Comparison{}}

	for _, r := range rows {
		total.Requests += r.Requests
		total.Cost += r.Cost
		total.Unpriced += r.Unpriced
		total.Usage.Input += r.Usage.Input
		total.Usage.Output += r.Usage.Output
		total.Usage.CacheRead += r.Usage.CacheRead
		total.Usage.CacheWrite += r.Usage.CacheWrite
		total.Usage.Reasoning += r.Usage.Reasoning
		total.OK += r.OK
		total.Failed += r.Failed
		total.Truncated += r.Truncated
		total.Disconnected += r.Disconnected

		for target, c := range r.Comparisons {
			t := total.Comparisons[target]
			t.Cost += c.Cost
			t.Requests += c.Requests
			total.Comparisons[target] = t
		}
	}

	return total
}

const comparisonTemplate = `
SELECT %s                          AS key,
       cf->>'target'               AS target,
       SUM((cf->>'cost')::bigint)  AS cost_nanos,
       count(*)                    AS requests
FROM decisions
CROSS JOIN LATERAL jsonb_array_elements(
    -- A decision may carry no comparisons at all, in which case the
    -- column holds jsonb null rather than an array. Expanding a scalar
    -- is an error, so coerce anything that is not an array to empty.
    CASE WHEN jsonb_typeof(counterfactuals) = 'array'
         THEN counterfactuals
         ELSE '[]'::jsonb END) cf
WHERE tenant = $1 AND occurred_at >= $2 AND occurred_at < $3
  AND cf->>'target' IS NOT NULL
GROUP BY 1, 2`

func attachComparisons(
	ctx context.Context, tx pgx.Tx, expr string, args []any, rows []domains.ReportRow,
) error {
	q, err := tx.Query(ctx, fmt.Sprintf(comparisonTemplate, expr), args...)
	if err != nil {
		return fmt.Errorf("postgres: comparisons: %w", err)
	}
	defer q.Close()

	byKey := make(map[string]map[string]domains.Comparison)
	for q.Next() {
		var key, target string
		var cost, requests int64
		if err := q.Scan(&key, &target, &cost, &requests); err != nil {
			return fmt.Errorf("postgres: scan comparison: %w", err)
		}
		if byKey[key] == nil {
			byKey[key] = map[string]domains.Comparison{}
		}
		byKey[key][target] = domains.Comparison{
			Cost:     domains.USD(cost),
			Requests: requests,
		}
	}
	if err := q.Err(); err != nil {
		return err
	}

	for i := range rows {
		rows[i].Comparisons = byKey[rows[i].Key]
	}
	return nil
}
