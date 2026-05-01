// Package filedest implements a destination adapter that writes events as
// NDJSON to a local file with size-based rotation.
package filedest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webhookscout/scouttrace/internal/destinations"
)

// Config configures a file adapter.
type Config struct {
	Name     string
	Path     string
	RotateMB int // 0 = no rotation
	Keep     int // backups to keep; default 7
}

// Adapter implements destinations.Adapter for files.
type Adapter struct {
	cfg Config
	mu  sync.Mutex
}

// New returns a configured file adapter. The directory is created if missing.
func New(cfg Config) (*Adapter, error) {
	if cfg.Path == "" {
		return nil, errors.New("filedest: path required")
	}
	if cfg.Keep == 0 {
		cfg.Keep = 7
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg}, nil
}

// Name returns the adapter name.
func (a *Adapter) Name() string { return a.cfg.Name }

// Type returns "file".
func (a *Adapter) Type() string { return "file" }

// Send writes the batch as NDJSON. Disk-full → retriable so the queue keeps
// the events.
func (a *Adapter) Send(ctx context.Context, b destinations.Batch) destinations.Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.rotateIfNeeded(); err != nil {
		return destinations.Result{Retriable: true, Err: err}
	}
	f, err := os.OpenFile(a.cfg.Path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return destinations.Result{Retriable: true, Err: err}
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for i := range b.Events {
		if err := enc.Encode(&b.Events[i]); err != nil {
			return destinations.Result{Retriable: true, Err: err}
		}
	}
	if err := f.Sync(); err != nil {
		return destinations.Result{Retriable: true, Err: err}
	}
	return destinations.Result{OK: true}
}

// Close is a no-op.
func (a *Adapter) Close() error { return nil }

func (a *Adapter) rotateIfNeeded() error {
	if a.cfg.RotateMB <= 0 {
		return nil
	}
	st, err := os.Stat(a.cfg.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Size() < int64(a.cfg.RotateMB)*1024*1024 {
		return nil
	}
	bak := fmt.Sprintf("%s.%s.bak", a.cfg.Path, time.Now().UTC().Format("20060102T150405"))
	if err := os.Rename(a.cfg.Path, bak); err != nil {
		return err
	}
	return a.pruneBackups()
}

func (a *Adapter) pruneBackups() error {
	dir := filepath.Dir(a.cfg.Path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := filepath.Base(a.cfg.Path) + "."
	type bk struct {
		name string
		mt   time.Time
	}
	var bks []bk
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		bks = append(bks, bk{name: e.Name(), mt: info.ModTime()})
	}
	sort.Slice(bks, func(i, j int) bool { return bks[i].mt.After(bks[j].mt) })
	for i := a.cfg.Keep; i < len(bks); i++ {
		_ = os.Remove(filepath.Join(dir, bks[i].name))
	}
	return nil
}
