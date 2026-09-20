package sink

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/adapters/outbound/store/postgresql"
	"github.com/harrison542002/go-route/internal/core/domains"
)

const (
	gatewayActor = "gateway"
	routeAction  = "route_request"
)

const billableByDefault = true

type PostgresWriter struct {
	pool    *pgxpool.Pool
	q       *gen.Queries
	tenants *postgresql.TenantIDs
}

var _ Writer = (*PostgresWriter)(nil)

func NewPostgresWriter(pool *pgxpool.Pool) *PostgresWriter {
	q := gen.New(pool)
	return &PostgresWriter{pool: pool, q: q, tenants: postgresql.NewTenantIDs(q)}
}

// Write persists a batch as one transaction. Two COPYs without one
// would let a crash leave spend recorded with no explanation of where it
// went.
func (w *PostgresWriter) Write(ctx context.Context, batch []domains.RoutingDecision) error {
	if len(batch) == 0 {
		return nil
	}

	ledger := make([]gen.InsertUsageLedgerParams, 0, len(batch))
	audit := make([]gen.InsertAuditLogParams, 0, len(batch))

	for _, d := range batch {
		// An unknown tenant here means one got past ingress, which is a
		// bug rather than a condition to paper over: the ledger must not
		// invent the customer a charge is attributed to.
		tenantID, err := w.tenants.Lookup(ctx, d.Tenant)
		if err != nil {
			return err
		}

		l, err := ledgerRow(d, tenantID)
		if err != nil {
			return fmt.Errorf("postgres: encode ledger %s: %w", d.ID, err)
		}
		a, err := auditRow(d, tenantID)
		if err != nil {
			return fmt.Errorf("postgres: encode audit %s: %w", d.ID, err)
		}

		ledger = append(ledger, l)
		audit = append(audit, a)
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := w.q.WithTx(tx)

	if _, err := q.InsertUsageLedger(ctx, ledger); err != nil {
		return fmt.Errorf("postgres: copy %d ledger rows: %w", len(ledger), err)
	}
	if _, err := q.InsertAuditLog(ctx, audit); err != nil {
		return fmt.Errorf("postgres: copy %d audit rows: %w", len(audit), err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit %d rows: %w", len(batch), err)
	}
	return nil
}

func ledgerRow(d domains.RoutingDecision, tenantID uuid.UUID) (gen.InsertUsageLedgerParams, error) {
	metadata, err := json.Marshal(d.Request.Metadata)
	if err != nil {
		return gen.InsertUsageLedgerParams{}, err
	}

	var (
		costNanos       *int64
		pricingVersion  *string
		counterfactuals = []byte("[]")
	)
	if d.Cost != nil {
		n := int64(d.Cost.Actual)
		costNanos = &n
		pricingVersion = &d.Cost.PriceTableVersion
		if len(d.Cost.Counterfactuals) > 0 {
			if counterfactuals, err = json.Marshal(d.Cost.Counterfactuals); err != nil {
				return gen.InsertUsageLedgerParams{}, err
			}
		}
	}

	return gen.InsertUsageLedgerParams{
		ID:               d.ID.UUID(),
		StartedAt:        d.OccurredAt,
		TenantID:         tenantID,
		KeyID:            optionalUUID(d.KeyID),
		RequestedModel:   d.Request.RequestedModel,
		ChosenTarget:     optional(d.Outcome.ChosenTarget()),
		Status:           string(d.Outcome.Status),
		InputTokens:      int32(d.Outcome.Usage.Input),
		OutputTokens:     int32(d.Outcome.Usage.Output),
		CacheReadTokens:  int32(d.Outcome.Usage.CacheRead),
		CacheWriteTokens: int32(d.Outcome.Usage.CacheWrite),
		ReasoningTokens:  int32(d.Outcome.Usage.Reasoning),
		CostNanos:        costNanos,
		TokenSource:      "provider",
		PricingVersion:   pricingVersion,
		Billable:         billableByDefault,
		TtftMs:           optionalInt(d.Outcome.TTFTMs),
		TotalMs:          optionalInt(d.Outcome.TotalMs),
		Metadata:         metadata,
		Counterfactuals:  counterfactuals,
	}, nil
}

func auditRow(d domains.RoutingDecision, tenantID uuid.UUID) (gen.InsertAuditLogParams, error) {
	ladder, err := json.Marshal(d.Ladder.Targets)
	if err != nil {
		return gen.InsertAuditLogParams{}, err
	}
	attempts, err := json.Marshal(d.Outcome.Attempts)
	if err != nil {
		return gen.InsertAuditLogParams{}, err
	}

	var policyVersion *int32
	if d.Ladder.Reason.PolicyVersion > 0 {
		v := int32(d.Ladder.Reason.PolicyVersion)
		policyVersion = &v
	}

	return gen.InsertAuditLogParams{
		ID:             d.ID.UUID(),
		Ts:             d.OccurredAt,
		TenantID:       &tenantID,
		KeyID:          optionalUUID(d.KeyID),
		Actor:          gatewayActor,
		Action:         routeAction,
		RequestID:      optional(d.ID.String()),
		ReasonKind:     optional(string(d.Ladder.Reason.Kind)),
		ReasonDetail:   optional(reasonDetail(d.Ladder.Reason)),
		PolicyVersion:  policyVersion,
		Ladder:         ladder,
		LadderAttempts: attempts,
		FinalTarget:    optional(d.Outcome.ChosenTarget()),
		Error:          optional(lastFailure(d.Outcome)),
	}, nil
}

// lastFailure is what an operator reads first, so it gets a column
// rather than living only inside ladder_attempts.
func lastFailure(o domains.Outcome) string {
	if len(o.Attempts) == 0 {
		return ""
	}
	last := o.Attempts[len(o.Attempts)-1]
	if last.Failure == nil {
		return ""
	}
	return last.Failure.Message
}

func reasonDetail(r domains.Reason) string {
	switch r.Kind {
	case domains.ReasonModelAlias:
		return r.ModelAlias
	case domains.ReasonRuleMatch:
		return r.RuleName
	default:
		return ""
	}
}

// optional maps the empty string to NULL, so no query has to ask about
// both.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optionalUUID maps the zero UUID to NULL: a record written with no
// credential behind it must not claim one.
func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func optionalInt(n int) *int32 {
	if n == 0 {
		return nil
	}
	v := int32(n)
	return &v
}
