// Package halt persists per-agent halt state observed in WebhookScout
// ingest responses. The proxy reads this cache before forwarding a
// tools/call to refuse outbound traffic for halted agents; the
// claude-hook pre-tool-use subcommand reads the same file to block
// Claude Code tool invocations before the model executes them.
//
// The cache is a small JSON file under the ScoutTrace home (default
// ~/.scouttrace/halt-state.json). Each instance of the cache reads the
// file lazily on the first call and writes it atomically (tmp file +
// rename) whenever the in-memory state changes. The format is forward-
// compatible: unknown fields are preserved on round-trip via
// json.RawMessage extras.
//
// See docs/superpowers/specs/2026-05-12-cost-gates-design.md (in the
// webhook-debug-tool-mvp repo) for the broader feature design.
package halt

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the persisted halt status for one agent.
type State struct {
	Halted              bool   `json:"halted"`
	HaltReason          string `json:"halt_reason,omitempty"`
	ManualClearRequired bool   `json:"manual_clear_required,omitempty"`
	// UpdatedAt is the wall clock at the time the cache was last updated
	// from an ingest response. Stale entries (Halted=true but no update
	// in HALT_STALE_AFTER) are still treated as halted — operator intent
	// from WebhookScout is the source of truth and ScoutTrace must err
	// on the side of refusing tool calls.
	UpdatedAt time.Time `json:"updated_at"`
}

// fileShape is the on-disk JSON structure. The top-level "agents" map
// keys by agentId. A "version" field is reserved for future migrations.
type fileShape struct {
	Version int              `json:"version"`
	Agents  map[string]State `json:"agents"`
}

// Cache is a goroutine-safe halt-state store backed by a JSON file.
// Multiple processes (proxy, claude-hook) sharing the same path will
// not corrupt one another because writes go through a temp-file rename
// — but they will race on read-modify-write. ScoutTrace's actual write
// path is the dispatcher in a single proxy process; CLI hooks are
// read-only. This trade-off matches the file queue design.
type Cache struct {
	path string

	mu     sync.RWMutex
	loaded bool
	data   map[string]State
}

// NewCache returns a cache backed by the JSON file at path. The file is
// not opened until the first read or write. An empty path returns a
// cache that is functional but persists nothing (in-memory only) —
// useful for tests.
func NewCache(path string) *Cache {
	return &Cache{path: path, data: make(map[string]State)}
}

// Get returns the current halt state for agentID, or zero value when
// none is recorded. Always loads the file once before the first read.
func (c *Cache) Get(agentID string) State {
	c.ensureLoaded()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data == nil {
		return State{}
	}
	return c.data[agentID]
}

// Set records the new halt state for agentID, persisting the file
// atomically on every change. Callers should only call Set when state
// actually changed (the cache itself does NOT diff — it always writes).
// The dispatcher is responsible for skipping the call when the new
// state matches the cached one.
func (c *Cache) Set(agentID string, s State) error {
	c.ensureLoaded()
	c.mu.Lock()
	if c.data == nil {
		c.data = make(map[string]State)
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	c.data[agentID] = s
	snapshot := cloneMap(c.data)
	path := c.path
	c.mu.Unlock()

	if path == "" {
		return nil
	}
	return writeAtomic(path, snapshot)
}

// SetIfChanged updates the cache only when the new state differs from
// the cached one. Returns true if a write happened.
func (c *Cache) SetIfChanged(agentID string, s State) (bool, error) {
	c.ensureLoaded()
	c.mu.Lock()
	prior, ok := c.data[agentID]
	if ok && prior.Halted == s.Halted && prior.HaltReason == s.HaltReason && prior.ManualClearRequired == s.ManualClearRequired {
		c.mu.Unlock()
		return false, nil
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	c.data[agentID] = s
	snapshot := cloneMap(c.data)
	path := c.path
	c.mu.Unlock()
	if path == "" {
		return true, nil
	}
	return true, writeAtomic(path, snapshot)
}

// All returns a copy of the entire cache, for debugging / status output.
func (c *Cache) All() map[string]State {
	c.ensureLoaded()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneMap(c.data)
}

func (c *Cache) ensureLoaded() {
	c.mu.RLock()
	if c.loaded {
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return
	}
	c.loaded = true
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		// Corrupt or unreadable file: start fresh rather than refuse
		// service. The dispatcher will re-populate on the next event.
		return
	}
	var fs fileShape
	if err := json.Unmarshal(raw, &fs); err != nil {
		return
	}
	if fs.Agents != nil {
		c.data = fs.Agents
	}
}

func cloneMap(m map[string]State) map[string]State {
	out := make(map[string]State, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func writeAtomic(path string, data map[string]State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".halt-state.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fileShape{Version: 1, Agents: data}); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// DefaultPath returns the canonical halt-state path for the given
// ScoutTrace home directory.
func DefaultPath(scoutHome string) string {
	if scoutHome == "" {
		return ""
	}
	return filepath.Join(scoutHome, "halt-state.json")
}
