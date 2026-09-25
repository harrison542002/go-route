package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// SyncMode decides when spooled records are fsynced.
type SyncMode string

const (
	// SyncInterval fsyncs on a timer, so a power loss or kernel crash can lose
	// up to SyncInterval of records. A process crash loses only what the file
	// writer had not yet handed to the kernel.
	SyncInterval SyncMode = "interval"

	// SyncAlways makes Record wait for the fsync covering its record. Records
	// that arrive together share one fsync.
	SyncAlways SyncMode = "always"
)

// Defaults for a zero Config
const (
	DefaultDir             = "data/spool"
	DefaultSegmentMaxBytes = 64 << 20
	DefaultSyncInterval    = 200 * time.Millisecond
	DefaultMaxSpoolBytes   = 1 << 30
	DefaultBufferSize      = 4096
	DefaultBatchSize       = 100
	DefaultFlushInterval   = time.Second
)

type Config struct {
	// Dir holds the segment files. It must be persistent and belong to one
	// process: a second process pointed at it fails to start.
	Dir string

	// SegmentMaxBytes is how large a segment grows before it is sealed. A
	// segment overshoots it by at most one record.
	SegmentMaxBytes int64

	Sync         SyncMode
	SyncInterval time.Duration

	// MaxSpoolBytes is an alarm, not a cap: past it the sink logs and keeps
	// writing. It never drops a record to get back under.
	MaxSpoolBytes int64

	// BufferSize is how many encoded records may wait in memory for the file
	// writer. It only fills if the disk stops accepting writes, and then
	// Record blocks rather than drops.
	BufferSize int

	// BatchSize is rows per shipping transaction; FlushInterval is how often a
	// partly filled segment is sealed and shipped.
	BatchSize     int
	FlushInterval time.Duration

	// WriteTimeout bounds one batch write; the backoffs bound the wait between
	// retries of a batch that failed for a transient reason.
	WriteTimeout    time.Duration
	RetryBackoff    time.Duration
	MaxRetryBackoff time.Duration
}

func (c Config) withDefaults() Config {
	if c.Dir == "" {
		c.Dir = DefaultDir
	}
	if c.SegmentMaxBytes <= 0 {
		c.SegmentMaxBytes = DefaultSegmentMaxBytes
	}
	if c.Sync == "" {
		c.Sync = SyncInterval
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = DefaultSyncInterval
	}
	if c.MaxSpoolBytes <= 0 {
		c.MaxSpoolBytes = DefaultMaxSpoolBytes
	}
	if c.BufferSize <= 0 {
		c.BufferSize = DefaultBufferSize
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 250 * time.Millisecond
	}
	if c.MaxRetryBackoff <= 0 {
		c.MaxRetryBackoff = 30 * time.Second
	}
	return c
}

// Stats is a point-in-time view of the sink, for metrics and tests.
type Stats struct {
	Spooled      int64 // records appended to a segment file
	Shipped      int64 // records the writer committed, replays included
	DeadLettered int64 // records moved to dead/ instead of the database
	Torn         int64 // torn final lines skipped after a crash
	Failures     int64 // batch writes that failed and will be retried
	Unencodable  int64 // records that could not be serialised, and so were lost
	Pending      int   // records in memory, not yet in a file
	SpoolBytes   int64 // bytes in segment files as of the last shipper pass
}

// Spooled is the decision sink: Record appends to the spool, and a shipper
// drains the spool into a ports.RecordWriter.
//
// Record hands an encoded line to a single file-writer goroutine through an
// in-memory queue rather than writing under a lock itself, so a request never
// waits on a write syscall a busy disk has stalled.
type Spooled struct {
	cfg    Config
	spool  *spool
	writer ports.RecordWriter
	now    func() time.Time

	mu       sync.Mutex
	cond     *sync.Cond // queue has space, or durable advanced, or the writer stopped
	pending  [][]byte
	enqueued uint64 // sequence of the last record queued
	durable  uint64 // sequence of the last record fsynced
	closed   bool   // Flush has begun; Record writes synchronously
	stopped  bool   // the file writer has exited

	wakeWriter  chan struct{}
	wakeShipper chan struct{}
	stopWriter  chan struct{}
	writerDone  chan struct{}
	shipperDone chan struct{}

	runCtx    context.Context
	cancelRun context.CancelFunc
	flushCtx  context.Context // set before stopWriter closes

	stopOnce    sync.Once
	releaseOnce sync.Once
	lateMu      sync.Mutex

	spooled, shipped, dead, torn, failures, unencodable atomic.Int64
	spoolBytes                                          atomic.Int64

	// Shipper-goroutine state.
	progress    map[string]int
	failingFrom time.Time
	lastFailLog time.Time
	overLimit   bool
	lastSizeLog time.Time
}

var _ ports.DecisionSink = (*Spooled)(nil)

// NewSpooled opens the spool directory, takes its lock, and starts the file
// writer and the shipper. Segments left by a previous run are shipped first.
func NewSpooled(w ports.RecordWriter, cfg Config, now func() time.Time) (*Spooled, error) {
	cfg = cfg.withDefaults()
	if cfg.Sync != SyncInterval && cfg.Sync != SyncAlways {
		return nil, fmt.Errorf("sink: unknown sync mode %q", cfg.Sync)
	}
	if now == nil {
		now = time.Now
	}

	sp, err := openSpool(cfg.Dir, cfg.SegmentMaxBytes)
	if err != nil {
		return nil, err
	}

	leftover, _ := sp.sealed()
	if len(leftover) > 0 {
		size, _ := sp.size()
		slog.Info("resuming decision records spooled by a previous run",
			"dir", cfg.Dir, "segments", len(leftover), "bytes", size)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	s := &Spooled{
		cfg:         cfg,
		spool:       sp,
		writer:      w,
		now:         now,
		wakeWriter:  make(chan struct{}, 1),
		wakeShipper: make(chan struct{}, 1),
		stopWriter:  make(chan struct{}),
		writerDone:  make(chan struct{}),
		shipperDone: make(chan struct{}),
		runCtx:      runCtx,
		cancelRun:   cancel,
		progress:    make(map[string]int),
	}
	s.cond = sync.NewCond(&s.mu)

	go s.writeLoop()
	go s.shipLoop()
	return s, nil
}

// Record queues a decision for the spool. It never fails and never waits on the
// database. It waits on the disk with SyncAlways, and when BufferSize records
// are already queued because the disk has stopped accepting writes: blocking
// there is deliberate, since the alternative is dropping a ledger row.
func (s *Spooled) Record(d domains.RoutingDecision) {
	line, err := encodeLine(d)
	if err != nil {
		n := s.unencodable.Add(1)
		slog.Error("decision record could not be encoded and is lost",
			"decision_id", d.ID.String(), "tenant", string(d.Tenant),
			"err", err, "lost_total", n)
		return
	}

	s.mu.Lock()
	for !s.closed && len(s.pending) >= s.cfg.BufferSize {
		s.cond.Wait()
	}
	if s.closed {
		s.mu.Unlock()
		s.recordLate(line)
		return
	}

	s.pending = append(s.pending, line)
	s.enqueued++
	seq := s.enqueued
	nudge(s.wakeWriter)

	if s.cfg.Sync == SyncAlways {
		for s.durable < seq && !s.stopped {
			s.cond.Wait()
		}
	}
	s.mu.Unlock()
}

// recordLate handles a record that arrives after Flush has begun. The file
// writer is gone, so the record is written and sealed here, into a segment of
// its own that the next start ships.
func (s *Spooled) recordLate(line []byte) {
	s.lateMu.Lock()
	defer s.lateMu.Unlock()

	_, _, err := s.spool.append([][]byte{line})
	if err == nil {
		err = s.spool.seal()
	}
	if err != nil {
		slog.Error("decision record arrived during shutdown and could not be spooled; it is lost",
			"err", err)
		return
	}
	s.spooled.Add(1)
}

// Flush stops accepting records into the queue, writes and fsyncs what is
// queued, and ships as much of the spool as ctx allows. Whatever does not ship
// stays on disk for the next start. Flush reports an error only if queued
// records could not reach the disk -- those are the only ones this process can
// lose.
func (s *Spooled) Flush(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cond.Broadcast()
		s.mu.Unlock()

		s.flushCtx = ctx
		close(s.stopWriter)
		s.cancelRun()
	})

	select {
	case <-s.writerDone:
	case <-ctx.Done():
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		return fmt.Errorf("sink: flush: %d records not written to the spool: %w", n, ctx.Err())
	}

	// Waiting for the shipper matters: the caller closes the database pool next.
	<-s.shipperDone

	s.mu.Lock()
	lost := len(s.pending)
	s.mu.Unlock()

	s.releaseOnce.Do(func() {
		if size, err := s.spool.size(); err == nil && size > 0 {
			slog.Warn("decision records remain in the spool; they will be shipped on next start",
				"dir", s.cfg.Dir, "bytes", size)
		}
		if err := s.spool.close(); err != nil {
			slog.Error("closing the spool failed", "err", err)
		}
	})

	if lost > 0 {
		return fmt.Errorf("sink: flush: %d records could not be written to the spool", lost)
	}
	return nil
}

// Stats reports counters for metrics and tests.
func (s *Spooled) Stats() Stats {
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()

	return Stats{
		Spooled:      s.spooled.Load(),
		Shipped:      s.shipped.Load(),
		DeadLettered: s.dead.Load(),
		Torn:         s.torn.Load(),
		Failures:     s.failures.Load(),
		Unencodable:  s.unencodable.Load(),
		Pending:      pending,
		SpoolBytes:   s.spoolBytes.Load(),
	}
}

// writeLoop is the only goroutine that appends to the active segment
// while the sink is running.
func (s *Spooled) writeLoop() {
	defer close(s.writerDone)

	var tick <-chan time.Time
	if s.cfg.Sync == SyncInterval {
		t := time.NewTicker(s.cfg.SyncInterval)
		defer t.Stop()
		tick = t.C
	}

	backoff := s.cfg.RetryBackoff
	var failLog throttle

	for {
		select {
		case <-s.stopWriter:
			s.drainFinal()
			return
		case <-s.wakeWriter:
		case <-tick:
		}

		err := s.drain()
		if err == nil && tick != nil {
			err = s.syncNow()
		}
		if err == nil {
			backoff = s.cfg.RetryBackoff
			continue
		}

		if failLog.allow(s.now()) {
			s.mu.Lock()
			n := len(s.pending)
			s.mu.Unlock()
			slog.Error("decision spool write failed; records are held in memory and retried",
				"dir", s.cfg.Dir, "pending", n, "err", err)
		}
		select {
		case <-time.After(backoff):
			nudge(s.wakeWriter)
		case <-s.stopWriter:
			s.drainFinal()
			return
		}
		backoff = min(backoff*2, s.cfg.MaxRetryBackoff)
	}
}

// drain moves everything queued into the active segment. On failure the records
// go back to the front of the queue, in order, to be retried.
func (s *Spooled) drain() error {
	s.mu.Lock()
	batch, hi := s.pending, s.enqueued
	s.pending = nil
	s.cond.Broadcast() // space in the queue
	s.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	written, sealed, err := s.spool.append(batch)
	s.spooled.Add(int64(written))
	if sealed {
		nudge(s.wakeShipper)
	}
	if err != nil {
		s.mu.Lock()
		s.pending = append(batch[written:], s.pending...)
		s.mu.Unlock()
		return err
	}

	if s.cfg.Sync == SyncAlways {
		if err := s.spool.sync(); err != nil {
			// The records are in the file but not known to be durable, so the
			// callers waiting on the fsync keep waiting while it is retried.
			return err
		}
		s.setDurable(hi)
	}
	return nil
}

func (s *Spooled) syncNow() error {
	s.mu.Lock()
	hi := s.enqueued - uint64(len(s.pending))
	s.mu.Unlock()

	if err := s.spool.sync(); err != nil {
		return err
	}
	s.setDurable(hi)
	return nil
}

func (s *Spooled) setDurable(seq uint64) {
	s.mu.Lock()
	if seq > s.durable {
		s.durable = seq
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// drainFinal writes out the queue at shutdown, retrying until flushCtx expires,
// then seals the active segment so the final shipper pass can take it.
func (s *Spooled) drainFinal() {
	defer func() {
		s.mu.Lock()
		s.stopped = true
		s.cond.Broadcast()
		s.mu.Unlock()
	}()

	backoff := s.cfg.RetryBackoff
	for {
		err := s.drain()
		if err == nil {
			s.mu.Lock()
			empty := len(s.pending) == 0
			s.mu.Unlock()
			if empty {
				break
			}
			continue
		}

		select {
		case <-time.After(backoff):
		case <-s.flushCtx.Done():
			s.mu.Lock()
			n := len(s.pending)
			s.mu.Unlock()
			slog.Error("decision records could not be written to the spool before shutdown; they are lost",
				"count", n, "err", err)
			return
		}
		backoff = min(backoff*2, s.cfg.MaxRetryBackoff)
	}

	if err := s.spool.seal(); err != nil {
		slog.Error("sealing the spool at shutdown failed", "err", err)
		return
	}
	s.setDurable(s.enqueuedNow())
}

func (s *Spooled) enqueuedNow() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueued
}

// shipLoop moves sealed segments into the writer, oldest first.
func (s *Spooled) shipLoop() {
	defer close(s.shipperDone)

	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	s.shipAll(s.runCtx)

	for {
		select {
		case <-s.runCtx.Done():
			s.shipFinal()
			return
		case <-ticker.C:
		case <-s.wakeShipper:
		}
		s.shipAll(s.runCtx)
	}
}

// shipFinal is the best-effort pass Flush asks for: once the file writer has
// emptied the queue into the spool, ship whatever ctx allows.
func (s *Spooled) shipFinal() {
	ctx := s.flushCtx
	select {
	case <-s.writerDone:
	case <-ctx.Done():
		return
	}
	s.shipAll(ctx)
}

// shipAll ships every sealed segment. The active segment is sealed only when
// nothing else is waiting, so an outage lets it grow to full size instead of
// leaving a file per tick behind.
func (s *Spooled) shipAll(ctx context.Context) {
	defer s.checkSize()

	names, err := s.spool.sealed()
	if err == nil && len(names) == 0 {
		if err = s.spool.seal(); err == nil {
			names, err = s.spool.sealed()
		}
	}
	if err != nil {
		slog.Error("listing the decision spool failed", "dir", s.cfg.Dir, "err", err)
		return
	}

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		if err := s.shipSegment(ctx, name); err != nil {
			// Stop at the first segment that cannot finish; the next pass
			// resumes it. Only cancellation and local disk errors get here --
			// database errors are retried inside deliver.
			if ctx.Err() == nil {
				slog.Error("shipping a spool segment failed; will retry",
					"segment", name, "err", err)
			}
			return
		}
	}
}

// spooledLine is one decoded record and the bytes it came from, kept so a dead
// letter preserves exactly what was spooled.
type spooledLine struct {
	raw []byte
	d   domains.RoutingDecision
}

// shipSegment delivers one sealed segment and removes it once every record in
// it is committed or dead-lettered. At-least-once: a crash before the remove
// replays the whole segment, which the writer's idempotency absorbs.
func (s *Spooled) shipSegment(ctx context.Context, name string) error {
	recs, bad, err := s.readSegment(name)
	if err != nil {
		return err
	}

	// Only on the first attempt at this segment in this process, so a retried
	// segment does not file its corrupt lines twice.
	done, seen := s.progress[name]
	if !seen {
		if err := s.spool.deadLetter(name, bad); err != nil {
			return err
		}
		s.dead.Add(int64(len(bad)))
		s.progress[name] = 0
	}

	for i := done; i < len(recs); i += s.cfg.BatchSize {
		end := min(i+s.cfg.BatchSize, len(recs))
		if err := s.deliver(ctx, name, recs[i:end]); err != nil {
			return err
		}
		s.progress[name] = end
	}

	if err := s.spool.remove(name); err != nil {
		return err
	}
	delete(s.progress, name)
	return nil
}

// deliver writes a batch, retrying transient failures with backoff until it
// commits or ctx ends. A rejected batch is split in half and each half delivered
// on its own, so one poison record costs a few extra round trips instead of
// wedging the spool; a rejected single record is dead-lettered.
func (s *Spooled) deliver(ctx context.Context, segment string, batch []spooledLine) error {
	backoff := s.cfg.RetryBackoff

	for {
		err := s.write(ctx, batch)
		if err == nil {
			s.shipped.Add(int64(len(batch)))
			s.recovered()
			return nil
		}

		if errors.Is(err, ports.ErrRejected) {
			if len(batch) == 1 {
				return s.bury(segment, batch[0], err)
			}
			mid := len(batch) / 2
			if err := s.deliver(ctx, segment, batch[:mid]); err != nil {
				return err
			}
			return s.deliver(ctx, segment, batch[mid:])
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.failed(len(batch), err)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, s.cfg.MaxRetryBackoff)
	}
}

func (s *Spooled) write(ctx context.Context, batch []spooledLine) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()

	ds := make([]domains.RoutingDecision, len(batch))
	for i, l := range batch {
		ds[i] = l.d
	}
	return s.writer.Write(ctx, ds)
}

// bury dead-letters one record the writer will never accept.
func (s *Spooled) bury(segment string, l spooledLine, reason error) error {
	err := s.spool.deadLetter(segment, []deadLetter{{
		Segment: segment,
		Reason:  reason.Error(),
		DeadAt:  s.now().UTC(),
		Record:  json.RawMessage(l.raw),
	}})
	if err != nil {
		return err
	}
	n := s.dead.Add(1)
	slog.Error("decision record rejected by the database; moved to the dead-letter directory",
		"decision_id", l.d.ID.String(), "tenant", string(l.d.Tenant),
		"segment", segment, "reason", reason, "dead_total", n)
	return nil
}

// readSegment decodes a sealed segment. A final line without its newline is the
// tail a crash tore: kept if it still decodes and passes its checksum, skipped
// and counted if not -- it was never durable, so skipping it loses nothing that
// was promised. Any other line that does not decode becomes a dead letter,
// preserved byte for byte.
func (s *Spooled) readSegment(name string) ([]spooledLine, []deadLetter, error) {
	data, err := os.ReadFile(s.spool.path(name))
	if err != nil {
		return nil, nil, fmt.Errorf("sink: read segment %s: %w", name, err)
	}

	lines := bytes.Split(data, []byte{'\n'})
	tail := bytes.Trim(lines[len(lines)-1], "\x00")
	lines = lines[:len(lines)-1]

	var (
		recs []spooledLine
		bad  []deadLetter
	)
	for _, line := range lines {
		// A crash can leave zero-filled blocks where unsynced data should have
		// been; they carry nothing and are not a record.
		line = bytes.Trim(line, "\x00")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		d, err := decodeLine(line)
		if err != nil {
			dl := deadLetter{Segment: name, Reason: err.Error(), DeadAt: s.now().UTC()}
			if json.Valid(line) {
				dl.Record = json.RawMessage(line)
			} else {
				dl.Raw = line
			}
			bad = append(bad, dl)
			continue
		}
		recs = append(recs, spooledLine{raw: line, d: d})
	}

	if len(tail) > 0 {
		if d, err := decodeLine(tail); err == nil {
			recs = append(recs, spooledLine{raw: tail, d: d})
		} else if _, seen := s.progress[name]; !seen {
			s.torn.Add(1)
			slog.Warn("spool segment ends in a torn record from a crash; skipping it",
				"segment", name, "bytes", len(tail))
		}
	}
	return recs, bad, nil
}

// failed logs a transient write failure: the first of an outage at once, then at
// most once a minute.
func (s *Spooled) failed(n int, err error) {
	s.failures.Add(1)
	now := s.now()
	if s.failingFrom.IsZero() {
		s.failingFrom = now
	}
	if now.Sub(s.lastFailLog) < time.Minute {
		return
	}
	s.lastFailLog = now
	slog.Error("decision batch write failed; records are safe in the spool and will be retried",
		"count", n, "failing_for", now.Sub(s.failingFrom).Round(time.Second), "err", err)
}

func (s *Spooled) recovered() {
	if s.failingFrom.IsZero() {
		return
	}
	slog.Info("decision batch writes recovered",
		"failing_for", s.now().Sub(s.failingFrom).Round(time.Second))
	s.failingFrom, s.lastFailLog = time.Time{}, time.Time{}
}

// checkSize raises the MaxSpoolBytes alarm. It logs rather than acts: see
// Config.MaxSpoolBytes.
func (s *Spooled) checkSize() {
	size, err := s.spool.size()
	if err != nil {
		return
	}
	s.spoolBytes.Store(size)

	now := s.now()
	switch {
	case size > s.cfg.MaxSpoolBytes:
		if !s.overLimit || now.Sub(s.lastSizeLog) >= time.Minute {
			slog.Error("decision spool is over max_spool_bytes; the database is not keeping up. "+
				"Records are still being kept, not dropped -- watch free disk space",
				"dir", s.cfg.Dir, "bytes", size, "max_spool_bytes", s.cfg.MaxSpoolBytes)
			s.lastSizeLog = now
		}
		s.overLimit = true
	case s.overLimit:
		slog.Info("decision spool is back under max_spool_bytes", "bytes", size)
		s.overLimit = false
	}
}

// nudge wakes a goroutine without blocking; one pending wake is enough.
func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// throttle allows an event at most once a minute.
type throttle struct {
	last time.Time
}

func (t *throttle) allow(now time.Time) bool {
	if !t.last.IsZero() && now.Sub(t.last) < time.Minute {
		return false
	}
	t.last = now
	return true
}
