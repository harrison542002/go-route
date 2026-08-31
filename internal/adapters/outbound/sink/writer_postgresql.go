package sink

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/internal/adapters/outbound/store/postgresql"
	"github.com/harrison542002/go-route/internal/core/domains"
)

type PostgresWriter struct {
	pool *pgxpool.Pool
}

var _ Writer = (*PostgresWriter)(nil)
var columns = []string{
	"id", "occurred_at", "tenant",
	"requested_model", "chosen_target", "status", "reason_kind", "reason_detail", "policy_version",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"cost_nanos", "price_table_version",
	"ttft_ms", "total_ms", "attempt_count",
	"metadata", "attempts", "counterfactuals", "ladder",
}

func NewPostgresWriter(ctx context.Context, dsn string) (*PostgresWriter, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	if _, err := pool.Exec(ctx, postgresql.Schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: schema: %w", err)
	}
	return &PostgresWriter{pool: pool}, nil
}

func (w *PostgresWriter) Close() error {
	w.pool.Close()
	return nil
}

func (w *PostgresWriter) Write(ctx context.Context, batch []domains.RoutingDecision) error {
	rows := make([][]any, 0, len(batch))
	for _, d := range batch {
		row, err := flatten(d)
		if err != nil {
			return fmt.Errorf("postgres: encode %s: %w", d.ID, err)
		}
		rows = append(rows, row)
	}

	_, err := w.pool.CopyFrom(ctx, pgx.Identifier{"decisions"}, columns, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("postgres: copy %d rows: %w", len(rows), err)
	}
	return nil
}

func flatten(d domains.RoutingDecision) ([]any, error) {
	metadata, err := json.Marshal(d.Request.Metadata)
	if err != nil {
		return nil, err
	}
	attempts, err := json.Marshal(d.Outcome.Attempts)
	if err != nil {
		return nil, err
	}
	ladder, err := json.Marshal(d.Ladder.Targets)
	if err != nil {
		return nil, err
	}

	var (
		costNanos   *int64
		priceTable  *string
		counterJSON = []byte("[]")
	)
	if d.Cost != nil {
		n := int64(d.Cost.Actual)
		costNanos = &n
		priceTable = &d.Cost.PriceTableVersion
		if len(d.Cost.Counterfactuals) > 0 {
			if counterJSON, err = json.Marshal(d.Cost.Counterfactuals); err != nil {
				return nil, err
			}
		}
	}

	var chosen *string
	if t := d.Outcome.ChosenTarget(); t != "" {
		chosen = &t
	}

	var policyVersion *int
	if d.Ladder.Reason.PolicyVersion > 0 {
		policyVersion = &d.Ladder.Reason.PolicyVersion
	}

	return []any{
		d.ID.UUID(), d.OccurredAt, string(d.Tenant),
		d.Request.RequestedModel, chosen, string(d.Outcome.Status),
		string(d.Ladder.Reason.Kind), reasonDetail(d.Ladder.Reason), policyVersion,
		d.Outcome.Usage.Input, d.Outcome.Usage.Output,
		d.Outcome.Usage.CacheRead, d.Outcome.Usage.CacheWrite, d.Outcome.Usage.Reasoning,
		costNanos, priceTable,
		d.Outcome.TTFTMs, d.Outcome.TotalMs, len(d.Outcome.Attempts),
		metadata, attempts, counterJSON, ladder,
	}, nil
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
