// Package stdoutdest writes events as NDJSON to os.Stdout (or any io.Writer).
package stdoutdest

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/webhookscout/scouttrace/internal/destinations"
)

// Config configures a stdout adapter. Output is overridable for tests.
type Config struct {
	Name   string
	Output io.Writer
}

// Adapter implements destinations.Adapter writing NDJSON to a writer.
type Adapter struct {
	cfg Config
	w   io.Writer
	mu  sync.Mutex
}

// New returns a configured stdout adapter.
func New(cfg Config) *Adapter {
	w := cfg.Output
	if w == nil {
		w = os.Stdout
	}
	return &Adapter{cfg: cfg, w: w}
}

// Name returns the adapter name.
func (a *Adapter) Name() string { return a.cfg.Name }

// Type returns "stdout".
func (a *Adapter) Type() string { return "stdout" }

// Send emits one JSON line per event.
func (a *Adapter) Send(ctx context.Context, b destinations.Batch) destinations.Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	enc := json.NewEncoder(a.w)
	for i := range b.Events {
		if err := enc.Encode(&b.Events[i]); err != nil {
			// Per §11.4: convert non-retriable broken-pipe to ack-with-warning.
			return destinations.Result{OK: true, Err: err}
		}
	}
	return destinations.Result{OK: true}
}

// Close is a no-op.
func (a *Adapter) Close() error { return nil }
