package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestClaudeHookDoesNotMisattributeTranscriptUsageToToolEvent(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	content := `{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":214}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "PostToolUse",
		"transcript_path": transcript,
		"tool_name":       "Bash",
		"tool_response":   map[string]any{"stdout": "no billing metadata"},
	})
	ev, err := buildClaudeHookEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Billing != nil {
		t.Fatalf("transcript usage/model must not be attached to individual tool event billing, got %+v", ev.Billing)
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

func TestClaudeHookStopBuildsLLMTurnEventFromTranscript(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"my secret prompt with whs_test_abcdefghijklmnop"}}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":2000,"output_tokens":1000},"content":"reply with sensitive data"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
		"cwd":             dir,
	})
	ev, err := buildClaudeStopEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("buildClaudeStopEvent: %v", err)
	}
	if ev == nil {
		t.Fatalf("expected event, got nil")
	}
	if ev.Source.Kind != "claude_code_hook" {
		t.Fatalf("source.kind = %q, want claude_code_hook", ev.Source.Kind)
	}
	if ev.Server.Name != "claude-code" {
		t.Fatalf("server.name = %q, want claude-code", ev.Server.Name)
	}
	if ev.Tool.Name != "llm_turn" {
		t.Fatalf("tool.name = %q, want llm_turn", ev.Tool.Name)
	}
	if !ev.Response.OK {
		t.Fatalf("response.ok = false, want true")
	}
	if len(ev.Request.Args) != 0 {
		t.Fatalf("request.args must be empty, got %s", ev.Request.Args)
	}
	if len(ev.Response.Result) != 0 {
		t.Fatalf("response.result must be empty, got %s", ev.Response.Result)
	}
	if ev.Billing == nil {
		t.Fatalf("expected billing block, got nil")
	}
	if ev.Billing.Model != "claude-opus-4-7" {
		t.Fatalf("billing.model = %q", ev.Billing.Model)
	}
	if ev.Billing.TokensIn == nil || *ev.Billing.TokensIn != 2000 {
		t.Fatalf("billing.tokens_in = %v, want 2000", ev.Billing.TokensIn)
	}
	if ev.Billing.TokensOut == nil || *ev.Billing.TokensOut != 1000 {
		t.Fatalf("billing.tokens_out = %v, want 1000", ev.Billing.TokensOut)
	}
	if ev.Billing.CostUSD == nil {
		t.Fatalf("billing.cost_usd nil; expected estimate")
	}
	if ev.Billing.PricingSource != "estimated" {
		t.Fatalf("billing.pricing_source = %q, want estimated", ev.Billing.PricingSource)
	}

	raw, _ := json.Marshal(ev)
	for _, leaked := range []string{"secret prompt", "sensitive data", "whs_test_abcdefghijklmnop"} {
		if contains(string(raw), leaked) {
			t.Fatalf("stop event leaked %q from transcript:\n%s", leaked, raw)
		}
	}
}

func TestClaudeHookStopUsesLatestAssistantUsage(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	content := `{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":777,"output_tokens":42}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	ev, err := buildClaudeStopEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("buildClaudeStopEvent: %v", err)
	}
	if ev == nil {
		t.Fatalf("expected event, got nil")
	}
	if ev.Billing == nil || ev.Billing.Model != "claude-sonnet-4-6" {
		t.Fatalf("expected last assistant model claude-sonnet-4-6, got %+v", ev.Billing)
	}
	if ev.Billing.TokensIn == nil || *ev.Billing.TokensIn != 777 {
		t.Fatalf("expected last-line input_tokens=777, got %v", ev.Billing.TokensIn)
	}
}

func TestClaudeHookStopReturnsNilWhenNoAssistantUsage(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	ev, err := buildClaudeStopEvent(body, &config.Config{}, "")
	if err != nil {
		t.Fatalf("buildClaudeStopEvent: %v", err)
	}
	if ev != nil {
		t.Fatalf("expected nil event when transcript has no assistant usage, got %+v", ev)
	}
}

func TestClaudeHookInstallWritesPostToolUseAndStop(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	exit, stdout, stderr := runCLI(t, home, "claude-hook", "install", "--scope", "local", "--project-dir", project, "--destination", "default")
	if exit != 0 {
		t.Fatalf("install exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	settingsPath := filepath.Join(project, ".claude", "settings.local.json")
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "PostToolUse") || !strings.Contains(text, "claude-hook post-tool-use") {
		t.Fatalf("settings missing PostToolUse hook:\n%s", text)
	}
	if !strings.Contains(text, "\"Stop\"") || !strings.Contains(text, "claude-hook stop") {
		t.Fatalf("settings missing Stop hook:\n%s", text)
	}
	exit, _, stderr = runCLI(t, home, "claude-hook", "install", "--scope", "local", "--project-dir", project, "--destination", "default")
	if exit != 0 {
		t.Fatalf("second install exit=%d stderr=%s", exit, stderr)
	}
	b2, _ := os.ReadFile(settingsPath)
	if strings.Count(string(b2), "claude-hook post-tool-use") != 1 {
		t.Fatalf("post-tool-use hook duplicated:\n%s", string(b2))
	}
	if strings.Count(string(b2), "claude-hook stop") != 1 {
		t.Fatalf("stop hook duplicated:\n%s", string(b2))
	}
}

func TestClaudeHookSnippetIncludesPostToolUseAndStop(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLI(t, home, "claude-hook", "snippet", "--destination", "default")
	if exit != 0 {
		t.Fatalf("snippet exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "PostToolUse") || !strings.Contains(stdout, "claude-hook post-tool-use") {
		t.Fatalf("snippet missing PostToolUse:\n%s", stdout)
	}
	if !strings.Contains(stdout, "\"Stop\"") || !strings.Contains(stdout, "claude-hook stop") {
		t.Fatalf("snippet missing Stop:\n%s", stdout)
	}
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
