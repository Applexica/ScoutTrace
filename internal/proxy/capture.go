// Package proxy is the per-server orchestrator that wires host stdio →
// upstream stdio with capture in between.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/webhookscout/scouttrace/internal/billing"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/jsonrpc"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/wire"
)

// StaticPriceLookup is the callback used to resolve a configured static
// per-tool price for a (server, tool) pair. A nil lookup disables static
// pricing.
type StaticPriceLookup = billing.StaticPriceLookup

// CaptureWorker consumes wire frames, builds envelopes, applies redaction,
// and persists them to the queue.
type CaptureWorker struct {
	Session        *event.SessionState
	Correlator     *jsonrpc.Correlator
	Engine         *redact.Engine
	Capture        *redact.CapturePolicy
	Queue          *queue.Queue
	Destination    string
	Host           string
	HostVersion    string
	ScoutVersion   string
	MaxArgBytes    int // 0 = unlimited
	MaxResultBytes int // 0 = unlimited
	Logger         func(format string, args ...any)
	// StaticPrices is consulted when the response carries no reported cost
	// and no model/tokens-derived estimate is available. May be nil.
	StaticPrices StaticPriceLookup
	// LivePrices is consulted between "reported" and "estimated" in the
	// billing priority chain. A nil lookup keeps the legacy static-only
	// behavior. LiveSource is the value written to BillingBlock.PricingSource
	// on a live hit (typically "pricepertoken").
	LivePrices LiveLookup
	LiveSource string

	// metrics
	parseErrors uint64
	dropped     uint64
}

// LiveLookup mirrors billing.LiveLookup so callers don't have to import the
// billing package just to hand a live pricing function to the proxy.
type LiveLookup = billing.LiveLookup

// NewCaptureWorker constructs a worker with sensible defaults.
func NewCaptureWorker(opts CaptureWorker) *CaptureWorker {
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	if opts.Correlator == nil {
		opts.Correlator = jsonrpc.NewCorrelator(0)
	}
	if opts.Capture == nil {
		opts.Capture = &redact.CapturePolicy{}
	}
	cw := opts
	return &cw
}

// Run consumes from in until it is closed, returning when done.
func (cw *CaptureWorker) Run(ctx context.Context, in <-chan wire.Frame) {
	gcTicker := time.NewTicker(30 * time.Second)
	defer gcTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gcTicker.C:
			cw.Correlator.SweepOrphans()
		case f, ok := <-in:
			if !ok {
				return
			}
			cw.handle(f)
		}
	}
}

// Counters returns observable counters.
func (cw *CaptureWorker) Counters() (parseErr, dropped uint64) {
	return atomic.LoadUint64(&cw.parseErrors), atomic.LoadUint64(&cw.dropped)
}

// EmitServerCrashed enqueues a one-off envelope describing an unexpected
// upstream exit. Best-effort; never returns an error to the caller because
// the wire path has already finished.
func (cw *CaptureWorker) EmitServerCrashed(exitCode int, lastErr string, lastTools []string, stderrTail string) {
	if cw.Queue == nil {
		return
	}
	now := time.Now().UTC()
	env := map[string]any{
		"id":          event.NewULID(),
		"schema":      event.CrashSchemaVersion,
		"captured_at": now,
		"session_id":  cw.Session.SessionID,
		"source": map[string]any{
			"kind": "mcp_stdio", "host": cw.Host, "host_version": cw.HostVersion,
			"scouttrace_version": cw.ScoutVersion,
		},
		"server": map[string]any{
			"name": cw.Session.ServerName, "protocol_version": cw.Session.ProtocolVersion,
		},
		"exit_code":       exitCode,
		"last_error":      lastErr,
		"last_tool_names": lastTools,
		"stderr_tail":     stderrTail,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		atomic.AddUint64(&cw.dropped, 1)
		return
	}
	id, _ := env["id"].(string)
	if err := cw.Queue.Enqueue(id, cw.Destination, raw); err != nil {
		atomic.AddUint64(&cw.dropped, 1)
	}
}

func (cw *CaptureWorker) handle(f wire.Frame) {
	defer func() {
		if r := recover(); r != nil {
			atomic.AddUint64(&cw.dropped, 1)
			cw.Logger("capture panic recovered: %v", r)
		}
	}()
	msg, err := jsonrpc.Parse(f.Bytes)
	if err != nil {
		atomic.AddUint64(&cw.parseErrors, 1)
		return
	}
	switch {
	case msg.IsRequest() && f.Dir == wire.DirHostToUpstream:
		span := event.NewSpanID()
		cw.Correlator.AddRequest(msg, span)
		// initialize is request from host: capture protocol metadata in
		// session state on the *response*; request side is uninteresting.
	case msg.IsResponse() && f.Dir == wire.DirUpstreamToHost:
		pair, ok := cw.Correlator.MatchResponse(msg)
		if !ok {
			return
		}
		switch pair.Method {
		case jsonrpc.MethodInitialize:
			cw.applyInitializeResult(pair)
			return
		case jsonrpc.MethodToolsCall:
			cw.emitToolCall(pair)
		}
	}
}

func (cw *CaptureWorker) applyInitializeResult(p jsonrpc.MatchedPair) {
	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      map[string]any `json:"serverInfo"`
	}
	if len(p.Result) > 0 {
		_ = json.Unmarshal(p.Result, &res)
	}
	cw.Session.ProtocolVersion = res.ProtocolVersion
	cw.Session.ServerInfo = res.ServerInfo
	cw.Session.Capabilities = capsKeys(res.Capabilities)
	cw.Session.Initialized = true
}

func capsKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (cw *CaptureWorker) emitToolCall(p jsonrpc.MatchedPair) {
	ev := event.New(cw.Session, cw.ScoutVersion, cw.Host, cw.HostVersion)
	ev.Server = event.ServerBlock{
		Name:            cw.Session.ServerName,
		ProtocolVersion: cw.Session.ProtocolVersion,
		Capabilities:    cw.Session.Capabilities,
	}
	ev.Tool = event.ToolBlock{Name: extractToolName(p.Params)}
	ev.SpanID = p.SpanID

	args := extractArgs(p.Params)
	argsOriginal := len(args)
	argsTruncated := false
	if !cw.Capture.ShouldCaptureArgs(cw.Session.ServerName) {
		args = nil
	} else if cw.MaxArgBytes > 0 && len(args) > cw.MaxArgBytes {
		args = json.RawMessage(fmt.Sprintf("\"[truncated:%d->%d]\"", argsOriginal, cw.MaxArgBytes))
		argsTruncated = true
	}
	ev.Request = event.RequestBlock{
		JSONRPCID:         p.ID,
		Args:              args,
		ArgsTruncated:     argsTruncated,
		ArgsBytesOriginal: argsOriginal,
	}
	resultBytes := p.Result
	resultOriginal := len(resultBytes)
	resultTruncated := false
	if !cw.Capture.ShouldCaptureResult(cw.Session.ServerName) {
		resultBytes = nil
	} else if cw.MaxResultBytes > 0 && len(resultBytes) > cw.MaxResultBytes {
		resultBytes = json.RawMessage(fmt.Sprintf("\"[truncated:%d->%d]\"", resultOriginal, cw.MaxResultBytes))
		resultTruncated = true
	}
	ev.Response = event.ResponseBlock{
		OK:                  p.Error == nil || len(p.Error) == 0,
		Result:              resultBytes,
		ResultTruncated:     resultTruncated,
		ResultBytesOriginal: resultOriginal,
		Error:               p.Error,
	}
	ev.Timing = event.TimingBlock{
		StartedAt: p.StartedAt,
		EndedAt:   p.EndedAt,
		LatencyMS: p.EndedAt.Sub(p.StartedAt).Milliseconds(),
	}
	// Best-effort billing extraction from the *raw* result before any
	// truncation marker has been substituted in. Run on p.Result rather
	// than resultBytes so we can recognise metadata even when capture is
	// disabled or the body was truncated. The live lookup is given a
	// short-lived context so a slow PricePerToken response cannot stall
	// capture; on timeout the call returns ok=false and falls through to
	// the static estimate.
	enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 3*time.Second)
	bb := billing.EnrichLive(enrichCtx, p.Result, cw.Session.ServerName, ev.Tool.Name, cw.StaticPrices, cw.LivePrices, cw.LiveSource)
	enrichCancel()
	if !bb.Empty() {
		ev.Billing = &event.BillingBlock{
			CostUSD:       bb.CostUSD,
			TokensIn:      bb.TokensIn,
			TokensOut:     bb.TokensOut,
			Model:         bb.Model,
			Provider:      bb.Provider,
			PricingSource: bb.PricingSource,
		}
	}
	// Apply redaction.
	raw, err := json.Marshal(ev)
	if err != nil {
		atomic.AddUint64(&cw.dropped, 1)
		return
	}
	red, res, err := cw.Engine.Apply(raw)
	if err != nil {
		atomic.AddUint64(&cw.dropped, 1)
		return
	}
	// Re-decode the redacted bytes back into the envelope so the redaction
	// block & truncation are reflected accurately.
	var redacted event.ToolCallEvent
	if err := json.Unmarshal(red, &redacted); err == nil {
		redacted.Redaction = event.RedactionBlock{
			PolicyName:     cw.Engine.Policy().Name,
			PolicyHash:     cw.Engine.Policy().Hash(),
			FieldsRedacted: res.FieldsRedacted,
			RulesApplied:   res.RulesApplied,
		}
		final, err := json.Marshal(&redacted)
		if err == nil {
			if err := cw.Queue.Enqueue(redacted.ID, cw.Destination, final); err != nil {
				atomic.AddUint64(&cw.dropped, 1)
				cw.Logger("queue enqueue failed: %v", err)
			}
			return
		}
	}
	if err := cw.Queue.Enqueue(ev.ID, cw.Destination, red); err != nil {
		atomic.AddUint64(&cw.dropped, 1)
	}
}

func extractToolName(params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err == nil {
		return p.Name
	}
	return ""
}

func extractArgs(params json.RawMessage) json.RawMessage {
	var p struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err == nil && len(p.Arguments) > 0 {
		return p.Arguments
	}
	return nil
}
