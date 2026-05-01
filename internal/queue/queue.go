// Package queue implements ScoutTrace's local durable event queue.
//
// The MVP build ships a file-backed queue: one record file per event with
// metadata embedded as JSON header + gzipped payload body. Status is encoded
// in the directory the file lives in (pending / inflight / dead) so that
// transitions are atomic POSIX renames.
//
// The package presents the same surface (Open, Enqueue, ClaimPending, Ack,
// Retry, MarkDead, Stats, Prune, RecoverInflight) as the §12 SQLite design
// so a drop-in modernc.org/sqlite implementation can replace it later.
package queue

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the lifecycle state of a queued event.
type Status string

const (
	StatusPending  Status = "pending"
	StatusInflight Status = "inflight"
	StatusDead     Status = "dead"
)

// Record is a queued event with its metadata.
type Record struct {
	ID          string `json:"id"`
	Destination string `json:"destination"`
	EnqueuedAt  int64  `json:"enqueued_at"`  // unix ms
	NextAttempt int64  `json:"next_attempt"` // unix ms
	Attempts    int    `json:"attempts"`
	Status      Status `json:"status"`
	LastError   string `json:"last_error,omitempty"`
	Payload     []byte `json:"-"` // gzipped JSON envelope
}

// Stats is a counters snapshot.
type Stats struct {
	Pending     int
	Inflight    int
	Dead        int
	Evicted     uint64
	WriteErrors uint64
	Recovered   uint64
}

// Errors.
var (
	ErrPayloadTooLarge = errors.New("queue: payload exceeds max row bytes")
	ErrNotFound        = errors.New("queue: event not found")
)

// Options configures Queue behavior.
type Options struct {
	Dir         string // base directory; e.g. ~/.scouttrace/queue
	MaxRowBytes int    // per-event payload cap (default 2 MiB)
	MaxBytes    int64  // total queue cap (0 = unlimited)
	DropPolicy  string // "oldest" (default) | "newest" | "block"
}

// Queue is a thread-safe local queue.
type Queue struct {
	opts      Options
	mu        sync.Mutex
	stats     Stats
	pendingD  string
	inflightD string
	deadD     string
	overflow  string
	now       func() time.Time
}

// Open creates the directory tree if needed and recovers any pre-existing
// inflight events back to pending (the AC-Q1 mechanism).
func Open(opts Options) (*Queue, error) {
	if opts.Dir == "" {
		return nil, errors.New("queue: Dir required")
	}
	if opts.MaxRowBytes == 0 {
		opts.MaxRowBytes = 2 * 1024 * 1024
	}
	if opts.DropPolicy == "" {
		opts.DropPolicy = "oldest"
	}
	q := &Queue{
		opts:      opts,
		pendingD:  filepath.Join(opts.Dir, "pending"),
		inflightD: filepath.Join(opts.Dir, "inflight"),
		deadD:     filepath.Join(opts.Dir, "dead"),
		overflow:  filepath.Join(opts.Dir, "dead_overflow"),
		now:       time.Now,
	}
	for _, d := range []string{q.pendingD, q.inflightD, q.deadD, q.overflow} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("queue: mkdir %s: %w", d, err)
		}
	}
	// Recover inflight → pending.
	rec, err := q.recoverInflight()
	if err != nil {
		return nil, err
	}
	q.stats.Recovered = uint64(rec)
	return q, nil
}

// Close is a no-op for the file backend.
func (q *Queue) Close() error { return nil }

// SetClock replaces the time source. Test-only.
func (q *Queue) SetClock(fn func() time.Time) { q.now = fn }

// Enqueue stores a fresh pending event. payloadJSON is gzipped before write.
func (q *Queue) Enqueue(id, destination string, payloadJSON []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	gz, err := gzipBytes(payloadJSON)
	if err != nil {
		return err
	}
	if len(gz) > q.opts.MaxRowBytes {
		return ErrPayloadTooLarge
	}
	if err := q.evictIfFull(int64(len(gz))); err != nil {
		return err
	}
	r := Record{
		ID:          id,
		Destination: destination,
		EnqueuedAt:  q.now().UnixMilli(),
		NextAttempt: q.now().UnixMilli(),
		Attempts:    0,
		Status:      StatusPending,
		Payload:     gz,
	}
	if err := q.writeRecord(q.pendingD, &r); err != nil {
		q.stats.WriteErrors++
		return err
	}
	return nil
}

// Peek returns up to limit pending records WITHOUT changing their status.
// Records are decompressed and returned in ascending enqueued-at order.
// This is the read-only path used by `scouttrace tail`.
func (q *Queue) Peek(destination string, limit int) ([]Record, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries, err := q.listDir(q.pendingD, destination)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].EnqueuedAt < entries[j].EnqueuedAt })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]Record, 0, len(entries))
	for _, m := range entries {
		r, err := q.readRecord(q.pendingD, m.ID)
		if err != nil {
			continue
		}
		if dec, err := gunzipBytes(r.Payload); err == nil {
			r.Payload = dec
		}
		out = append(out, r)
	}
	return out, nil
}

// ClaimPending atomically transitions up to limit pending records (whose
// next_attempt <= now) to inflight, and returns them in ascending
// next_attempt order. Each record's payload is gunzipped.
func (q *Queue) ClaimPending(destination string, limit int) ([]Record, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries, err := q.listDir(q.pendingD, destination)
	if err != nil {
		return nil, err
	}
	now := q.now().UnixMilli()
	eligible := entries[:0]
	for _, e := range entries {
		if e.NextAttempt <= now {
			eligible = append(eligible, e)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].NextAttempt < eligible[j].NextAttempt })
	if limit > 0 && len(eligible) > limit {
		eligible = eligible[:limit]
	}
	out := make([]Record, 0, len(eligible))
	for _, m := range eligible {
		r, err := q.readRecord(q.pendingD, m.ID)
		if err != nil {
			continue
		}
		r.Status = StatusInflight
		if err := q.moveRecord(q.pendingD, q.inflightD, &r); err != nil {
			continue
		}
		// Decompress payload for caller.
		dec, err := gunzipBytes(r.Payload)
		if err == nil {
			r.Payload = dec
		}
		out = append(out, r)
	}
	return out, nil
}

// Ack permanently removes the inflight record.
func (q *Queue) Ack(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return os.Remove(filepath.Join(q.inflightD, id+".rec"))
}

// Retry re-schedules an inflight record back to pending with the supplied
// nextAttempt and bumped attempts/last_error.
func (q *Queue) Retry(id string, nextAttempt time.Time, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.readRecord(q.inflightD, id)
	if err != nil {
		return err
	}
	r.Attempts++
	r.NextAttempt = nextAttempt.UnixMilli()
	r.LastError = lastErr
	r.Status = StatusPending
	return q.moveRecord(q.inflightD, q.pendingD, &r)
}

// MarkDead moves an inflight record to the dead lane.
func (q *Queue) MarkDead(id, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.readRecord(q.inflightD, id)
	if err != nil {
		return err
	}
	r.Status = StatusDead
	r.LastError = lastErr
	return q.moveRecord(q.inflightD, q.deadD, &r)
}

// Stats returns a counters snapshot.
func (q *Queue) Stats() (Stats, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.stats
	for _, p := range []struct {
		dir string
		n   *int
	}{
		{q.pendingD, &s.Pending},
		{q.inflightD, &s.Inflight},
		{q.deadD, &s.Dead},
	} {
		entries, err := os.ReadDir(p.dir)
		if err != nil {
			return Stats{}, err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".rec") {
				*p.n++
			}
		}
	}
	return s, nil
}

// Prune removes dead records older than maxAge.
func (q *Queue) Prune(maxAge time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := q.now().Add(-maxAge).UnixMilli()
	for _, d := range []string{q.deadD, q.overflow} {
		entries, err := q.listDir(d, "")
		if err != nil {
			return err
		}
		for _, m := range entries {
			if m.EnqueuedAt < cutoff {
				_ = os.Remove(filepath.Join(d, m.ID+".rec"))
			}
		}
	}
	return nil
}

// RecoverInflight rolls inflight records back to pending. Idempotent.
func (q *Queue) RecoverInflight() (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.recoverInflight()
}

// ----- internals -----

type entryMeta struct {
	ID          string
	Destination string
	EnqueuedAt  int64
	NextAttempt int64
}

func (q *Queue) listDir(dir, dest string) ([]entryMeta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]entryMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rec") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".rec")
		r, err := q.readMetaOnly(dir, id)
		if err != nil {
			continue
		}
		if dest != "" && r.Destination != dest {
			continue
		}
		out = append(out, entryMeta{
			ID:          r.ID,
			Destination: r.Destination,
			EnqueuedAt:  r.EnqueuedAt,
			NextAttempt: r.NextAttempt,
		})
	}
	return out, nil
}

func (q *Queue) recoverInflight() (int, error) {
	entries, err := q.listDir(q.inflightD, "")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, m := range entries {
		r, err := q.readRecord(q.inflightD, m.ID)
		if err != nil {
			continue
		}
		r.Status = StatusPending
		if err := q.moveRecord(q.inflightD, q.pendingD, &r); err == nil {
			count++
		}
	}
	return count, nil
}

func (q *Queue) writeRecord(dir string, r *Record) error {
	hdr, err := json.Marshal(struct {
		ID          string `json:"id"`
		Destination string `json:"destination"`
		EnqueuedAt  int64  `json:"enqueued_at"`
		NextAttempt int64  `json:"next_attempt"`
		Attempts    int    `json:"attempts"`
		Status      Status `json:"status"`
		LastError   string `json:"last_error,omitempty"`
		PayloadLen  int    `json:"payload_len"`
	}{
		ID: r.ID, Destination: r.Destination, EnqueuedAt: r.EnqueuedAt,
		NextAttempt: r.NextAttempt, Attempts: r.Attempts, Status: r.Status,
		LastError: r.LastError, PayloadLen: len(r.Payload),
	})
	if err != nil {
		return err
	}
	final := filepath.Join(dir, r.ID+".rec")
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufWrite{f: f}
	w.write(hdr)
	w.write([]byte("\n"))
	w.write(r.Payload)
	if err := w.err(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, final)
}

func (q *Queue) readRecord(dir, id string) (Record, error) {
	b, err := os.ReadFile(filepath.Join(dir, id+".rec"))
	if err != nil {
		return Record{}, err
	}
	idx := bytes.IndexByte(b, '\n')
	if idx < 0 {
		return Record{}, errors.New("queue: malformed record (no header terminator)")
	}
	var hdr struct {
		ID          string `json:"id"`
		Destination string `json:"destination"`
		EnqueuedAt  int64  `json:"enqueued_at"`
		NextAttempt int64  `json:"next_attempt"`
		Attempts    int    `json:"attempts"`
		Status      Status `json:"status"`
		LastError   string `json:"last_error,omitempty"`
		PayloadLen  int    `json:"payload_len"`
	}
	if err := json.Unmarshal(b[:idx], &hdr); err != nil {
		return Record{}, err
	}
	payload := b[idx+1:]
	return Record{
		ID: hdr.ID, Destination: hdr.Destination, EnqueuedAt: hdr.EnqueuedAt,
		NextAttempt: hdr.NextAttempt, Attempts: hdr.Attempts, Status: hdr.Status,
		LastError: hdr.LastError, Payload: payload,
	}, nil
}

func (q *Queue) readMetaOnly(dir, id string) (Record, error) {
	f, err := os.Open(filepath.Join(dir, id+".rec"))
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	rdr := io.LimitReader(f, 4096)
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(rdr, buf)
	idx := bytes.IndexByte(buf[:n], '\n')
	if idx < 0 {
		return Record{}, errors.New("queue: malformed header")
	}
	var hdr struct {
		ID          string `json:"id"`
		Destination string `json:"destination"`
		EnqueuedAt  int64  `json:"enqueued_at"`
		NextAttempt int64  `json:"next_attempt"`
		Attempts    int    `json:"attempts"`
		Status      Status `json:"status"`
	}
	if err := json.Unmarshal(buf[:idx], &hdr); err != nil {
		return Record{}, err
	}
	return Record{
		ID: hdr.ID, Destination: hdr.Destination, EnqueuedAt: hdr.EnqueuedAt,
		NextAttempt: hdr.NextAttempt, Attempts: hdr.Attempts, Status: hdr.Status,
	}, nil
}

func (q *Queue) moveRecord(srcDir, dstDir string, r *Record) error {
	// Re-write the header with updated metadata and rename atomically.
	if err := q.writeRecord(dstDir, r); err != nil {
		return err
	}
	return os.Remove(filepath.Join(srcDir, r.ID+".rec"))
}

// evictIfFull enforces MaxBytes per the configured drop policy.
func (q *Queue) evictIfFull(incoming int64) error {
	if q.opts.MaxBytes <= 0 {
		return nil
	}
	size, _ := q.totalBytes()
	if size+incoming <= q.opts.MaxBytes {
		return nil
	}
	switch q.opts.DropPolicy {
	case "newest":
		return errors.New("queue: full, drop=newest")
	case "block":
		// Caller treats this as a temporary error.
		return errors.New("queue: full, drop=block")
	default: // "oldest"
		entries, _ := q.listDir(q.pendingD, "")
		sort.Slice(entries, func(i, j int) bool { return entries[i].EnqueuedAt < entries[j].EnqueuedAt })
		freed := int64(0)
		for _, m := range entries {
			if size-freed+incoming <= q.opts.MaxBytes {
				break
			}
			r, err := q.readRecord(q.pendingD, m.ID)
			if err != nil {
				continue
			}
			r.Status = "evicted"
			if err := q.moveRecord(q.pendingD, q.overflow, &r); err == nil {
				q.stats.Evicted++
				freed += int64(len(r.Payload))
			}
		}
		return nil
	}
}

func (q *Queue) totalBytes() (int64, error) {
	var total int64
	for _, d := range []string{q.pendingD, q.inflightD, q.deadD} {
		entries, err := os.ReadDir(d)
		if err != nil {
			return 0, err
		}
		for _, e := range entries {
			info, err := e.Info()
			if err == nil {
				total += info.Size()
			}
		}
	}
	return total, nil
}

// ----- gzip helpers -----

func gzipBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(in); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(in []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// bufWrite is a tiny helper that swallows write errors until err() is called.
type bufWrite struct {
	f *os.File
	e error
}

func (b *bufWrite) write(p []byte) {
	if b.e != nil {
		return
	}
	_, b.e = b.f.Write(p)
}
func (b *bufWrite) err() error { return b.e }
