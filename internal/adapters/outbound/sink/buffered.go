// Package sink buffers decision records between the request hot path and
// whatever persist them
package sink

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type Config struct {
	BufferSize    int
	BatchSize     int
	FlushInterval time.Duration
	WriteTimeout  time.Duration
}

// Buffered decouples Record from persistence
type Buffered struct {
	ch     chan domains.RoutingDecision
	writer Writer
	cfg    Config

	dropped atomic.Int64
	failed  atomic.Int64
	written atomic.Int64

	done     chan struct{}
	stopOnce sync.Once
	closed   atomic.Bool
}

var _ ports.DecisionSink = (*Buffered)(nil)

func NewBuffered(w Writer, cfg Config) *Buffered {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 4096
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}

	b := &Buffered{
		ch:     make(chan domains.RoutingDecision, cfg.BufferSize),
		writer: w,
		cfg:    cfg,
		done:   make(chan struct{}),
	}
	go b.loop()
	return b
}

// Record enqueues a decision. If the buffer is full it drops and counts.
func (b *Buffered) Record(d domains.RoutingDecision) {
	if b.closed.Load() {
		b.dropped.Add(1)
		return
	}

	select {
	case b.ch <- d:
	default:
		n := b.dropped.Add(1)

		// Log the first drop and then sparsely: under sustained overload
		// this fires per request, and flooding logs makes the incident
		// harder to diagnose, not easier.
		if n == 1 || n%1000 == 0 {
			slog.Error("decision sink buffer full; records lost",
				"dropped_total", n, "buffer_size", b.cfg.BufferSize)
		}
	}
}

func (b *Buffered) loop() {
	defer close(b.done)

	batch := make([]domains.RoutingDecision, 0, b.cfg.BatchSize)
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case d, ok := <-b.ch:
			if !ok {
				b.write(batch) // drain on close
				return
			}
			batch = append(batch, d)
			if len(batch) >= b.cfg.BatchSize {
				b.write(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				b.write(batch)
				batch = batch[:0]
			}
		}
	}
}

// write persists a batch with its own timeout, independent of any
// request context: records must survive the request they describe.
func (b *Buffered) write(batch []domains.RoutingDecision) {
	if len(batch) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.WriteTimeout)
	defer cancel()

	if err := b.writer.Write(ctx, batch); err != nil {
		b.failed.Add(int64(len(batch)))
		slog.Error("decision batch write failed", "count", len(batch), "err", err)
		return
	}
	b.written.Add(int64(len(batch)))
}

// Flush stops accepting records and waits for the writer to drain.
func (b *Buffered) Flush(ctx context.Context) error {
	b.stopOnce.Do(func() {
		b.closed.Store(true)
		close(b.ch)
	})

	select {
	case <-b.done:
		if d := b.dropped.Load(); d > 0 {
			slog.Warn("decision records were dropped during this run", "count", d)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats reports counters for metrics and tests.
func (b *Buffered) Stats() (written, dropped, failed int64) {
	return b.written.Load(), b.dropped.Load(), b.failed.Load()
}
