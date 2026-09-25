package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// scriptedStore is a usage counter repository whose writes can be made to fail
// and whose flushes can be observed.
type scriptedStore struct {
	mu      sync.Mutex
	fail    bool
	batches [][]domains.WindowUsage
	wrote   chan struct{}
}

var _ ports.UsageCounterRepository = (*scriptedStore)(nil)

func newScriptedStore() *scriptedStore {
	return &scriptedStore{wrote: make(chan struct{}, 100)}
}

func (s *scriptedStore) Get(context.Context, uuid.UUID, domains.Window) (domains.QuotaUsage, error) {
	return domains.QuotaUsage{}, nil
}

func (s *scriptedStore) AddDeltas(_ context.Context, batch []domains.WindowUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { s.wrote <- struct{}{} }()
	if s.fail {
		return errors.New("postgres down")
	}
	s.batches = append(s.batches, append([]domains.WindowUsage(nil), batch...))
	return nil
}

func (s *scriptedStore) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *scriptedStore) flushed() [][]domains.WindowUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]domains.WindowUsage(nil), s.batches...)
}

func delta(tenant uuid.UUID, kind domains.WindowKind, tokens int64) domains.WindowUsage {
	w, _ := domains.ClockWindow(kind, testNow)
	return domains.WindowUsage{TenantID: tenant, Window: w, Usage: domains.QuotaUsage{Requests: 1, Tokens: tokens}}
}

// Deltas for one counter are summed before they reach Postgres: one row
// write per counter per interval, however busy the tenant.
func TestFlusher_SumsPerCounterAndSortsTheBatch(t *testing.T) {
	store := newScriptedStore()
	f := NewFlusher(store, time.Hour, 100) // only Close flushes

	a := uuid.MustParse("00000000-0000-7000-8000-00000000000a")
	b := uuid.MustParse("00000000-0000-7000-8000-00000000000b")
	f.Add(delta(b, domains.WindowMinute, 5))
	f.Add(delta(a, domains.WindowMinute, 1))
	f.Add(delta(a, domains.WindowMinute, 2))
	f.Add(delta(a, domains.WindowDay, 3))

	if err := f.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	batches := store.flushed()
	if len(batches) != 1 {
		t.Fatalf("got %d batches, want 1", len(batches))
	}
	got := batches[0]
	if len(got) != 3 {
		t.Fatalf("got %d deltas, want 3 counters", len(got))
	}

	// Sorted by tenant, then kind: the order two replicas take row locks
	// in, which is what keeps them from deadlocking.
	if got[0].TenantID != a || got[0].Window.Kind != domains.WindowDay ||
		got[1].TenantID != a || got[1].Window.Kind != domains.WindowMinute ||
		got[2].TenantID != b {
		t.Errorf("order = %+v", got)
	}
	if got[1].Usage != (domains.QuotaUsage{Requests: 2, Tokens: 3}) {
		t.Errorf("summed = %+v, want requests 2 tokens 3", got[1].Usage)
	}
}

// A failed write is retried, not lost: the deltas are already summed, so
// holding them costs a row per counter, not per request.
func TestFlusher_RetriesAFailedWrite(t *testing.T) {
	store := newScriptedStore()
	store.setFail(true)
	f := NewFlusher(store, 10*time.Millisecond, 100)

	f.Add(delta(tenantID, domains.WindowMinute, 7))

	select {
	case <-store.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("no flush attempted")
	}
	store.setFail(false)

	if err := f.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches := store.flushed()
	if len(batches) == 0 || batches[len(batches)-1][0].Usage.Tokens != 7 {
		t.Errorf("batches = %+v, want the failed delta written on a later attempt", batches)
	}
}

// Under backpressure a delta is dropped and counted rather than blocking
// the request that produced it.
func TestFlusher_DropsRatherThanBlocks(t *testing.T) {
	store := newScriptedStore()
	f := NewFlusher(store, time.Hour, 1)

	// The loop drains the one-slot channel into a one-key map; distinct
	// counters beyond that have nowhere to go.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 50 {
			f.Add(delta(uuid.New(), domains.WindowMinute, int64(i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Add blocked")
	}
	if err := f.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Dropped() == 0 {
		t.Error("nothing dropped, yet only one counter fits")
	}

	// A delta arriving after shutdown is dropped too, not a panic.
	f.Add(delta(tenantID, domains.WindowMinute, 1))
}
