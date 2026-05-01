// Package webhookscout is the default ScoutTrace destination. It is a
// thin wrapper around the HTTP adapter that adds an `agent_id` to the
// envelope and points at the WebhookScout v1 events endpoint.
//
// Per §18.4 of the design, this package must NOT have any privileged code
// path beyond what an arbitrary user-configured HTTP destination can access.
// The wrapper composes the HTTP adapter rather than reimplementing it.
package webhookscout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/destinations/httpdest"
)

// Config configures the WebhookScout adapter.
type Config struct {
	Name          string
	APIBase       string // e.g. https://api.webhookscout.com
	AgentID       string
	AuthHeaderRef string // typically keychain://scouttrace/webhookscout/<name>
}

// Adapter wraps httpdest.Adapter with an agent-aware envelope.
type Adapter struct {
	inner *httpdest.Adapter
	cfg   Config
}

// New constructs a WebhookScout adapter.
func New(cfg Config, res destinations.Resolver) (*Adapter, error) {
	if cfg.APIBase == "" {
		return nil, errors.New("webhookscout: api_base required")
	}
	if cfg.AgentID == "" {
		return nil, errors.New("webhookscout: agent_id required")
	}
	url := fmt.Sprintf("%s/v1/agents/%s/events", cfg.APIBase, cfg.AgentID)
	inner, err := httpdest.New(httpdest.Config{
		Name:          cfg.Name,
		URL:           url,
		AuthHeaderRef: cfg.AuthHeaderRef,
		UseGzip:       true,
		BodyEnvelope: func(body []byte) ([]byte, error) {
			return wrapWithAgentID(body, cfg.AgentID)
		},
	}, res)
	if err != nil {
		return nil, err
	}
	return &Adapter{inner: inner, cfg: cfg}, nil
}

// Name returns the adapter name.
func (a *Adapter) Name() string { return a.cfg.Name }

// Type returns "webhookscout".
func (a *Adapter) Type() string { return "webhookscout" }

// Send delegates to the inner HTTP adapter.
func (a *Adapter) Send(ctx context.Context, b destinations.Batch) destinations.Result {
	return a.inner.Send(ctx, b)
}

// Close releases inner resources.
func (a *Adapter) Close() error { return a.inner.Close() }

func wrapWithAgentID(body []byte, agentID string) ([]byte, error) {
	var inner map[string]any
	if err := json.Unmarshal(body, &inner); err != nil {
		return nil, err
	}
	out := map[string]any{
		"agent_id": agentID,
	}
	for k, v := range inner {
		out[k] = v
	}
	return json.Marshal(out)
}
