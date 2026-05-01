// Package destinations defines the Adapter interface that all sinks must
// implement and re-exports a small registry helper for the dispatcher.
package destinations

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Batch is a batch of events with a stable idempotency key. Events are
// carried as raw JSON so envelope variants (ToolCallEvent, server_crashed,
// queue_recovered, …) all flow through unchanged.
type Batch struct {
	ID         string
	Events     []json.RawMessage
	PreparedAt time.Time
}

// Result is what an Adapter.Send returns.
type Result struct {
	OK         bool
	Retriable  bool
	Status     int
	Err        error
	RetryAfter time.Duration
}

// Adapter is implemented by every destination type.
type Adapter interface {
	Name() string
	Type() string
	Send(ctx context.Context, b Batch) Result
	Close() error
}

// Resolver resolves credential refs into secret material.
type Resolver interface {
	Resolve(ref string) (string, error)
}

// ErrNoSuchAdapter is returned by Registry.Lookup for unknown names.
var ErrNoSuchAdapter = errors.New("destinations: no such adapter")

// Registry is a tiny name → Adapter map.
type Registry struct {
	byName map[string]Adapter
	order  []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Adapter)}
}

// Add registers an adapter; returns an error if a name conflicts.
func (r *Registry) Add(a Adapter) error {
	if _, dup := r.byName[a.Name()]; dup {
		return errors.New("destinations: duplicate name " + a.Name())
	}
	r.byName[a.Name()] = a
	r.order = append(r.order, a.Name())
	return nil
}

// Lookup returns the adapter registered under name.
func (r *Registry) Lookup(name string) (Adapter, error) {
	a, ok := r.byName[name]
	if !ok {
		return nil, ErrNoSuchAdapter
	}
	return a, nil
}

// Names returns adapter names in registration order.
func (r *Registry) Names() []string { return append([]string(nil), r.order...) }

// All returns adapters in registration order.
func (r *Registry) All() []Adapter {
	out := make([]Adapter, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Close closes every adapter and returns the first error.
func (r *Registry) Close() error {
	var firstErr error
	for _, a := range r.All() {
		if err := a.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
