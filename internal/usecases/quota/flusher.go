package quota

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// flushWriteTimeout bounds one batch write, independent of any request.
const flushWriteTimeout = 10 * time.Second

type Flusher struct {
	store    ports.UsageCounterRepository
	interval time.Duration

	ch      chan domains.WindowUsage
	maxKeys int

	dropped atomic.Int64
	written atomic.Int64

	mu     sync.RWMutex
	closed bool
	done   chan struct{}
}

type counterKey struct {
	tenant uuid.UUID
	kind   domains.WindowKind
	start  int64
}

func NewFlusher(store ports.UsageCounterRepository, interval time.Duration, buffer int) *Flusher {
	if interval <= 0 {
		interval = time.Second
	}
	if buffer <= 0 {
		buffer = 8192
	}

	f := &Flusher{
		store:    store,
		interval: interval,
		ch:       make(chan domains.WindowUsage, buffer),
		maxKeys:  buffer,
		done:     make(chan struct{}),
	}
	go f.loop()
	return f
}

func (f *Flusher) Add(d domains.WindowUsage) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.closed {
		f.drop(1, "flusher closed")
		return
	}
	select {
	case f.ch <- d:
	default:
		f.drop(1, "buffer full")
	}
}

func (f *Flusher) loop() {
	defer close(f.done)

	pending := make(map[counterKey]domains.WindowUsage)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		select {
		case d, ok := <-f.ch:
			if !ok {
				f.write(pending)
				return
			}
			f.merge(pending, d)

		case <-ticker.C:
			if len(pending) > 0 {
				pending = f.write(pending)
			}
		}
	}
}

func (f *Flusher) merge(pending map[counterKey]domains.WindowUsage, d domains.WindowUsage) {
	k := counterKey{tenant: d.TenantID, kind: d.Window.Kind, start: d.Window.Start.Unix()}
	if cur, ok := pending[k]; ok {
		cur.Usage = cur.Usage.Add(d.Usage)
		pending[k] = cur
		return
	}
	if len(pending) >= f.maxKeys {
		f.drop(1, "too many unflushed counters")
		return
	}
	pending[k] = d
}

// write persists every pending delta and returns the map to accumulate
// into next. A failed write keeps its deltas for the next tick: they are
// already summed per counter, so a database outage grows the map by
// counters rather than by requests, and merge caps even that.
func (f *Flusher) write(pending map[counterKey]domains.WindowUsage) map[counterKey]domains.WindowUsage {
	if len(pending) == 0 {
		return pending
	}

	batch := make([]domains.WindowUsage, 0, len(pending))
	for _, d := range pending {
		batch = append(batch, d)
	}

	// A fixed order means two replicas flushing the same counters take
	// row locks in the same order, so they queue instead of deadlocking.
	sort.Slice(batch, func(i, j int) bool {
		a, b := batch[i], batch[j]
		if c := bytes.Compare(a.TenantID[:], b.TenantID[:]); c != 0 {
			return c < 0
		}
		if a.Window.Kind != b.Window.Kind {
			return a.Window.Kind < b.Window.Kind
		}
		return a.Window.Start.Before(b.Window.Start)
	})

	ctx, cancel := context.WithTimeout(context.Background(), flushWriteTimeout)
	defer cancel()

	if err := f.store.AddDeltas(ctx, batch); err != nil {
		slog.Error("usage counter flush failed; retrying next tick",
			"counters", len(batch), "err", err)
		return pending
	}

	f.written.Add(int64(len(batch)))
	return make(map[counterKey]domains.WindowUsage)
}

func (f *Flusher) drop(n int64, reason string) {
	total := f.dropped.Add(n)
	if total == n || total%1000 == 0 {
		slog.Error("usage counter deltas dropped; rehydration after a Redis loss will undercount",
			"reason", reason, "dropped_total", total)
	}
}

// Close stops accepting deltas and writes what is pending.
func (f *Flusher) Close(ctx context.Context) error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.ch)
	}
	f.mu.Unlock()

	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped is the number of deltas lost to backpressure.
func (f *Flusher) Dropped() int64 { return f.dropped.Load() }
