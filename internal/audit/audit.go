// Package audit appends one JSON line per side-effecting CLI / runtime
// action to ~/.scouttrace/audit.log. The file is rotated at 10 MiB,
// keeping 5 generations.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxLogBytes is the rotation threshold (10 MiB).
const MaxLogBytes = 10 * 1024 * 1024

// MaxRotations is the number of generations kept.
const MaxRotations = 5

// Entry is a structured audit record.
type Entry struct {
	TS     time.Time              `json:"ts"`
	Actor  string                 `json:"actor"` // "cli", "proxy", "dispatcher"
	PID    int                    `json:"pid"`
	Event  string                 `json:"event"`
	Fields map[string]interface{} `json:"-"`
}

// MarshalJSON flattens Fields into the top-level object.
func (e Entry) MarshalJSON() ([]byte, error) {
	out := map[string]interface{}{
		"ts":    e.TS.UTC().Format(time.RFC3339Nano),
		"actor": e.Actor,
		"pid":   e.PID,
		"event": e.Event,
	}
	for k, v := range e.Fields {
		out[k] = v
	}
	return json.Marshal(out)
}

// Logger is a goroutine-safe append-only audit logger.
type Logger struct {
	path string
	mu   sync.Mutex
}

// NewLogger returns a Logger that writes to path. The directory is
// created with 0700 if absent.
func NewLogger(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return &Logger{path: path}, nil
}

// Append writes one JSON line and flushes. Safe to call on a nil receiver
// — used so callers can treat audit logging as best-effort.
func (l *Logger) Append(actor, event string, fields map[string]interface{}) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateIfNeeded(); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	e := Entry{
		TS: time.Now(), Actor: actor, PID: os.Getpid(), Event: event, Fields: fields,
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, string(b)); err != nil {
		return err
	}
	return f.Sync()
}

func (l *Logger) rotateIfNeeded() error {
	st, err := os.Stat(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Size() < MaxLogBytes {
		return nil
	}
	// Shift files: audit.log.4 → 5, ..., audit.log → 1
	for i := MaxRotations; i > 1; i-- {
		from := fmt.Sprintf("%s.%d", l.path, i-1)
		to := fmt.Sprintf("%s.%d", l.path, i)
		_ = os.Rename(from, to)
	}
	return os.Rename(l.path, l.path+".1")
}
