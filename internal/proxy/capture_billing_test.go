package proxy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/wire"
)

// runOneToolCall drives one tools/call request+response through a fresh
// capture worker and returns the single event the queue captured.
func runOneToolCall(t *testing.T, opts CaptureWorker, request, response string) event.ToolCallEvent {
	t.Helper()
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	if opts.Engine == nil {
		eng, _ := redact.NewEngine(redact.StandardProfile(), nil)
		opts.Engine = eng
	}
	if opts.Session == nil {
		opts.Session = event.NewSession("filesystem")
	}
	if opts.Queue == nil {
		opts.Queue = q
	}
	if opts.Destination == "" {
		opts.Destination = "default"
	}
	if opts.ScoutVersion == "" {
		opts.ScoutVersion = "test"
	}
	cw := NewCaptureWorker(opts)
	in := make(chan wire.Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cw.Run(ctx, in); close(done) }()
	in <- wire.Frame{Dir: wire.DirHostToUpstream, Bytes: []byte(request)}
	in <- wire.Frame{Dir: wire.DirUpstreamToHost, Bytes: []byte(response)}
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	rows, err := opts.Queue.ClaimPending(opts.Destination, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	var ev event.ToolCallEvent
	if err := json.Unmarshal(rows[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return ev
}

func TestMCPCaptureExtractsReportedCostAndTokens(t *testing.T) {
	ev := runOneToolCall(t, CaptureWorker{},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"summarise","arguments":{"q":"x"}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"cost_usd":0.04,"tokens_in":1000,"tokens_out":200,"model":"claude-sonnet-4-6","provider":"anthropic","content":[{"type":"text","text":"ok"}]}}`,
	)
	if ev.Billing == nil {
		t.Fatalf("billing missing on captured event")
	}
	if ev.Billing.CostUSD == nil || *ev.Billing.CostUSD != 0.04 {
		t.Fatalf("cost = %v", ev.Billing.CostUSD)
	}
	if ev.Billing.Model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q", ev.Billing.Model)
	}
	if ev.Billing.Provider != "anthropic" {
		t.Fatalf("provider = %q", ev.Billing.Provider)
	}
	if ev.Billing.PricingSource != "reported" {
		t.Fatalf("pricing_source = %q, want reported", ev.Billing.PricingSource)
	}
}

func TestMCPCaptureEstimatesCostWhenOnlyTokensAndModelReported(t *testing.T) {
	ev := runOneToolCall(t, CaptureWorker{},
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"summarise"}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"usage":{"input_tokens":1000,"output_tokens":1000},"model":"claude-haiku-4-5"}}`,
	)
	if ev.Billing == nil || ev.Billing.CostUSD == nil {
		t.Fatalf("expected estimated cost, got %+v", ev.Billing)
	}
	if ev.Billing.PricingSource != "estimated" {
		t.Fatalf("pricing_source = %q, want estimated", ev.Billing.PricingSource)
	}
	if *ev.Billing.CostUSD <= 0 {
		t.Fatalf("expected positive cost, got %v", *ev.Billing.CostUSD)
	}
}

func TestMCPCaptureNoBillingWhenNoMetadata(t *testing.T) {
	ev := runOneToolCall(t, CaptureWorker{},
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"no billing here"}]}}`,
	)
	if ev.Billing != nil {
		t.Fatalf("expected no billing block, got %+v", ev.Billing)
	}
}
