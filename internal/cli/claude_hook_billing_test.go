package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/webhookscout/scouttrace/internal/config"
)

func TestClaudeHookBuildsBillingFromToolResponse(t *testing.T) {
	body := []byte(`{
		"session_id":"sess",
		"hook_event_name":"PostToolUse",
		"tool_name":"mcp__llm__complete",
		"tool_input":{"prompt":"hi"},
		"tool_response":{"cost_usd":0.05,"tokens_in":1000,"tokens_out":500,"model":"claude-sonnet-4-6"}
	}`)
	ev, err := buildClaudeHookEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("buildClaudeHookEvent: %v", err)
	}
	if ev.Billing == nil || ev.Billing.CostUSD == nil || *ev.Billing.CostUSD != 0.05 {
		t.Fatalf("expected reported cost in billing, got %+v", ev.Billing)
	}
	if ev.Billing.PricingSource != "reported" {
		t.Fatalf("pricing_source = %q, want reported", ev.Billing.PricingSource)
	}
	if ev.Billing.Model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q", ev.Billing.Model)
	}
	if ev.Billing.Provider != "anthropic" {
		t.Fatalf("provider = %q, want anthropic (filled from model)", ev.Billing.Provider)
	}
}

func TestClaudeHookEstimatesCostFromTokensAndModel(t *testing.T) {
	body := []byte(`{
		"session_id":"sess",
		"hook_event_name":"PostToolUse",
		"tool_name":"mcp__llm__complete",
		"tool_response":{"usage":{"input_tokens":2000,"output_tokens":1000},"model":"claude-haiku-4-5"}
	}`)
	ev, err := buildClaudeHookEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Billing == nil || ev.Billing.CostUSD == nil {
		t.Fatalf("expected estimated cost, got %+v", ev.Billing)
	}
	if ev.Billing.PricingSource != "estimated" {
		t.Fatalf("pricing_source = %q, want estimated", ev.Billing.PricingSource)
	}
}

func TestClaudeHookFallsBackToTranscriptMetadata(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":10,"output_tokens":2}}}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"id":"m2","model":"claude-sonnet-4-6","usage":{"input_tokens":3000,"output_tokens":600}}}`,
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "PostToolUse",
		"transcript_path": transcript,
		"tool_name":       "Bash",
		"tool_response":   map[string]any{"stdout": "no metadata"},
	})
	ev, err := buildClaudeHookEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Billing == nil {
		t.Fatalf("expected billing from transcript, got nil")
	}
	if ev.Billing.Model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q (want last assistant model)", ev.Billing.Model)
	}
	if ev.Billing.TokensIn == nil || *ev.Billing.TokensIn != 3000 {
		t.Fatalf("tokens_in = %v", ev.Billing.TokensIn)
	}
	if ev.Billing.PricingSource != "estimated" {
		t.Fatalf("pricing_source = %q, want estimated", ev.Billing.PricingSource)
	}
}

func TestClaudeHookTranscriptDoesNotCapturePromptText(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(transcript, []byte(
		`{"type":"user","message":{"role":"user","content":"my secret prompt with whs_test_abcdefghijklmnop"}}`+"\n"+
			`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":1,"output_tokens":1},"content":"reply with sensitive data"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "PostToolUse",
		"transcript_path": transcript,
		"tool_name":       "Bash",
		"tool_response":   map[string]any{"stdout": "ok"},
	})
	ev, err := buildClaudeHookEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	raw, _ := json.Marshal(ev)
	if string(raw) == "" {
		t.Fatalf("empty event")
	}
	for _, leaked := range []string{"secret prompt", "sensitive data", "whs_test_abcdefghijklmnop"} {
		if contains(string(raw), leaked) {
			t.Fatalf("envelope leaked %q from transcript:\n%s", leaked, raw)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0))
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestClaudeHookStaticToolPriceFillsMissingCost(t *testing.T) {
	cfg := &config.Config{
		Cost: config.CostConfig{
			ToolPrices: []config.ToolPriceEntry{
				{ServerGlob: "playwright", ToolGlob: "browser_*", CostUSD: 0.001, Provider: "playwright", Model: "n/a"},
			},
		},
	}
	body := []byte(`{
		"session_id":"sess",
		"hook_event_name":"PostToolUse",
		"tool_name":"mcp__playwright__browser_click",
		"tool_input":{"selector":"#go"},
		"tool_response":{"content":[{"type":"text","text":"clicked"}]}
	}`)
	ev, err := buildClaudeHookEvent(body, cfg, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Billing == nil || ev.Billing.CostUSD == nil || *ev.Billing.CostUSD != 0.001 {
		t.Fatalf("expected static $0.001 cost, got %+v", ev.Billing)
	}
	if ev.Billing.PricingSource != "static" {
		t.Fatalf("pricing_source = %q, want static", ev.Billing.PricingSource)
	}
	if ev.Billing.Provider != "playwright" {
		t.Fatalf("provider = %q", ev.Billing.Provider)
	}
}
