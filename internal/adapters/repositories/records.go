package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

const (
	gatewayActor = "gateway"
	routeAction  = "route_request"
)

const billableByDefault = true

type RecordWriter struct {
	pool    *pgxpool.Pool
	q       *gen.Queries
	tenants *TenantIDs
}

var _ ports.RecordWriter = (*RecordWriter)(nil)

func NewRecordWriter(pool *pgxpool.Pool) *RecordWriter {
	q := gen.New(pool)
	return &RecordWriter{pool: pool, q: q, tenants: NewTenantIDs(q)}
}

// Write persists a batch as one transaction.
func (w *RecordWriter) Write(ctx context.Context, batch []domains.RoutingDecision) error {
	if len(batch) == 0 {
		return nil
	}

	ledger := make([]ledgerRow, 0, len(batch))
	audit := make([]auditRow, 0, len(batch))

	for _, d := range batch {
		// An unknown tenant here means one got past ingress, which is a
		// bug rather than a condition to paper over: the ledger must not
		// invent the customer a charge is attributed to.
		tenantID, err := w.tenants.Lookup(ctx, d.Tenant)
		if errors.Is(err, ports.ErrUnknownTenant) {
			return fmt.Errorf("%w: decision %s: %w", ports.ErrRejected, d.ID, err)
		}
		if err != nil {
			return err
		}

		l, err := newLedgerRow(d, tenantID)
		if err != nil {
			return fmt.Errorf("%w: postgres: encode ledger %s: %w", ports.ErrRejected, d.ID, err)
		}
		a, err := newAuditRow(d, tenantID)
		if err != nil {
			return fmt.Errorf("%w: postgres: encode audit %s: %w", ports.ErrRejected, d.ID, err)
		}

		ledger = append(ledger, l)
		audit = append(audit, a)
	}

	ledgerJSON, err := json.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("%w: postgres: encode ledger batch: %w", ports.ErrRejected, err)
	}
	auditJSON, err := json.Marshal(audit)
	if err != nil {
		return fmt.Errorf("%w: postgres: encode audit batch: %w", ports.ErrRejected, err)
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := w.q.WithTx(tx)

	if _, err := q.InsertUsageLedgerRows(ctx, ledgerJSON); err != nil {
		return classify(fmt.Errorf("postgres: insert %d ledger rows: %w", len(ledger), err))
	}
	if _, err := q.InsertAuditLogRows(ctx, auditJSON); err != nil {
		return classify(fmt.Errorf("postgres: insert %d audit rows: %w", len(audit), err))
	}

	if err := tx.Commit(ctx); err != nil {
		return classify(fmt.Errorf("postgres: commit %d rows: %w", len(batch), err))
	}
	return nil
}

func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")) {
		return fmt.Errorf("%w: %w", ports.ErrRejected, err)
	}
	return err
}

type ledgerRow struct {
	ID               uuid.UUID       `json:"id"`
	StartedAt        time.Time       `json:"started_at"`
	TenantID         uuid.UUID       `json:"tenant_id"`
	KeyID            *uuid.UUID      `json:"key_id"`
	RequestID        *string         `json:"request_id"`
	RequestedModel   string          `json:"requested_model"`
	ChosenTarget     *string         `json:"chosen_target"`
	Status           string          `json:"status"`
	InputTokens      int32           `json:"input_tokens"`
	OutputTokens     int32           `json:"output_tokens"`
	CacheReadTokens  int32           `json:"cache_read_tokens"`
	CacheWriteTokens int32           `json:"cache_write_tokens"`
	ReasoningTokens  int32           `json:"reasoning_tokens"`
	CostNanos        *int64          `json:"cost_nanos"`
	TokenSource      string          `json:"token_source"`
	PricingVersion   *string         `json:"pricing_version"`
	Billable         bool            `json:"billable"`
	TtftMs           *int32          `json:"ttft_ms"`
	TotalMs          *int32          `json:"total_ms"`
	Metadata         json.RawMessage `json:"metadata"`
	Counterfactuals  json.RawMessage `json:"counterfactuals"`
}

type auditRow struct {
	ID             uuid.UUID       `json:"id"`
	Ts             time.Time       `json:"ts"`
	TenantID       *uuid.UUID      `json:"tenant_id"`
	KeyID          *uuid.UUID      `json:"key_id"`
	Actor          string          `json:"actor"`
	Action         string          `json:"action"`
	RequestID      *string         `json:"request_id"`
	ReasonKind     *string         `json:"reason_kind"`
	ReasonDetail   *string         `json:"reason_detail"`
	PolicyVersion  *int32          `json:"policy_version"`
	Ladder         json.RawMessage `json:"ladder"`
	LadderAttempts json.RawMessage `json:"ladder_attempts"`
	FinalTarget    *string         `json:"final_target"`
	StatusCode     *int32          `json:"status_code"`
	Error          *string         `json:"error"`
}

func newLedgerRow(d domains.RoutingDecision, tenantID uuid.UUID) (ledgerRow, error) {
	metadata, err := jsonOr(d.Request.Metadata, len(d.Request.Metadata) == 0, "{}")
	if err != nil {
		return ledgerRow{}, err
	}

	var (
		costNanos       *int64
		pricingVersion  *string
		counterfactuals = json.RawMessage("[]")
	)
	if d.Cost != nil {
		n := int64(d.Cost.Actual)
		costNanos = &n
		pricingVersion = &d.Cost.PriceTableVersion
		counterfactuals, err = jsonOr(d.Cost.Counterfactuals, len(d.Cost.Counterfactuals) == 0, "[]")
		if err != nil {
			return ledgerRow{}, err
		}
	}

	return ledgerRow{
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

func newAuditRow(d domains.RoutingDecision, tenantID uuid.UUID) (auditRow, error) {
	ladder, err := jsonOr(d.Ladder.Targets, len(d.Ladder.Targets) == 0, "[]")
	if err != nil {
		return auditRow{}, err
	}
	attempts, err := jsonOr(d.Outcome.Attempts, len(d.Outcome.Attempts) == 0, "[]")
	if err != nil {
		return auditRow{}, err
	}

	var policyVersion *int32
	if d.Ladder.Reason.PolicyVersion > 0 {
		v := int32(d.Ladder.Reason.PolicyVersion)
		policyVersion = &v
	}

	return auditRow{
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

// jsonOr encodes v, or returns fallback when v is empty. Go encodes a nil map or
// slice as null, which these NOT NULL JSONB columns would take as SQL NULL
// rather than as an empty document.
func jsonOr(v any, empty bool, fallback string) (json.RawMessage, error) {
	if empty {
		return json.RawMessage(fallback), nil
	}
	return json.Marshal(v)
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
