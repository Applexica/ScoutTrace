// Package webhookscout is the default ScoutTrace destination. It adapts the
// canonical ScoutTrace ToolCallEvent envelope to WebhookScout's MCP event ingest
// API.
//
// Per §18.4 of the design, this package must NOT have any privileged code path
// beyond what an arbitrary user-configured HTTP destination can access. It
// composes the HTTP adapter for transport concerns and performs only endpoint,
// auth-header, and payload-shape adaptation.
package webhookscout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/destinations/httpdest"
	"github.com/webhookscout/scouttrace/internal/event"
)

// Config configures the WebhookScout adapter.
type Config struct {
	Name          string
	APIBase       string // e.g. https://api.webhookscout.com
	AgentID       string
	AuthHeaderRef string // typically keychain://scouttrace/webhookscout/<name>
}

// Adapter wraps httpdest.Adapter with WebhookScout MCP ingest semantics.
type Adapter struct {
	inner *httpdest.Adapter
	cfg   Config
}

type resolverFunc func(ref string) (string, error)

func (f resolverFunc) Resolve(ref string) (string, error) { return f(ref) }

// New constructs a WebhookScout adapter.
func New(cfg Config, res destinations.Resolver) (*Adapter, error) {
	if cfg.APIBase == "" {
		return nil, errors.New("webhookscout: api_base required")
	}
	if cfg.AgentID == "" {
		return nil, errors.New("webhookscout: agent_id required")
	}
	url := fmt.Sprintf("%s/api/mcp/%s/events", strings.TrimRight(cfg.APIBase, "/"), cfg.AgentID)
	inner, err := httpdest.New(httpdest.Config{
		Name:          cfg.Name,
		URL:           url,
		AuthHeaderRef: cfg.AuthHeaderRef,
		UseGzip:       true,
		BodyEnvelope:  webhookScoutEventBody,
	}, webhookScoutResolver(res))
	if err != nil {
		return nil, err
	}
	return &Adapter{inner: inner, cfg: cfg}, nil
}

// Name returns the adapter name.
func (a *Adapter) Name() string { return a.cfg.Name }

// Type returns "webhookscout".
func (a *Adapter) Type() string { return "webhookscout" }

// Send posts each ScoutTrace event as one WebhookScout MCP event. The current
// WebhookScout ingest API accepts a single event per POST, not a batch envelope.
func (a *Adapter) Send(ctx context.Context, b destinations.Batch) destinations.Result {
	if len(b.Events) == 0 {
		return destinations.Result{OK: true}
	}
	var last destinations.Result
	for i, ev := range b.Events {
		one := destinations.Batch{
			ID:         fmt.Sprintf("%s-%d", b.ID, i),
			Events:     []json.RawMessage{ev},
			PreparedAt: b.PreparedAt,
		}
		last = a.inner.Send(ctx, one)
		if !last.OK {
			return last
		}
	}
	return last
}

// Close releases inner resources.
func (a *Adapter) Close() error { return a.inner.Close() }

func webhookScoutResolver(res destinations.Resolver) destinations.Resolver {
	if res == nil {
		return nil
	}
	return resolverFunc(func(ref string) (string, error) {
		secret, err := res.Resolve(ref)
		if err != nil {
			return "", err
		}
		secret = strings.TrimSpace(secret)
		if secret == "" || strings.HasPrefix(strings.ToLower(secret), "bearer ") {
			return secret, nil
		}
		return "Bearer " + secret, nil
	})
}

type httpBatchEnvelope struct {
	Events []json.RawMessage `json:"events"`
}

func webhookScoutEventBody(body []byte) ([]byte, error) {
	var batch httpBatchEnvelope
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	if len(batch.Events) != 1 {
		return nil, fmt.Errorf("webhookscout: expected exactly one event, got %d", len(batch.Events))
	}
	return mapToolCallEvent(batch.Events[0])
}

func mapToolCallEvent(raw json.RawMessage) ([]byte, error) {
	var ev event.ToolCallEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	tool := strings.TrimSpace(ev.Tool.Name)
	if tool == "" {
		tool = strings.TrimSpace(ev.Server.Name)
	}
	if tool == "" {
		tool = "unknown"
	}
	status := "error"
	if ev.Response.OK {
		status = "ok"
	}
	out := map[string]any{
		"tool":   tool,
		"status": status,
	}
	if len(ev.Request.Args) > 0 {
		out["input"] = string(ev.Request.Args)
	}
	if len(ev.Response.Result) > 0 {
		out["output"] = string(ev.Response.Result)
	}
	if ev.Timing.LatencyMS >= 0 {
		out["latencyMs"] = ev.Timing.LatencyMS
	}
	if len(ev.Response.Error) > 0 {
		out["errorMessage"] = string(ev.Response.Error)
		if _, ok := out["output"]; !ok {
			out["output"] = string(ev.Response.Error)
		}
	}
	if ev.Billing != nil {
		if ev.Billing.CostUSD != nil {
			out["costUsd"] = *ev.Billing.CostUSD
		}
		if ev.Billing.TokensIn != nil {
			out["tokensIn"] = *ev.Billing.TokensIn
		}
		if ev.Billing.TokensOut != nil {
			out["tokensOut"] = *ev.Billing.TokensOut
		}
		if ev.Billing.Model != "" {
			out["model"] = ev.Billing.Model
		}
		if ev.Billing.Provider != "" {
			out["provider"] = ev.Billing.Provider
		}
		if ev.Billing.PricingSource != "" {
			out["pricingSource"] = ev.Billing.PricingSource
		}
	}
	return json.Marshal(out)
}
