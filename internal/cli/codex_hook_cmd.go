package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/webhookscout/scouttrace/internal/billing"
	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/version"
)

// CmdCodexHook integrates ScoutTrace with Codex hooks. Unlike Claude Code,
// Codex currently surfaces every PostToolUse hook in the live stream, so this
// command intentionally installs a Stop-only hook and reconstructs telemetry
// from the Codex session JSONL at the end of the turn/session.
func CmdCodexHook(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "codex-hook: subcommand required (stop|install|snippet)")
		return 64
	}
	switch args[0] {
	case "stop":
		return codexHookStop(ctx, g, args[1:])
	case "install":
		return codexHookInstall(g, args[1:])
	case "snippet":
		return codexHookSnippet(g, args[1:])
	default:
		fmt.Fprintf(g.Stderr, "codex-hook: unknown subcommand %q\n", args[0])
		return 64
	}
}

type codexStopHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	SessionPath    string `json:"session_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

func (p codexStopHookPayload) path() string {
	for _, candidate := range []string{p.TranscriptPath, p.SessionPath} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

func codexHookStop(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("codex-hook stop", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	destFlag := fs.String("destination", "", "destination name (defaults to config default)")
	flush := fs.Bool("flush", false, "attempt a best-effort dispatch after enqueue")
	failClosed := fs.Bool("fail-closed", false, "return non-zero if capture or flush fails")
	hostVersion := fs.String("host-version", "", "optional Codex version label")
	sessionPath := fs.String("session-path", "", "optional Codex session JSONL path when the hook payload omits it")
	if err := fs.Parse(args); err != nil {
		return 64
	}

	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		if *failClosed {
			return 1
		}
		return 0
	}
	dest := *destFlag
	if dest == "" {
		dest = c.DefaultDestination
	}
	if dest == "" {
		dest = "default"
	}

	body, err := readStdin(g)
	if err != nil || len(strings.TrimSpace(string(body))) == 0 {
		fmt.Fprintln(g.Stderr, "codex-hook: empty or unreadable hook payload")
		if *failClosed {
			return 1
		}
		return 0
	}
	if *sessionPath != "" {
		body, err = codexHookPayloadWithSessionPath(body, *sessionPath)
		if err != nil {
			fmt.Fprintln(g.Stderr, "codex-hook:", err)
			if *failClosed {
				return 1
			}
			return 0
		}
	}

	events, err := buildCodexStopEvents(body, c, *hostVersion, g.Home)
	if err != nil {
		fmt.Fprintln(g.Stderr, "codex-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	ids := make([]string, 0, len(events))
	if len(events) == 0 {
		if g.JSON {
			_ = printJSON(g.Stdout, map[string]any{"ids": ids, "count": 0, "destination": dest}, false)
		} else if g.Verbose > 0 {
			fmt.Fprintln(g.Stderr, "codex-hook stop: no new Codex telemetry rows; skipping")
		}
		return 0
	}

	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, "codex-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	for _, env := range events {
		payload, err := json.Marshal(env)
		if err != nil {
			fmt.Fprintln(g.Stderr, "codex-hook:", err)
			if *failClosed {
				return 1
			}
			continue
		}
		if err := q.Enqueue(env.ID, dest, payload); err != nil {
			fmt.Fprintln(g.Stderr, "codex-hook:", err)
			if *failClosed {
				return 1
			}
			continue
		}
		ids = append(ids, env.ID)
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]any{"ids": ids, "count": len(ids), "destination": dest}, false)
	} else if g.Verbose > 0 {
		fmt.Fprintf(g.Stderr, "codex-hook: enqueued %d event(s) -> %s\n", len(ids), dest)
	}

	if *flush {
		if err := flushQueueOnce(ctx, g, c, q, dest, 3*time.Second); err != nil {
			fmt.Fprintln(g.Stderr, "codex-hook: flush skipped/failed:", err)
			if *failClosed {
				return 1
			}
		}
	}
	return 0
}

func codexHookPayloadWithSessionPath(body []byte, sessionPath string) ([]byte, error) {
	var hp map[string]any
	if err := json.Unmarshal(body, &hp); err != nil {
		return nil, err
	}
	hp["transcript_path"] = sessionPath
	return json.Marshal(hp)
}

func buildCodexStopEvents(body []byte, c *config.Config, hostVersion, scoutHome string) ([]*event.ToolCallEvent, error) {
	var hp codexStopHookPayload
	if err := json.Unmarshal(body, &hp); err != nil {
		return nil, fmt.Errorf("invalid Codex Stop hook JSON: %w", err)
	}
	sessionPath := hp.path()
	if sessionPath == "" {
		var err error
		sessionPath, err = findCodexSessionPath(hp.SessionID)
		if err != nil {
			return nil, err
		}
	}
	return buildCodexSessionEvents(sessionPath, hp.SessionID, c, hostVersion, scoutHome)
}

type codexSessionCursor struct {
	Offset int64 `json:"offset"`
}

type codexSessionMetadata struct {
	SessionID   string
	HostVersion string
	Model       string
	Provider    string
}

type codexPendingCall struct {
	Name      string
	Args      json.RawMessage
	CallID    string
	StartedAt time.Time
}

type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

func buildCodexSessionEvents(sessionPath, hookSessionID string, c *config.Config, hostVersion, scoutHome string) ([]*event.ToolCallEvent, error) {
	if sessionPath == "" {
		return nil, fmt.Errorf("missing Codex session path")
	}
	meta := readCodexSessionMetadata(sessionPath)
	if hookSessionID != "" {
		meta.SessionID = hookSessionID
	}
	if hostVersion != "" {
		meta.HostVersion = hostVersion
	}
	if meta.SessionID == "" {
		meta.SessionID = event.NewULID()
	}
	if meta.Provider == "" {
		meta.Provider = billing.LookupProvider(meta.Model)
	}

	cursorPath := codexSessionCursorPath(scoutHome, meta.SessionID, sessionPath)
	startCursor := readCodexSessionCursor(cursorPath)
	events, endCursor, err := readNewCodexSessionEvents(sessionPath, startCursor, meta, c, scoutHome)
	if err != nil {
		return nil, err
	}
	if endCursor.Offset > startCursor.Offset {
		_ = writeCodexSessionCursor(cursorPath, endCursor)
	}
	return events, nil
}

func readCodexSessionMetadata(path string) codexSessionMetadata {
	var meta codexSessionMetadata
	f, err := os.Open(path)
	if err != nil {
		return meta
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var row struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue
		}
		switch row.Type {
		case "session_meta":
			var p struct {
				ID            string `json:"id"`
				CLIVersion    string `json:"cli_version"`
				ModelProvider string `json:"model_provider"`
			}
			if json.Unmarshal(row.Payload, &p) == nil {
				if p.ID != "" {
					meta.SessionID = p.ID
				}
				if p.CLIVersion != "" {
					meta.HostVersion = p.CLIVersion
				}
				if p.ModelProvider != "" {
					meta.Provider = p.ModelProvider
				}
			}
		case "turn_context":
			var p struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(row.Payload, &p) == nil && p.Model != "" {
				meta.Model = p.Model
			}
		}
	}
	return meta
}

func readNewCodexSessionEvents(path string, start codexSessionCursor, meta codexSessionMetadata, c *config.Config, scoutHome string) ([]*event.ToolCallEvent, codexSessionCursor, error) {
	end := start
	f, err := os.Open(path)
	if err != nil {
		return nil, end, fmt.Errorf("open Codex session: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, end, fmt.Errorf("stat Codex session: %w", err)
	}
	if start.Offset > info.Size() {
		start = codexSessionCursor{}
		end = start
	}
	if _, err := f.Seek(start.Offset, io.SeekStart); err != nil {
		return nil, end, fmt.Errorf("seek Codex session: %w", err)
	}

	r := bufio.NewReader(f)
	end.Offset = start.Offset
	pending := map[string]codexPendingCall{}
	var out []*event.ToolCallEvent
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, end, fmt.Errorf("read Codex session: %w", err)
		}
		end.Offset += int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var row struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(trimmed), &row); err != nil {
			continue
		}
		ts := parseCodexTimestamp(row.Timestamp)
		switch row.Type {
		case "response_item":
			ev := handleCodexResponseItem(row.Payload, ts, pending, meta, c)
			if ev != nil {
				out = append(out, ev)
			}
		case "event_msg":
			ev := buildCodexLLMTurnEvent(row.Payload, ts, meta, c, scoutHome)
			if ev != nil {
				out = append(out, ev)
			}
		}
	}
	return out, end, nil
}

func handleCodexResponseItem(payload json.RawMessage, ts time.Time, pending map[string]codexPendingCall, meta codexSessionMetadata, c *config.Config) *event.ToolCallEvent {
	var p struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		CallID    string `json:"call_id"`
		Output    string `json:"output"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	switch p.Type {
	case "function_call":
		if p.CallID == "" || p.Name == "" {
			return nil
		}
		pending[p.CallID] = codexPendingCall{
			Name:      p.Name,
			Args:      rawFromString(p.Arguments),
			CallID:    p.CallID,
			StartedAt: ts,
		}
	case "function_call_output":
		call, ok := pending[p.CallID]
		if !ok {
			return nil
		}
		delete(pending, p.CallID)
		return buildCodexToolEvent(call, p.Output, ts, meta, c)
	}
	return nil
}

func buildCodexToolEvent(call codexPendingCall, output string, endedAt time.Time, meta codexSessionMetadata, c *config.Config) *event.ToolCallEvent {
	startedAt := call.StartedAt
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	latency := endedAt.Sub(startedAt).Milliseconds()
	if latency < 0 {
		latency = 0
	}
	result := rawFromString(output)
	ev := &event.ToolCallEvent{
		ID:         event.NewULIDAt(endedAt),
		Schema:     event.SchemaVersion,
		CapturedAt: endedAt,
		SessionID:  meta.SessionID,
		TraceID:    event.NewTraceID(),
		SpanID:     event.NewSpanID(),
		Source: event.SourceBlock{
			Kind:              "codex_hook",
			Host:              "codex",
			HostVersion:       meta.HostVersion,
			ScoutTraceVersion: version.Version,
		},
		Server: event.ServerBlock{Name: "codex"},
		Tool:   event.ToolBlock{Name: call.Name},
		Request: event.RequestBlock{
			JSONRPCID:         call.CallID,
			Args:              call.Args,
			ArgsBytesOriginal: len(call.Args),
		},
		Response: event.ResponseBlock{
			OK:                  codexOutputOK(result),
			Result:              result,
			ResultBytesOriginal: len(result),
		},
		Timing: event.TimingBlock{StartedAt: startedAt, EndedAt: endedAt, LatencyMS: latency},
	}
	final, err := finalizeWithRedaction(ev, c)
	if err != nil {
		return nil
	}
	return final
}

func buildCodexLLMTurnEvent(payload json.RawMessage, ts time.Time, meta codexSessionMetadata, c *config.Config, scoutHome string) *event.ToolCallEvent {
	var p struct {
		Type string `json:"type"`
		Info struct {
			LastTokenUsage codexTokenUsage `json:"last_token_usage"`
		} `json:"info"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Type != "token_count" {
		return nil
	}
	u := p.Info.LastTokenUsage
	if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.TotalTokens <= 0 {
		return nil
	}
	if meta.Model == "" {
		return nil
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	tokensIn := u.InputTokens
	tokensOut := u.OutputTokens
	provider := meta.Provider
	if provider == "" {
		provider = billing.LookupProvider(meta.Model)
	}
	bb := event.BillingBlock{
		TokensIn:  &tokensIn,
		TokensOut: &tokensOut,
		Model:     meta.Model,
		Provider:  provider,
	}
	live, liveSource := liveLookup(c, scoutHome)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	nonCached := u.InputTokens - u.CachedInputTokens
	if nonCached < 0 {
		nonCached = 0
	}
	usage := billing.Usage{Input: nonCached, CacheRead: u.CachedInputTokens, Output: u.OutputTokens}
	if cost, source, ok := billing.EstimateUsage(ctx, live, liveSource, meta.Model, usage); ok {
		bb.CostUSD = &cost
		bb.PricingSource = source
		if bb.Provider == "" {
			bb.Provider = billing.LookupProvider(meta.Model)
		}
	}
	ev := &event.ToolCallEvent{
		ID:         event.NewULIDAt(ts),
		Schema:     event.SchemaVersion,
		CapturedAt: ts,
		SessionID:  meta.SessionID,
		TraceID:    event.NewTraceID(),
		SpanID:     event.NewSpanID(),
		Source: event.SourceBlock{
			Kind:              "codex_hook",
			Host:              "codex",
			HostVersion:       meta.HostVersion,
			ScoutTraceVersion: version.Version,
		},
		Server:   event.ServerBlock{Name: "codex"},
		Tool:     event.ToolBlock{Name: "llm_turn"},
		Request:  event.RequestBlock{JSONRPCID: "codex-token-count"},
		Response: event.ResponseBlock{OK: true},
		Timing:   event.TimingBlock{StartedAt: ts, EndedAt: ts, LatencyMS: 0},
		Billing:  &bb,
	}
	final, err := finalizeWithRedaction(ev, c)
	if err != nil {
		return nil
	}
	return final
}

func codexOutputOK(raw json.RawMessage) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return true
	}
	if v, ok := m["exit_code"]; ok {
		if n, ok := jsonNumberToInt(v); ok && n != 0 {
			return false
		}
	}
	if v, ok := m["exitCode"]; ok {
		if n, ok := jsonNumberToInt(v); ok && n != 0 {
			return false
		}
	}
	if v, ok := m["error"]; ok && v != nil && fmt.Sprint(v) != "" {
		return false
	}
	return true
}

func rawFromString(s string) json.RawMessage {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	b, _ := json.Marshal(s)
	return b
}

func parseCodexTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func codexSessionCursorPath(scoutHome, sessionID, sessionPath string) string {
	if scoutHome == "" {
		return ""
	}
	return filepath.Join(scoutHome, "codex_hook", "cursors", safeCursorName(sessionID+"::"+sessionPath)+".cursor")
}

func safeCursorName(s string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	out := replacer.Replace(s)
	if len(out) > 180 {
		out = out[len(out)-180:]
	}
	return out
}

func readCodexSessionCursor(path string) codexSessionCursor {
	if path == "" {
		return codexSessionCursor{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return codexSessionCursor{}
	}
	var cur codexSessionCursor
	if json.Unmarshal(b, &cur) == nil && cur.Offset >= 0 {
		return cur
	}
	return codexSessionCursor{}
}

func writeCodexSessionCursor(path string, cur codexSessionCursor) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func findCodexSessionPath(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("missing transcript_path/session_path and session_id")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	pattern := filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*"+sessionID+"*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("Codex session %q not found under ~/.codex/sessions", sessionID)
	}
	sort.Slice(matches, func(i, j int) bool {
		ii, errI := os.Stat(matches[i])
		jj, errJ := os.Stat(matches[j])
		if errI != nil || errJ != nil {
			return matches[i] > matches[j]
		}
		return ii.ModTime().After(jj.ModTime())
	})
	return matches[0], nil
}

func codexHookCommand(g *Globals, flush bool, destination string) string {
	bin := g.ScoutBinary
	if bin == "" {
		bin = "scouttrace"
	}
	parts := []string{shellQuote(bin)}
	if g.Home != "" {
		parts = append(parts, "--home", shellQuote(g.Home))
	}
	parts = append(parts, "codex-hook", "stop")
	if destination != "" {
		parts = append(parts, "--destination", shellQuote(destination))
	}
	if flush {
		parts = append(parts, "--flush")
	}
	return strings.Join(parts, " ")
}

func codexHookSnippet(g *Globals, args []string) int {
	fs := flag.NewFlagSet("codex-hook snippet", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	dest := fs.String("destination", "", "destination name")
	flush := fs.Bool("flush", true, "include --flush in the hook command")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	stopCmd := codexHookCommand(g, *flush, *dest)
	snippet := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": stopCmd},
					},
				},
			},
		},
	}
	_ = printJSON(g.Stdout, snippet, true)
	return 0
}

func codexHookInstall(g *Globals, args []string) int {
	fs := flag.NewFlagSet("codex-hook install", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	path := fs.String("path", "", "Codex hooks.json path (default ~/.codex/hooks.json)")
	dest := fs.String("destination", "", "destination name")
	flush := fs.Bool("flush", true, "attempt dispatch from the hook after enqueue")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	hooksPath := *path
	if hooksPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
		hooksPath = filepath.Join(home, ".codex", "hooks.json")
	}
	stopCmd := codexHookCommand(g, *flush, *dest)
	removed, err := installCodexStopHook(hooksPath, stopCmd)
	if err != nil {
		fmt.Fprintln(g.Stderr, "codex-hook install:", err)
		return 1
	}
	if removed > 0 {
		fmt.Fprintf(g.Stdout, "Removed %d legacy ScoutTrace hook(s) from %s\n", removed, hooksPath)
	}
	fmt.Fprintf(g.Stdout, "Installed Codex Stop hook in %s\n", hooksPath)
	fmt.Fprintln(g.Stdout, "Restart Codex or reopen the project for hook settings to take effect.")
	return 0
}

func installCodexStopHook(path, stopCommand string) (int, error) {
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			return 0, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	removed := removeScoutTracePostToolUseHooks(hooks)
	removed += removeLegacyScoutTraceCodexStopHooks(hooks, stopCommand)
	if !hookCommandExists(hooks["Stop"], stopCommand) {
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": stopCommand}}}
		arr, _ := hooks["Stop"].([]any)
		hooks["Stop"] = append(arr, entry)
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	return removed, os.WriteFile(path, append(b, '\n'), 0o600)
}

func removeScoutTracePostToolUseHooks(hooks map[string]any) int {
	raw, ok := hooks["PostToolUse"]
	if !ok {
		return 0
	}
	arr, _ := raw.([]any)
	if len(arr) == 0 {
		delete(hooks, "PostToolUse")
		return 0
	}
	kept := make([]any, 0, len(arr))
	removed := 0
	for _, entry := range arr {
		if hookEntryIsScoutTracePostToolUse(entry) {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		delete(hooks, "PostToolUse")
	} else {
		hooks["PostToolUse"] = kept
	}
	return removed
}

func removeLegacyScoutTraceCodexStopHooks(hooks map[string]any, keepCommand string) int {
	raw, ok := hooks["Stop"]
	if !ok {
		return 0
	}
	arr, _ := raw.([]any)
	if len(arr) == 0 {
		delete(hooks, "Stop")
		return 0
	}
	kept := make([]any, 0, len(arr))
	removed := 0
	for _, entry := range arr {
		cmd := hookEntryCommand(entry)
		isLegacyClaude := strings.Contains(cmd, "scouttrace") && strings.Contains(cmd, "claude-hook stop")
		isStaleCodex := strings.Contains(cmd, "scouttrace") && strings.Contains(cmd, "codex-hook stop") && cmd != keepCommand
		if isLegacyClaude || isStaleCodex {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = kept
	}
	return removed
}

func hookEntryIsScoutTracePostToolUse(entry any) bool {
	cmd := hookEntryCommand(entry)
	return strings.Contains(cmd, "scouttrace") &&
		(strings.Contains(cmd, "claude-hook post-tool-use") || strings.Contains(cmd, "codex-hook post-tool-use"))
}

func hookEntryCommand(entry any) string {
	m, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	hs, _ := m["hooks"].([]any)
	if len(hs) == 0 {
		return ""
	}
	for _, h := range hs {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if cmd != "" {
			return cmd
		}
	}
	return ""
}

func hookCommandExists(raw any, command string) bool {
	arr, _ := raw.([]any)
	for _, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		hs, _ := m["hooks"].([]any)
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if ok && hm["command"] == command {
				return true
			}
		}
	}
	return false
}
