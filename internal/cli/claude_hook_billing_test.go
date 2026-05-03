package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	home := t.TempDir()
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
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
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

func TestClaudeHookStopBuildsOneEventPerAssistantLine(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	// Effective input totals along the way: 10, 777, 3. Incremental deltas are
	// 10 (first), 777-10=767 (second), and on the third the running total
	// shrinks from 777 to 3 — a context reset — so we bill the current 3.
	content := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"again"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":777,"output_tokens":42}}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3 (one per assistant line)", len(events))
	}
	wantModels := []string{"claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-7"}
	wantTokensIn := []int{10, 767, 3}
	wantTokensOut := []int{5, 42, 1}
	seenIDs := map[string]bool{}
	for i, ev := range events {
		if ev.Billing == nil {
			t.Fatalf("event %d billing nil", i)
		}
		if ev.Billing.Model != wantModels[i] {
			t.Fatalf("event %d model = %q, want %q", i, ev.Billing.Model, wantModels[i])
		}
		if ev.Billing.TokensIn == nil || *ev.Billing.TokensIn != wantTokensIn[i] {
			got := -1
			if ev.Billing.TokensIn != nil {
				got = *ev.Billing.TokensIn
			}
			t.Fatalf("event %d tokens_in = %d, want %d", i, got, wantTokensIn[i])
		}
		if ev.Billing.TokensOut == nil || *ev.Billing.TokensOut != wantTokensOut[i] {
			t.Fatalf("event %d tokens_out = %v, want %d", i, ev.Billing.TokensOut, wantTokensOut[i])
		}
		if seenIDs[ev.ID] {
			t.Fatalf("duplicate event ID %q across the batch", ev.ID)
		}
		seenIDs[ev.ID] = true
	}
}

func TestClaudeHookStopSumsClaudeCacheTokensIntoTokensIn(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	// Real Claude tool-loop transcripts often show input_tokens=1 with the
	// bulk of context tokens billed under cache_creation/cache_read fields.
	// The displayed/billed input total must include all three.
	content := `{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":5000,"cache_read_input_tokens":3000,"output_tokens":214}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Billing == nil || ev.Billing.TokensIn == nil {
		t.Fatalf("billing or tokens_in nil: %+v", ev.Billing)
	}
	want := 1 + 5000 + 3000
	if *ev.Billing.TokensIn != want {
		t.Fatalf("tokens_in = %d, want %d (input + cache_creation + cache_read)", *ev.Billing.TokensIn, want)
	}
	if ev.Billing.TokensOut == nil || *ev.Billing.TokensOut != 214 {
		t.Fatalf("tokens_out = %v, want 214", ev.Billing.TokensOut)
	}
}

func TestClaudeHookStopCursorSkipsAlreadyProcessedLines(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	first := `{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(first), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events1, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("first buildClaudeStopEvents: %v", err)
	}
	if len(events1) != 1 {
		t.Fatalf("first call len(events) = %d, want 1", len(events1))
	}

	// Calling again with no new lines must return zero events.
	events2, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("second buildClaudeStopEvents: %v", err)
	}
	if len(events2) != 0 {
		t.Fatalf("second call (no new lines) len(events) = %d, want 0", len(events2))
	}

	// Append two new assistant lines and one user line; expect exactly 2 new events.
	more := `{"type":"user","message":{"content":"continue"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":50,"output_tokens":11}}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":99,"output_tokens":7}}}` + "\n"
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("append open: %v", err)
	}
	if _, err := f.WriteString(more); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	events3, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("third buildClaudeStopEvents: %v", err)
	}
	if len(events3) != 2 {
		t.Fatalf("third call len(events) = %d, want 2 (only new assistant lines)", len(events3))
	}
	if events3[0].Billing == nil || events3[0].Billing.Model != "claude-sonnet-4-6" {
		t.Fatalf("third call event[0] model = %+v, want claude-sonnet-4-6", events3[0].Billing)
	}
	if events3[1].Billing == nil || events3[1].Billing.Model != "claude-opus-4-7" {
		t.Fatalf("third call event[1] model = %+v, want claude-opus-4-7", events3[1].Billing)
	}
}

func TestClaudeHookStopReturnsEmptyWhenNoAssistantUsage(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events when transcript has no assistant usage, got %d", len(events))
	}
}

func TestClaudeHookStopMultiLineDoesNotLeakPromptOrAssistantContent(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"my secret prompt with whs_test_abcdefghijklmnop"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5},"content":"first reply with sensitive data"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"another secret PROMPT_TWO_LEAK"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":1,"cache_read_input_tokens":2000,"output_tokens":3},"content":"second reply with REPLY_TWO_LEAK"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	for i, ev := range events {
		raw, _ := json.Marshal(ev)
		for _, leaked := range []string{
			"secret prompt", "sensitive data",
			"whs_test_abcdefghijklmnop",
			"PROMPT_TWO_LEAK", "REPLY_TWO_LEAK",
			"another secret",
		} {
			if contains(string(raw), leaked) {
				t.Fatalf("event %d leaked %q from transcript:\n%s", i, leaked, raw)
			}
		}
	}
}

func TestClaudeHookStopCLIJSONOutputIncludesIDsAndCount(t *testing.T) {
	home := t.TempDir()
	exit, _, stderr := runCLI(t, home, "init", "--yes", "--hosts", "none", "--destination", "stdout")
	if exit != 0 {
		t.Fatalf("init exit=%d stderr=%s", exit, stderr)
	}
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	content := `{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":1,"cache_creation_input_tokens":4000,"output_tokens":11}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"session_id":      "sess-json",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	exit, stdout, stderr := runCLIWithInput(t, home, string(payload), "--json", "claude-hook", "stop", "--destination", "default")
	if exit != 0 {
		t.Fatalf("stop exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout)
	}
	if out["destination"] != "default" {
		t.Fatalf("destination = %v, want default", out["destination"])
	}
	count, ok := out["count"].(float64)
	if !ok {
		t.Fatalf("count missing or not numeric: %#v", out["count"])
	}
	if int(count) != 2 {
		t.Fatalf("count = %v, want 2", count)
	}
	idsRaw, ok := out["ids"].([]any)
	if !ok {
		t.Fatalf("ids missing or not array: %#v", out["ids"])
	}
	if len(idsRaw) != 2 {
		t.Fatalf("len(ids) = %d, want 2", len(idsRaw))
	}
	for i, v := range idsRaw {
		s, _ := v.(string)
		if s == "" {
			t.Fatalf("ids[%d] empty: %#v", i, v)
		}
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

// TestClaudeHookPostToolUseScanCollectsLLMTurnsAndStopDedupesThem covers the
// core fix for v0.1.11: long Claude tool loops would only emit llm_turn events
// from the Stop hook, so WebhookScout received them all in a single burst at
// the very end. The fix scans the transcript on every PostToolUse so events
// stream out in real time, with Stop as a no-op catch-up that reuses the same
// cursor and therefore must not duplicate already-emitted turns.
func TestClaudeHookPostToolUseScanCollectsLLMTurnsAndStopDedupesThem(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")

	// Mid-loop transcript state after the first assistant turn lands.
	first := `{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(first), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// PostToolUse scan #1: should pick up the new turn.
	postEvents1, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("post #1: %v", err)
	}
	if len(postEvents1) != 1 {
		t.Fatalf("post #1 len = %d, want 1", len(postEvents1))
	}

	// A second PostToolUse fires before any new assistant turn lands. The
	// cursor must remember what we already emitted so we do not double-bill.
	postEvents2, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("post #2: %v", err)
	}
	if len(postEvents2) != 0 {
		t.Fatalf("post #2 len = %d, want 0 (dedupe via cursor)", len(postEvents2))
	}

	// A second assistant turn appends; PostToolUse scan #3 picks up only that.
	more := `{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_read_input_tokens":50,"output_tokens":7}}}` + "\n"
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("append open: %v", err)
	}
	if _, err := f.WriteString(more); err != nil {
		t.Fatalf("append write: %v", err)
	}
	_ = f.Close()

	postEvents3, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("post #3: %v", err)
	}
	if len(postEvents3) != 1 {
		t.Fatalf("post #3 len = %d, want 1", len(postEvents3))
	}

	// Stop fires last as the final catch-up. The transcript holds nothing
	// new, so the Stop hook must yield zero events — no double-billing.
	stopBody, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	stopEvents, err := buildClaudeStopEvents(stopBody, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(stopEvents) != 0 {
		t.Fatalf("stop len = %d, want 0 (PostToolUse already drained)", len(stopEvents))
	}
}

// TestClaudeHookLLMTurnTokensAreIncrementalNotCumulative pins down the
// per-call token math: assistant turns in a Claude tool loop report the
// cumulative effective input each time (input + cache_creation + cache_read),
// so emitting that raw figure as tokens_in for every turn re-bills the shared
// context once per turn. The fix subtracts the prior cumulative total
// persisted in the cursor.
func TestClaudeHookLLMTurnTokensAreIncrementalNotCumulative(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")

	// Two assistant turns: first reports a cumulative effective input of
	// 68000 (1 input + 67999 cache_creation), the second 68500 (1 input +
	// 500 cache_creation + 67999 cache_read). The expected event tokens_in
	// values are 68000 and 500.
	content := `{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":67999,"cache_read_input_tokens":0,"output_tokens":42}}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":500,"cache_read_input_tokens":67999,"output_tokens":17}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	wantTokensIn := []int{68000, 500}
	wantTokensOut := []int{42, 17}
	for i, ev := range events {
		if ev.Billing == nil {
			t.Fatalf("event %d billing nil", i)
		}
		if ev.Billing.TokensIn == nil || *ev.Billing.TokensIn != wantTokensIn[i] {
			got := -1
			if ev.Billing.TokensIn != nil {
				got = *ev.Billing.TokensIn
			}
			t.Fatalf("event %d tokens_in = %d, want %d", i, got, wantTokensIn[i])
		}
		if ev.Billing.TokensOut == nil || *ev.Billing.TokensOut != wantTokensOut[i] {
			t.Fatalf("event %d tokens_out = %v, want %d", i, ev.Billing.TokensOut, wantTokensOut[i])
		}
		// Cost must be derived from the incremental tokens: the second
		// event's estimated cost should be strictly less than the first
		// because tokens_in dropped from 68000 to 500 (with output also
		// shrinking from 42 to 17). If the bug were present both events
		// would have ~equal cost.
		if ev.Billing.CostUSD == nil {
			t.Fatalf("event %d cost_usd nil; expected estimate", i)
		}
	}
	if *events[1].Billing.CostUSD >= *events[0].Billing.CostUSD {
		t.Fatalf("incremental cost regression: event[1].cost (%f) should be < event[0].cost (%f)",
			*events[1].Billing.CostUSD, *events[0].Billing.CostUSD)
	}
}

func TestClaudeHookLLMTurnDedupesRepeatedAssistantTranscriptRows(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")

	first := `{"type":"assistant","requestId":"req_1","message":{"id":"msg_1","model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":67999,"cache_read_input_tokens":0,"output_tokens":42}}}` + "\n"
	duplicate := `{"type":"assistant","requestId":"req_1","message":{"id":"msg_1","model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":67999,"cache_read_input_tokens":0,"output_tokens":42}}}` + "\n"
	second := `{"type":"assistant","requestId":"req_2","message":{"id":"msg_2","model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":500,"cache_read_input_tokens":67999,"output_tokens":17}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(first), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	events1, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if len(events1) != 1 {
		t.Fatalf("first scan len = %d, want 1", len(events1))
	}
	if events1[0].Billing == nil || events1[0].Billing.TokensIn == nil || *events1[0].Billing.TokensIn != 68000 {
		t.Fatalf("first scan tokens_in = %+v, want 68000", events1[0].Billing)
	}

	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("append open: %v", err)
	}
	if _, err := f.WriteString(duplicate + second); err != nil {
		t.Fatalf("append write: %v", err)
	}
	_ = f.Close()

	events2, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(events2) != 1 {
		t.Fatalf("second scan len = %d, want 1 (duplicate assistant row skipped)", len(events2))
	}
	if events2[0].Billing == nil || events2[0].Billing.TokensIn == nil || *events2[0].Billing.TokensIn != 500 {
		t.Fatalf("second scan tokens_in = %+v, want 500 not repeated 68000", events2[0].Billing)
	}
	if events2[0].Billing.TokensOut == nil || *events2[0].Billing.TokensOut != 17 {
		t.Fatalf("second scan tokens_out = %+v, want 17", events2[0].Billing)
	}
}

func TestClaudeHookLLMTurnEqualEffectiveInputDoesNotRebillFullContext(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")

	content := `{"type":"assistant","requestId":"req_1","message":{"id":"msg_1","model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":999,"output_tokens":42}}}` + "\n" +
		`{"type":"assistant","requestId":"req_2","message":{"id":"msg_2","model":"claude-opus-4-7","usage":{"input_tokens":1,"cache_creation_input_tokens":999,"output_tokens":17}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	events, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeLLMTurnEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[1].Billing == nil || events[1].Billing.TokensIn == nil || *events[1].Billing.TokensIn != 0 {
		t.Fatalf("second equal-effective-input tokens_in = %+v, want 0 not repeated 1000", events[1].Billing)
	}
	if events[1].Billing.TokensOut == nil || *events[1].Billing.TokensOut != 17 {
		t.Fatalf("second tokens_out = %+v, want 17", events[1].Billing)
	}
}

// TestClaudeHookCursorBackwardCompatLegacyIntegerOffset verifies that a
// cursor file written by v0.1.11 (a bare decimal byte offset) is still
// honored after upgrading to the JSON {offset, prior_effective_in} shape.
// Without this, an upgrade would re-emit the entire transcript on the first
// hook fire.
func TestClaudeHookCursorBackwardCompatLegacyIntegerOffset(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	first := `{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	second := `{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":20,"output_tokens":7}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(first+second), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Pre-seed a legacy cursor: a plain integer byte offset matching the
	// length of the first line, exactly what v0.1.11 wrote.
	cursorPath := claudeTranscriptCursorPath(home, "sess", transcript)
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		t.Fatalf("mkdir cursor: %v", err)
	}
	legacy := []byte(strconv.FormatInt(int64(len(first)), 10))
	if err := os.WriteFile(cursorPath, legacy, 0o600); err != nil {
		t.Fatalf("write legacy cursor: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	})
	events, err := buildClaudeStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeStopEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("legacy cursor: len(events) = %d, want 1 (only the second line is new)", len(events))
	}
	if events[0].Billing == nil || events[0].Billing.Model != "claude-sonnet-4-6" {
		t.Fatalf("legacy cursor: model = %+v, want claude-sonnet-4-6", events[0].Billing)
	}
	// Because the legacy cursor had no prior_effective_in, the second turn
	// is billed against a baseline of 0, so the full 20 tokens are charged.
	if events[0].Billing.TokensIn == nil || *events[0].Billing.TokensIn != 20 {
		got := -1
		if events[0].Billing.TokensIn != nil {
			got = *events[0].Billing.TokensIn
		}
		t.Fatalf("legacy cursor: tokens_in = %d, want 20 (prior=0 from legacy file)", got)
	}

	// After this run, the cursor must have been rewritten in the new JSON
	// shape so subsequent fires get incremental deltas.
	raw, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	var cur claudeTranscriptCursor
	if err := json.Unmarshal(raw, &cur); err != nil {
		t.Fatalf("cursor not rewritten as JSON: %s", raw)
	}
	if cur.PriorEffectiveIn != 20 {
		t.Fatalf("cursor.prior_effective_in = %d, want 20", cur.PriorEffectiveIn)
	}
}

// TestClaudeHookPostToolUseScanDoesNotLeakTranscriptContent covers the
// privacy guarantee on the new PostToolUse-driven path: even though
// PostToolUse now scans the transcript directly, no user prompt or assistant
// content can appear in any emitted event envelope.
func TestClaudeHookPostToolUseScanDoesNotLeakTranscriptContent(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"my secret prompt with whs_test_abcdefghijklmnop"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5},"content":"first reply with sensitive data"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"PROMPT_TWO_LEAK"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":1,"cache_read_input_tokens":2000,"output_tokens":3},"content":"REPLY_TWO_LEAK"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	events, err := buildClaudeLLMTurnEvents(transcript, "sess", &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildClaudeLLMTurnEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	for i, ev := range events {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), transcript) {
			t.Fatalf("event %d leaks transcript path:\n%s", i, raw)
		}
		for _, leaked := range []string{
			"secret prompt", "sensitive data",
			"whs_test_abcdefghijklmnop",
			"PROMPT_TWO_LEAK", "REPLY_TWO_LEAK",
		} {
			if contains(string(raw), leaked) {
				t.Fatalf("event %d leaked %q from transcript:\n%s", i, leaked, raw)
			}
		}
	}
}
