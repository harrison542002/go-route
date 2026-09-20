package postgresql

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/db/gen"
)

const (
	// PartitionMonthsAhead is how far ahead partitions are pre-created.
	PartitionMonthsAhead = 3

	// PartitionInterval is how often coverage is topped up.
	//
	// The interval is what makes uptime irrelevant. Creating partitions
	// only at startup pins coverage to the last restart, so a process
	// left running past its final partition starts failing every write,
	// and the buffered sink drops those records rather than retrying
	// them. Daily is far more often than monthly boundaries need, and
	// the call is idempotent, so the cost of being early is nil.
	PartitionInterval = 24 * time.Hour
)

type Partitions struct {
	q        *gen.Queries
	interval time.Duration
	now      func() time.Time

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewPartitions(pool *pgxpool.Pool) *Partitions {
	return &Partitions{
		q:        gen.New(pool),
		interval: PartitionInterval,
		now:      time.Now,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (p *Partitions) Ensure(ctx context.Context) error {
	err := p.q.EnsureRecordPartitions(ctx, gen.EnsureRecordPartitionsParams{
		FromTs: p.now(),
		Months: PartitionMonthsAhead,
	})
	if err != nil {
		return fmt.Errorf("postgres: ensure partitions: %w", err)
	}
	return nil
}

func (p *Partitions) Start() {
	go func() {
		defer close(p.done)

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-p.stop:
				return

			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				err := p.Ensure(ctx)
				cancel()
				if err != nil {
					slog.Error("could not extend table partitions", "err", err)
				}
			}
		}
	}()
}

func (p *Partitions) Stop() error {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
	return nil
}
