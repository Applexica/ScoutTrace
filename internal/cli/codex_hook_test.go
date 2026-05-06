package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webhookscout/scouttrace/internal/config"
)

func TestCodexHookSnippetUsesStopOnly(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLI(t, home, "codex-hook", "snippet", "--destination", "default")
	if exit != 0 {
		t.Fatalf("snippet exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if strings.Contains(stdout, "PostToolUse") || strings.Contains(stdout, "post-tool-use") {
		t.Fatalf("Codex snippet must not install per-tool hooks that spam the live stream:\n%s", stdout)
	}
	if !strings.Contains(stdout, "\"Stop\"") || !strings.Contains(stdout, "codex-hook stop") {
		t.Fatalf("snippet missing Stop codex hook:\n%s", stdout)
	}
}

func TestCodexHookInstallRemovesLegacyScoutTraceHooks(t *testing.T) {
	home := t.TempDir()
	hooksPath := filepath.Join(t.TempDir(), "hooks.json")
	legacy := `{
  "hooks": {
    "PostToolUse": [
      {"matcher":"*","hooks":[{"type":"command","command":"/opt/homebrew/bin/scouttrace --home /Users/me/.scouttrace claude-hook post-tool-use --destination default --flush"}]}
    ],
    "Stop": [
      {"hooks":[{"type":"command","command":"/opt/homebrew/bin/scouttrace --home /Users/me/.scouttrace claude-hook stop --destination default --flush"}]},
      {"hooks":[{"type":"command","command":"/old/scouttrace --home /Users/me/.scouttrace codex-hook stop --destination default --flush"}]}
    ]
  }
}`
	if err := os.WriteFile(hooksPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write hooks: %v", err)
	}

	exit, stdout, stderr := runCLI(t, home, "codex-hook", "install", "--path", hooksPath, "--destination", "default")
	if exit != 0 {
		t.Fatalf("install exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	b, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	text := string(b)
	if strings.Contains(text, "PostToolUse") || strings.Contains(text, "claude-hook") || strings.Contains(text, "/old/scouttrace") {
		t.Fatalf("legacy ScoutTrace hooks were not removed:\n%s", text)
	}
	if !strings.Contains(text, "codex-hook stop") {
		t.Fatalf("codex Stop hook missing after install:\n%s", text)
	}
}

func TestCodexHookInstallProjectScopeRepairsProjectHooks(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	hooksPath := filepath.Join(projectDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	legacy := `{
  "hooks": {
    "PostToolUse": [
      {"matcher":"*","hooks":[{"type":"command","command":"/opt/homebrew/bin/scouttrace --home /Users/me/.scouttrace claude-hook post-tool-use --destination default --flush"}]}
    ],
    "Stop": [
      {"hooks":[{"type":"command","command":"/opt/homebrew/bin/scouttrace --home /Users/me/.scouttrace claude-hook stop --destination default --flush"}]}
    ]
  }
}`
	if err := os.WriteFile(hooksPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write hooks: %v", err)
	}

	exit, stdout, stderr := runCLI(
		t,
		home,
		"codex-hook",
		"install",
		"--scope",
		"project",
		"--project-dir",
		projectDir,
		"--destination",
		"default",
	)
	if exit != 0 {
		t.Fatalf("install exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	b, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	text := string(b)
	if strings.Contains(text, "PostToolUse") || strings.Contains(text, "claude-hook") {
		t.Fatalf("project legacy ScoutTrace hooks were not removed:\n%s", text)
	}
	if !strings.Contains(text, "codex-hook stop") {
		t.Fatalf("project codex Stop hook missing after install:\n%s", text)
	}
	if !strings.Contains(stdout, hooksPath) {
		t.Fatalf("install output did not mention project hooks path:\n%s", stdout)
	}
}

func TestCodexHookStopBuildsToolAndLLMTurnEventsFromSession(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	sessionPath := filepath.Join(dir, "rollout-2026-05-06T16-22-51-session-1.jsonl")
	session := strings.Join([]string{
		`{"timestamp":"2026-05-06T20:26:00.000Z","type":"session_meta","payload":{"id":"session-1","cli_version":"0.128.0-alpha.1","model_provider":"openai"}}`,
		`{"timestamp":"2026-05-06T20:26:01.000Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		`{"timestamp":"2026-05-06T20:26:02.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}","call_id":"call_1"}}`,
		`{"timestamp":"2026-05-06T20:26:03.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"{\"stdout\":\"/tmp\\n\",\"exit_code\":0}"}}`,
		`{"timestamp":"2026-05-06T20:26:04.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050}}}}`,
		"",
	}, "\n")
	if err := os.WriteFile(sessionPath, []byte(session), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"session_id":      "session-1",
		"hook_event_name": "Stop",
		"transcript_path": sessionPath,
	})
	events, err := buildCodexStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("buildCodexStopEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Source.Host != "codex" || events[0].Tool.Name != "exec_command" {
		t.Fatalf("tool event = host %q tool %q", events[0].Source.Host, events[0].Tool.Name)
	}
	if events[1].Tool.Name != "llm_turn" {
		t.Fatalf("second tool = %q, want llm_turn", events[1].Tool.Name)
	}
	if events[1].Billing == nil {
		t.Fatalf("llm_turn billing nil")
	}
	if events[1].Billing.Model != "gpt-5.5" || events[1].Billing.Provider != "openai" {
		t.Fatalf("llm_turn model/provider = %+v", events[1].Billing)
	}
	if events[1].Billing.TokensIn == nil || *events[1].Billing.TokensIn != 1000 {
		t.Fatalf("tokens_in = %+v, want 1000", events[1].Billing.TokensIn)
	}
	if events[1].Billing.TokensOut == nil || *events[1].Billing.TokensOut != 50 {
		t.Fatalf("tokens_out = %+v, want 50", events[1].Billing.TokensOut)
	}
	if events[1].Billing.CostUSD == nil || *events[1].Billing.CostUSD <= 0 {
		t.Fatalf("cost_usd = %+v, want positive estimate", events[1].Billing.CostUSD)
	}
}

func TestCodexHookStopCursorSkipsAlreadyProcessedRows(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	sessionPath := filepath.Join(dir, "session.jsonl")
	content := `{"timestamp":"2026-05-06T20:26:01.000Z","type":"turn_context","payload":{"model":"gpt-5.5"}}` + "\n" +
		`{"timestamp":"2026-05-06T20:26:04.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":0,"output_tokens":50,"total_tokens":1050}}}}` + "\n"
	if err := os.WriteFile(sessionPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"session_id":      "session-1",
		"hook_event_name": "Stop",
		"transcript_path": sessionPath,
	})
	first, err := buildCodexStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("first buildCodexStopEvents: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first len = %d, want 1", len(first))
	}
	second, err := buildCodexStopEvents(body, &config.Config{}, "", home)
	if err != nil {
		t.Fatalf("second buildCodexStopEvents: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second len = %d, want 0 after cursor", len(second))
	}
}
