package postgresql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// Store reads the record tables. It shares a schema with PostgresWriter
// but nothing else: writes are batched appends on a background
// goroutine, reads are ad-hoc aggregates from the CLI.
type Store struct {
	pool    *pgxpool.Pool
	q       *gen.Queries
	tenants *TenantIDs
}

var _ ports.DecisionStore = (*Store)(nil)

// NewStore opens its own pool. The reader is the CLI -- a separate
// process from the gateway -- so there is no pool to share with.
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	pool, err := Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	q := gen.New(pool)
	return &Store{pool: pool, q: q, tenants: NewTenantIDs(q)}, nil
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// Get reassembles one decision from the two tables it was split across:
// usage_ledger for what it cost, audit_log for why it went where it did.
func (s *Store) Get(ctx context.Context, id domains.DecisionID) (domains.RoutingDecision, error) {
	entry, err := s.q.GetUsageLedgerEntry(ctx, id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		return domains.RoutingDecision{}, fmt.Errorf("%w: %s", ports.ErrDecisionNotFound, id)
	}
	if err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("postgres: get %s: %w", id, err)
	}

	tenant, err := s.tenants.Name(ctx, entry.TenantID)
	if err != nil {
		return domains.RoutingDecision{}, err
	}

	d := domains.RoutingDecision{
		ID:         id,
		OccurredAt: entry.StartedAt,
		Tenant:     tenant,
		Request: domains.RequestSummary{
			RequestedModel: entry.RequestedModel,
		},
		Outcome: domains.Outcome{
			Status: domains.Status(entry.Status),
			Usage: domains.TokenUsage{
				Input:      int(entry.InputTokens),
				Output:     int(entry.OutputTokens),
				CacheRead:  int(entry.CacheReadTokens),
				CacheWrite: int(entry.CacheWriteTokens),
				Reasoning:  int(entry.ReasoningTokens),
			},
			TTFTMs:  derefInt(entry.TtftMs),
			TotalMs: derefInt(entry.TotalMs),
		},
	}

	if err := json.Unmarshal(entry.Metadata, &d.Request.Metadata); err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("postgres: decode metadata %s: %w", id, err)
	}

	if entry.CostNanos != nil {
		d.Cost = &domains.CostBreakdown{Actual: domains.USD(*entry.CostNanos)}
		if entry.PricingVersion != nil {
			d.Cost.PriceTableVersion = *entry.PricingVersion
		}
		if err := json.Unmarshal(entry.Counterfactuals, &d.Cost.Counterfactuals); err != nil {
			return domains.RoutingDecision{}, fmt.Errorf("postgres: decode counterfactuals %s: %w", id, err)
		}
	}

	// A ledger row without an audit row still answers the money
	// question, so `explain` degrades rather than failing outright.
	audit, err := s.q.GetAuditEntry(ctx, id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("postgres: get audit %s: %w", id, err)
	}

	if err := hydrateLadder(&d, audit); err != nil {
		return domains.RoutingDecision{}, fmt.Errorf("postgres: decode %s: %w", id, err)
	}
	return d, nil
}

func hydrateLadder(d *domains.RoutingDecision, audit gen.AuditLog) error {
	if audit.ReasonKind != nil {
		d.Ladder.Reason.Kind = domains.ReasonKind(*audit.ReasonKind)
	}
	if audit.ReasonDetail != nil {
		switch d.Ladder.Reason.Kind {
		case domains.ReasonModelAlias:
			d.Ladder.Reason.ModelAlias = *audit.ReasonDetail
		case domains.ReasonRuleMatch:
			d.Ladder.Reason.RuleName = *audit.ReasonDetail
		}
	}
	if audit.PolicyVersion != nil {
		d.Ladder.Reason.PolicyVersion = int(*audit.PolicyVersion)
	}

	if err := json.Unmarshal(audit.Ladder, &d.Ladder.Targets); err != nil {
		return err
	}
	return json.Unmarshal(audit.LadderAttempts, &d.Outcome.Attempts)
}

func (s *Store) Aggregate(ctx context.Context, spec ports.ReportSpec) (domains.Report, error) {
	if err := spec.Validate(); err != nil {
		return domains.Report{}, err
	}

	empty := domains.Report{
		Spec:  domains.ReportRange{Tenant: spec.Tenant, Since: spec.Since, Until: spec.Until},
		Total: domains.ReportRow{Comparisons: map[string]domains.Comparison{}},
	}

	// An unknown tenant is an empty report, not an error.
	tenantID, err := s.tenants.Lookup(ctx, spec.Tenant)
	if errors.Is(err, ports.ErrUnknownTenant) {
		return empty, nil
	}
	if err != nil {
		return domains.Report{}, err
	}

	limit := spec.Limit
	if limit == 0 {
		limit = ports.DefaultReportLimit
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return domains.Report{}, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	grouped, err := q.AggregateUsage(ctx, gen.AggregateUsageParams{
		TenantID: tenantID,
		Since:    spec.Since,
		Until:    spec.Until,
		GroupBy:  string(spec.GroupBy),
		MetaKey:  spec.MetaKey,
		RowLimit: int32(limit),
	})
	if err != nil {
		return domains.Report{}, fmt.Errorf("postgres: aggregate: %w", err)
	}

	rows := make([]domains.ReportRow, 0, len(grouped))
	var totalGroups int64
	for _, g := range grouped {
		totalGroups = g.TotalGroups
		rows = append(rows, domains.ReportRow{
			Key:      g.Key,
			Requests: g.Requests,
			Cost:     domains.USD(g.CostNanos),
			Unpriced: g.Unpriced,
			Usage: domains.TokenUsage{
				Input:      int(g.InputTokens),
				Output:     int(g.OutputTokens),
				CacheRead:  int(g.CacheReadTokens),
				CacheWrite: int(g.CacheWriteTokens),
				Reasoning:  int(g.ReasoningTokens),
			},
			OK:           g.Ok,
			Failed:       g.Failed,
			Truncated:    g.Truncated,
			Disconnected: g.Disconnected,
			P50TTFTMs:    int(g.P50TtftMs),
			P95TTFTMs:    int(g.P95TtftMs),
			P95TotalMs:   int(g.P95TotalMs),
		})
	}

	if err := s.attachComparisons(ctx, q, spec, tenantID, rows); err != nil {
		return domains.Report{}, err
	}

	var truncated int64
	if totalGroups > int64(len(rows)) {
		truncated = totalGroups - int64(len(rows))
	}

	return domains.Report{
		Spec:            domains.ReportRange{Tenant: spec.Tenant, Since: spec.Since, Until: spec.Until},
		Rows:            rows,
		Total:           totalise(rows),
		TruncatedGroups: truncated,
	}, nil
}

func (s *Store) attachComparisons(
	ctx context.Context,
	q *gen.Queries,
	spec ports.ReportSpec,
	tenantID uuid.UUID,
	rows []domains.ReportRow,
) error {
	found, err := q.AggregateCounterfactuals(ctx, gen.AggregateCounterfactualsParams{
		TenantID: tenantID,
		Since:    spec.Since,
		Until:    spec.Until,
		GroupBy:  string(spec.GroupBy),
		MetaKey:  spec.MetaKey,
	})
	if err != nil {
		return fmt.Errorf("postgres: comparisons: %w", err)
	}

	byKey := make(map[string]map[string]domains.Comparison, len(rows))
	for _, c := range found {
		if byKey[c.Key] == nil {
			byKey[c.Key] = map[string]domains.Comparison{}
		}
		byKey[c.Key][c.Target] = domains.Comparison{
			Cost:     domains.USD(c.CostNanos),
			Requests: c.Requests,
		}
	}

	for i := range rows {
		rows[i].Comparisons = byKey[rows[i].Key]
	}
	return nil
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

func derefInt(p *int32) int {
	if p == nil {
		return 0
	}
	return int(*p)
}
