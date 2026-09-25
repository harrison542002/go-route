package sink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

var testTime = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func newDecision(tenant string) domains.RoutingDecision {
	return domains.RoutingDecision{
		ID:         domains.NewDecisionID(),
		OccurredAt: testTime,
		Tenant:     domains.Tenant(tenant),
		KeyID:      uuid.New(),
		Request: domains.RequestSummary{
			RequestedModel: "chat",
			Stream:         true,
			Metadata:       map[string]string{"feature": "auto-tag"},
		},
		Ladder: domains.Ladder{
			Targets: []domains.TargetRef{{Name: "openai/gpt-5-mini", Provider: "openai", UpstreamModel: "gpt-5-mini"}},
			Reason:  domains.Reason{Kind: domains.ReasonModelAlias, ModelAlias: "chat", PolicyVersion: 3},
		},
		Outcome: domains.Outcome{
			Status:   domains.StatusOK,
			Attempts: []domains.Attempt{{Target: "openai/gpt-5-mini", StartedAt: testTime, DurationMs: 412}},
			Usage:    domains.TokenUsage{Input: 80, Output: 50, CacheRead: 20, CacheWrite: 4, Reasoning: 10},
			TTFTMs:   412,
			TotalMs:  1893,
		},
		Cost: &domains.CostBreakdown{
			Actual:              domains.USD(120_500),
			PriceTableVersion:   "2026-08-01",
			Counterfactuals:     []domains.Counterfactual{{Target: "openai/gpt-5", Cost: domains.USD(602_500)}},
			UnpricedComparisons: []string{"anthropic/claude"},
		},
	}
}

// fakeWriter stands in for Postgres. It dedupes by ID the way the
// ON CONFLICT DO NOTHING inserts do, and counts deliveries so a test can
// tell a replay from a loss.
type fakeWriter struct {
	mu        sync.Mutex
	stored    map[uuid.UUID]domains.RoutingDecision
	delivered int
	calls     int

	down     bool
	failNext int
	block    bool
	reject   func(domains.RoutingDecision) bool
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{stored: make(map[uuid.UUID]domains.RoutingDecision)}
}

func (w *fakeWriter) Write(ctx context.Context, batch []domains.RoutingDecision) error {
	w.mu.Lock()
	w.calls++
	block := w.block
	w.mu.Unlock()

	if block {
		<-ctx.Done()
		return ctx.Err()
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.down {
		return errors.New("fake: connection refused")
	}
	if w.failNext > 0 {
		w.failNext--
		return errors.New("fake: connection reset")
	}
	for _, d := range batch {
		if w.reject != nil && w.reject(d) {
			return fmt.Errorf("%w: fake: unknown tenant %s", ports.ErrRejected, d.Tenant)
		}
	}
	for _, d := range batch {
		w.stored[d.ID.UUID()] = d
		w.delivered++
	}
	return nil
}

func (w *fakeWriter) set(f func(w *fakeWriter)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.stored)
}

func (w *fakeWriter) has(id domains.DecisionID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.stored[id.UUID()]
	return ok
}

func testConfig(dir string) Config {
	return Config{
		Dir:             dir,
		SyncInterval:    10 * time.Millisecond,
		FlushInterval:   20 * time.Millisecond,
		RetryBackoff:    5 * time.Millisecond,
		MaxRetryBackoff: 20 * time.Millisecond,
		WriteTimeout:    time.Second,
	}
}

func open(t *testing.T, w ports.RecordWriter, cfg Config) *Spooled {
	t.Helper()
	s, err := NewSpooled(w, cfg, nil)
	if err != nil {
		t.Fatalf("NewSpooled: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Flush(ctx)
	})
	return s
}

// crash stops the sink the way a killed process would: whatever is
// queued in memory is gone, nothing more is shipped, and the directory
// lock is released so a new instance can open it.
func (s *Spooled) crash() {
	gone, cancel := context.WithCancel(context.Background())
	cancel()

	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.pending = nil
		s.cond.Broadcast()
		s.mu.Unlock()

		s.flushCtx = gone
		close(s.stopWriter)
		s.cancelRun()
	})
	<-s.writerDone
	<-s.shipperDone
	s.releaseOnce.Do(func() { _ = s.spool.close() })
}

func flush(t *testing.T, s *Spooled, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.Flush(ctx)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func segments(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"+segmentExt))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func deadLetters(t *testing.T, dir string) []deadLetter {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, deadDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	var out []deadLetter
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			var dl deadLetter
			if err := json.Unmarshal(sc.Bytes(), &dl); err != nil {
				t.Fatalf("dead letter is not JSON: %v", err)
			}
			out = append(out, dl)
		}
	}
	return out
}

func writeSegment(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o640); err != nil {
		t.Fatal(err)
	}
}

func mustEncode(t *testing.T, d domains.RoutingDecision) []byte {
	t.Helper()
	line, err := encodeLine(d)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestRecordRoundTrip(t *testing.T) {
	exhausted := newDecision("acme")
	exhausted.Cost = nil
	exhausted.KeyID = uuid.Nil
	exhausted.Request.Metadata = nil
	exhausted.Outcome = domains.Outcome{
		Status: domains.StatusExhausted,
		Attempts: []domains.Attempt{{
			Target: "openai/gpt-5-mini", StartedAt: testTime,
			Failure: &domains.AttemptFailure{Kind: "connect", Message: "refused", Retryable: true},
		}},
	}

	for name, d := range map[string]domains.RoutingDecision{
		"priced":    newDecision("default"),
		"exhausted": exhausted,
	} {
		t.Run(name, func(t *testing.T) {
			line := mustEncode(t, d)
			if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
				t.Fatalf("encoded record must be exactly one line: %q", line)
			}

			got, err := decodeLine(bytes.TrimSuffix(line, []byte("\n")))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, d) {
				t.Errorf("round trip changed the record\n got: %+v\nwant: %+v", got, d)
			}
		})
	}
}

func TestDecodeLineRejectsDamage(t *testing.T) {
	line := bytes.TrimSuffix(mustEncode(t, newDecision("default")), []byte("\n"))

	flipped := bytes.Replace(line, []byte(`"chat"`), []byte(`"chaT"`), 1)
	future := bytes.Replace(line, []byte(`{"v":1,`), []byte(`{"v":9,`), 1)

	for name, l := range map[string][]byte{
		"truncated":       line[:len(line)/2],
		"flipped byte":    flipped,
		"unknown version": future,
		"not json":        []byte("\x00\x00garbage"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeLine(l); !errors.Is(err, errCorrupt) {
				t.Errorf("err = %v, want errCorrupt", err)
			}
		})
	}
}

func TestSpooled_ShipsEverythingOnFlush(t *testing.T) {
	dir := t.TempDir()
	w := newFakeWriter()
	cfg := testConfig(dir)
	cfg.FlushInterval = time.Hour
	s := open(t, w, cfg)

	for range 250 {
		s.Record(newDecision("default"))
	}
	if err := flush(t, s, 5*time.Second); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := w.count(); got != 250 {
		t.Errorf("stored %d records, want 250", got)
	}
	if segs := segments(t, dir); len(segs) != 0 {
		t.Errorf("shipped segments must be removed, found %v", segs)
	}
	if st := s.Stats(); st.Spooled != 250 || st.Shipped != 250 {
		t.Errorf("stats = %+v", st)
	}
}

func TestSpooled_ShipsWithinFlushInterval(t *testing.T) {
	w := newFakeWriter()
	s := open(t, w, testConfig(t.TempDir()))

	s.Record(newDecision("default"))
	s.Record(newDecision("default"))

	eventually(t, "a quiet spool to ship", func() bool { return w.count() == 2 })
}

func TestSpooled_RecordDoesNotWaitOnTheDatabase(t *testing.T) {
	w := newFakeWriter()
	w.block = true
	s := open(t, w, testConfig(t.TempDir()))

	start := time.Now()
	for range 2000 {
		s.Record(newDecision("default"))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("2000 Records took %v with the database hung", elapsed)
	}
	eventually(t, "records to reach the disk", func() bool { return s.Stats().Spooled == 2000 })
}

func TestSpooled_RotatesBySize(t *testing.T) {
	dir := t.TempDir()
	w := newFakeWriter()
	w.down = true

	cfg := testConfig(dir)
	cfg.SegmentMaxBytes = 4 << 10
	s := open(t, w, cfg)

	for range 60 {
		s.Record(newDecision("default"))
	}
	eventually(t, "records to reach the disk", func() bool { return s.Stats().Spooled == 60 })

	if n := len(segments(t, dir)); n < 3 {
		t.Errorf("found %d segments, want several at 4KiB each", n)
	}

	w.set(func(w *fakeWriter) { w.down = false })
	eventually(t, "the backlog to ship", func() bool { return w.count() == 60 })
	eventually(t, "shipped segments to be removed", func() bool { return len(segments(t, dir)) == 0 })
}

func TestSpooled_RetriesTransientFailures(t *testing.T) {
	w := newFakeWriter()
	w.failNext = 5
	s := open(t, w, testConfig(t.TempDir()))

	var ids []domains.DecisionID
	for range 100 {
		d := newDecision("default")
		ids = append(ids, d.ID)
		s.Record(d)
	}
	if err := flush(t, s, 5*time.Second); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	for _, id := range ids {
		if !w.has(id) {
			t.Fatalf("record %s was lost across write failures", id)
		}
	}
	if got := w.count(); got != 100 {
		t.Errorf("stored %d distinct records, want 100", got)
	}
	if st := s.Stats(); st.Failures == 0 {
		t.Errorf("failures = %d, want them counted", st.Failures)
	}
}

func TestSpooled_ReplaysAfterCrash(t *testing.T) {
	dir := t.TempDir()

	down := newFakeWriter()
	down.down = true
	first := open(t, down, testConfig(dir))

	var ids []domains.DecisionID
	for range 100 {
		d := newDecision("default")
		ids = append(ids, d.ID)
		first.Record(d)
	}
	eventually(t, "records to reach the disk", func() bool { return first.Stats().Spooled == 100 })
	first.crash()

	// Half of them committed before the crash, but the segment holding
	// them was never removed: the replay must re-deliver without
	// creating a second copy.
	up := newFakeWriter()
	for _, id := range ids[:50] {
		up.stored[id.UUID()] = domains.RoutingDecision{ID: id}
	}
	open(t, up, testConfig(dir))

	eventually(t, "the leftover spool to ship", func() bool {
		for _, id := range ids {
			if !up.has(id) {
				return false
			}
		}
		return true
	})
	if got := up.count(); got != 100 {
		t.Errorf("stored %d distinct records, want exactly 100", got)
	}
	eventually(t, "replayed segments to be removed", func() bool { return len(segments(t, dir)) == 0 })
}

func TestSpooled_TornTail(t *testing.T) {
	a, b, c := newDecision("default"), newDecision("default"), newDecision("default")
	whole := mustEncode(t, newDecision("default"))

	tests := []struct {
		name     string
		tail     []byte
		wantRecs int
		wantTorn int64
	}{
		{"partial final line", whole[:len(whole)/3], 3, 1},
		{"zero-filled final block", bytes.Repeat([]byte{0}, 512), 3, 0},
		{"only the newline lost", bytes.TrimSuffix(whole, []byte("\n")), 4, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var content []byte
			for _, d := range []domains.RoutingDecision{a, b, c} {
				content = append(content, mustEncode(t, d)...)
			}
			content = append(content, tt.tail...)
			writeSegment(t, dir, "0000000000000001.jsonl", content)

			w := newFakeWriter()
			s := open(t, w, testConfig(dir))

			eventually(t, "the segment to ship", func() bool { return len(segments(t, dir)) == 0 })
			if got := w.count(); got != tt.wantRecs {
				t.Errorf("stored %d records, want %d", got, tt.wantRecs)
			}
			if got := s.Stats().Torn; got != tt.wantTorn {
				t.Errorf("torn = %d, want %d", got, tt.wantTorn)
			}
			if dl := deadLetters(t, dir); len(dl) != 0 {
				t.Errorf("a torn tail is not a dead letter: %+v", dl)
			}
		})
	}
}

func TestSpooled_NumbersAfterLeftovers(t *testing.T) {
	dir := t.TempDir()
	writeSegment(t, dir, "0000000000000007.jsonl", mustEncode(t, newDecision("default")))

	w := newFakeWriter()
	w.down = true
	s := open(t, w, testConfig(dir))
	s.Record(newDecision("default"))
	eventually(t, "a new segment", func() bool { return len(segments(t, dir)) == 2 })

	if _, err := os.Stat(filepath.Join(dir, "0000000000000008.jsonl")); err != nil {
		t.Errorf("new segments must number after leftovers, never reuse them: %v", err)
	}
}

func TestSpooled_CorruptLineIsDeadLettered(t *testing.T) {
	dir := t.TempDir()
	good1, bad, good2 := newDecision("default"), newDecision("default"), newDecision("default")

	damaged := bytes.Replace(mustEncode(t, bad), []byte(`"chat"`), []byte(`"chaT"`), 1)
	content := append(append(mustEncode(t, good1), damaged...), mustEncode(t, good2)...)
	writeSegment(t, dir, "0000000000000001.jsonl", content)

	w := newFakeWriter()
	s := open(t, w, testConfig(dir))
	eventually(t, "the segment to ship", func() bool { return len(segments(t, dir)) == 0 })

	if !w.has(good1.ID) || !w.has(good2.ID) || w.count() != 2 {
		t.Errorf("the good records around a corrupt line must still ship")
	}
	dl := deadLetters(t, dir)
	if len(dl) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dl))
	}
	if !strings.Contains(dl[0].Reason, "checksum") {
		t.Errorf("reason = %q, want it to name the checksum", dl[0].Reason)
	}
	if !bytes.Equal(dl[0].Record, bytes.TrimSuffix(damaged, []byte("\n"))) {
		t.Errorf("the dead letter must preserve the line byte for byte")
	}
	if got := s.Stats().DeadLettered; got != 1 {
		t.Errorf("dead-lettered = %d, want 1", got)
	}
}

func TestSpooled_PoisonRecordIsDeadLettered(t *testing.T) {
	dir := t.TempDir()
	w := newFakeWriter()
	w.reject = func(d domains.RoutingDecision) bool { return d.Tenant == "ghost" }
	cfg := testConfig(dir)
	cfg.FlushInterval = time.Hour
	s := open(t, w, cfg)

	var good []domains.DecisionID
	for i := range 21 {
		tenant := "default"
		if i == 13 {
			tenant = "ghost"
		}
		d := newDecision(tenant)
		if tenant == "default" {
			good = append(good, d.ID)
		}
		s.Record(d)
	}
	if err := flush(t, s, 5*time.Second); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	for _, id := range good {
		if !w.has(id) {
			t.Fatalf("good record %s did not ship alongside the poison one", id)
		}
	}
	if w.count() != 20 {
		t.Errorf("stored %d, want 20", w.count())
	}
	if segs := segments(t, dir); len(segs) != 0 {
		t.Errorf("a poison record must not wedge its segment: %v", segs)
	}

	dl := deadLetters(t, dir)
	if len(dl) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dl))
	}
	if !strings.Contains(dl[0].Reason, "unknown tenant") {
		t.Errorf("reason = %q, want the writer's error", dl[0].Reason)
	}
	d, err := decodeLine(dl[0].Record)
	if err != nil {
		t.Fatalf("a dead letter must hold a record that can be moved back: %v", err)
	}
	if d.Tenant != "ghost" {
		t.Errorf("dead-lettered the wrong record: tenant %q", d.Tenant)
	}
}

func TestSpooled_FlushLeavesUnshippedRecordsOnDisk(t *testing.T) {
	dir := t.TempDir()
	down := newFakeWriter()
	down.down = true
	s := open(t, down, testConfig(dir))

	for range 20 {
		s.Record(newDecision("default"))
	}
	if err := flush(t, s, 200*time.Millisecond); err != nil {
		t.Fatalf("records that reached the disk are not lost; Flush must not report failure: %v", err)
	}
	if len(segments(t, dir)) == 0 {
		t.Fatal("unshipped records must stay on disk")
	}

	up := newFakeWriter()
	open(t, up, testConfig(dir))
	eventually(t, "the next start to ship them", func() bool { return up.count() == 20 })
}

func TestSpooled_RecordAfterFlushIsKept(t *testing.T) {
	dir := t.TempDir()
	s := open(t, newFakeWriter(), testConfig(dir))
	if err := flush(t, s, time.Second); err != nil {
		t.Fatal(err)
	}

	late := newDecision("default")
	s.Record(late)

	up := newFakeWriter()
	open(t, up, testConfig(dir))
	eventually(t, "the late record to ship", func() bool { return up.has(late.ID) })
}

func TestSpooled_FlushTwice(t *testing.T) {
	s := open(t, newFakeWriter(), testConfig(t.TempDir()))
	s.Record(newDecision("default"))
	if err := flush(t, s, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := flush(t, s, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSpooled_SyncAlwaysIsOnDiskWhenRecordReturns(t *testing.T) {
	dir := t.TempDir()
	w := newFakeWriter()
	w.down = true
	cfg := testConfig(dir)
	cfg.Sync = SyncAlways
	s := open(t, w, cfg)

	d := newDecision("default")
	s.Record(d)

	var found bool
	for _, seg := range segments(t, dir) {
		data, err := os.ReadFile(seg)
		if err != nil {
			t.Fatal(err)
		}
		found = found || bytes.Contains(data, []byte(d.ID.UUID().String()))
	}
	if !found {
		t.Error("with sync: always, Record must not return before its record is in the file")
	}
}

func TestSpooled_DirectoryIsExclusive(t *testing.T) {
	dir := t.TempDir()
	open(t, newFakeWriter(), testConfig(dir))

	_, err := NewSpooled(newFakeWriter(), testConfig(dir), nil)
	if !errors.Is(err, ErrSpoolLocked) {
		t.Errorf("second open err = %v, want ErrSpoolLocked", err)
	}
}

func TestSpooled_RejectsUnknownSyncMode(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Sync = "sometimes"
	if _, err := NewSpooled(newFakeWriter(), cfg, nil); err == nil {
		t.Error("an unknown sync mode must fail construction")
	}
}
