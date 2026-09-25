package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// UsageCounterRepo keeps usage_counters, the durable snapshot behind the live
// quota counters. Like Auth, it reads through a pool it does not own.
type UsageCounterRepo struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

var _ ports.UsageCounterRepository = (*UsageCounterRepo)(nil)

func NewUsageCounterRepo(pool *pgxpool.Pool) *UsageCounterRepo {
	return &UsageCounterRepo{pool: pool, q: gen.New(pool)}
}

func (r *UsageCounterRepo) Get(ctx context.Context, tenantID uuid.UUID, w domains.Window) (domains.QuotaUsage, error) {
	row, err := r.q.GetUsageCounter(ctx, gen.GetUsageCounterParams{
		TenantID:    tenantID,
		WindowKind:  gen.WindowKind(w.Kind),
		WindowStart: w.Start,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domains.QuotaUsage{}, nil
	}
	if err != nil {
		return domains.QuotaUsage{}, fmt.Errorf("postgres: get usage counter: %w", err)
	}
	return domains.QuotaUsage{
		Requests: row.Requests,
		Tokens:   row.Tokens,
		Cost:     domains.USD(row.CostNanos),
	}, nil
}

// AddDeltas writes a batch in one transaction, in the order given. Callers sort
// it, so two replicas flushing overlapping counters lock rows in the same order
// and cannot deadlock each other.
func (r *UsageCounterRepo) AddDeltas(ctx context.Context, deltas []domains.WindowUsage) error {
	if len(deltas) == 0 {
		return nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := r.q.WithTx(tx)
	for _, d := range deltas {
		err := q.AddUsageCounterDelta(ctx, gen.AddUsageCounterDeltaParams{
			TenantID:    d.TenantID,
			WindowKind:  gen.WindowKind(d.Window.Kind),
			WindowStart: d.Window.Start,
			Requests:    d.Usage.Requests,
			Tokens:      d.Usage.Tokens,
			CostNanos:   int64(d.Usage.Cost),
		})
		if err != nil {
			return fmt.Errorf("postgres: add usage delta: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit %d usage deltas: %w", len(deltas), err)
	}
	return nil
}
