package sink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segmentExt = ".jsonl"
	deadDir    = "dead"
	lockName   = "LOCK"
)

// ErrSpoolLocked means another process already owns the spool directory.
var ErrSpoolLocked = errors.New("sink: spool directory is in use by another process")

// spool owns the segment files in one directory: an append-only active segment
// that the file writer adds to, and sealed segments that the shipper reads,
// commits and deletes.
//
// Sealing -- fsynced, closed, never written again -- is what lets the shipper
// read a file without coordinating with the writer: only the active segment ever
// changes.
type spool struct {
	dir      string
	maxBytes int64

	lock *os.File

	mu        sync.Mutex
	active    *os.File
	activeN   string // file name of the active segment, "" when none is open
	activeLen int64
	dirty     bool // written since the last fsync
	nextSeq   uint64
}

// openSpool takes the directory's lock and works out where numbering resumes.
// Anything already there is treated as sealed: nothing will append to it again,
// so a torn tail from a crash is never followed by fresh records.
func openSpool(dir string, maxBytes int64) (*spool, error) {
	if err := os.MkdirAll(filepath.Join(dir, deadDir), 0o750); err != nil {
		return nil, fmt.Errorf("sink: create spool dir %s: %w", dir, err)
	}

	lock, err := lockDir(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}

	s := &spool{dir: dir, maxBytes: maxBytes, lock: lock, nextSeq: 1}

	names, err := s.segmentNames()
	if err != nil {
		_ = unlockDir(lock)
		return nil, err
	}
	for _, n := range names {
		if seq, ok := parseSeq(n); ok && seq >= s.nextSeq {
			s.nextSeq = seq + 1
		}
	}
	return s, nil
}

// append writes lines to the active segment, opening one when needed and sealing
// it as soon as it reaches maxBytes. Lines land in the page cache; durability
// comes from sync.
//
// It returns how many lines are safely in a file, so a caller retrying after an
// error resends only the rest, and whether it sealed a segment, so the caller
// can wake the shipper.
func (s *spool) append(lines [][]byte) (written int, sealed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for written < len(lines) {
		if s.active == nil {
			if err := s.openActiveLocked(); err != nil {
				return written, sealed, err
			}
		}

		// Take as many lines as fit before the limit -- always at least one --
		// and write them in one syscall, so a crash can tear at most its tail.
		end, size := written, s.activeLen
		for end < len(lines) && (end == written || size+int64(len(lines[end])) <= s.maxBytes) {
			size += int64(len(lines[end]))
			end++
		}

		name := s.activeN
		n, err := s.active.Write(bytes.Join(lines[written:end], nil))
		s.activeLen += int64(n)
		if n > 0 {
			s.dirty = true
		}
		if err != nil {
			// A short write leaves a partial line at the tail. Seal the segment
			// so the retry starts a fresh file: the reader skips that partial
			// line as torn, and the retried records arrive whole in the next
			// segment. Whole lines resent with them are absorbed by the
			// idempotent writer.
			_ = s.sealLocked()
			return written, sealed, fmt.Errorf("sink: append to %s: %w", name, err)
		}
		written = end

		if s.activeLen >= s.maxBytes {
			if err := s.sealLocked(); err != nil {
				return written, sealed, err
			}
			sealed = true
		}
	}
	return written, sealed, nil
}

// sync makes everything appended so far durable.
func (s *spool) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active == nil || !s.dirty {
		return nil
	}
	if err := s.active.Sync(); err != nil {
		return fmt.Errorf("sink: fsync %s: %w", s.activeN, err)
	}
	s.dirty = false
	return nil
}

// seal closes the active segment so the shipper can take it. An empty one is
// left alone: sealing it would only churn files.
func (s *spool) seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active == nil || s.activeLen == 0 {
		return nil
	}
	return s.sealLocked()
}

// close seals whatever is open and releases the directory lock.
func (s *spool) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.sealLocked()
	if uerr := unlockDir(s.lock); uerr != nil && err == nil {
		err = uerr
	}
	return err
}

func (s *spool) openActiveLocked() error {
	name := fmt.Sprintf("%016d%s", s.nextSeq, segmentExt)
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("sink: create segment %s: %w", name, err)
	}
	s.nextSeq++
	s.active, s.activeN, s.activeLen, s.dirty = f, name, 0, false

	// The directory entry has to be durable too, or a crash can keep the data
	// blocks and lose the name that points at them.
	syncDir(s.dir)
	return nil
}

func (s *spool) sealLocked() error {
	if s.active == nil {
		return nil
	}
	f, name := s.active, s.activeN
	s.active, s.activeN, s.activeLen = nil, "", 0

	var err error
	if s.dirty {
		if serr := f.Sync(); serr != nil {
			err = fmt.Errorf("sink: fsync %s: %w", name, serr)
		}
	}
	s.dirty = false
	if cerr := f.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("sink: close %s: %w", name, cerr)
	}
	return err
}

// sealed lists segments that are safe to read, oldest first. Numbered segments
// sort by sequence; anything else ending in .jsonl -- a file an operator moved
// back from dead/ -- sorts after them by name.
func (s *spool) sealed() ([]string, error) {
	names, err := s.segmentNames()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	active := s.activeN
	s.mu.Unlock()

	out := names[:0]
	for _, n := range names {
		if n != active {
			out = append(out, n)
		}
	}
	return out, nil
}

// size is the total bytes held in segments, active included. Dead letters are
// not counted: they wait on a person, not on the database, and must not make the
// spool look backed up.
func (s *spool) size() (int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("sink: read spool dir: %w", err)
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), segmentExt) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // shipped and removed between ReadDir and Info
		}
		if err != nil {
			return 0, fmt.Errorf("sink: stat %s: %w", e.Name(), err)
		}
		total += info.Size()
	}
	return total, nil
}

func (s *spool) path(name string) string { return filepath.Join(s.dir, name) }

// remove deletes a segment whose every record is committed or dead-lettered. It
// is the only place data leaves the spool.
func (s *spool) remove(name string) error {
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sink: remove segment %s: %w", name, err)
	}
	syncDir(s.dir)
	return nil
}

// deadLetter is one line of a dead/<segment> file. Record holds the original
// spooled line when it was at least valid JSON, so moving it back is
// `jq -c .record`; Raw holds the bytes when it was not, because a string would
// mangle invalid UTF-8.
type deadLetter struct {
	Segment string          `json:"segment"`
	Reason  string          `json:"reason"`
	DeadAt  time.Time       `json:"dead_at"`
	Record  json.RawMessage `json:"record,omitempty"`
	Raw     []byte          `json:"raw_base64,omitempty"`
}

// deadLetter appends lines to dead/<segment> and fsyncs before returning: the
// caller deletes the segment next, and a dead letter that is not on disk is a
// record that no longer exists anywhere.
func (s *spool) deadLetter(segment string, entries []deadLetter) error {
	if len(entries) == 0 {
		return nil
	}

	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("sink: encode dead letter: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	path := filepath.Join(s.dir, deadDir, segment)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("sink: open dead letter file: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("sink: write dead letter: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sink: fsync dead letter: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sink: close dead letter: %w", err)
	}
	syncDir(filepath.Join(s.dir, deadDir))
	return nil
}

func (s *spool) segmentNames() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("sink: read spool dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), segmentExt) {
			names = append(names, e.Name())
		}
	}

	sort.Slice(names, func(i, j int) bool {
		si, iok := parseSeq(names[i])
		sj, jok := parseSeq(names[j])
		switch {
		case iok && jok:
			return si < sj
		case iok != jok:
			return iok
		default:
			return names[i] < names[j]
		}
	})
	return names, nil
}

func parseSeq(name string) (uint64, bool) {
	seq, err := strconv.ParseUint(strings.TrimSuffix(name, segmentExt), 10, 64)
	return seq, err == nil
}

// syncDir fsyncs a directory so a segment's creation or removal survives a power
// loss, not only its contents. Best effort: Windows cannot fsync a directory,
// and elsewhere a failure leaves the spool no worse than a crash a moment
// earlier -- a removed segment may reappear and be replayed.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
