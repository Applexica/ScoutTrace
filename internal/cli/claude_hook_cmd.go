package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/webhookscout/scouttrace/internal/billing"
	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/dispatch"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/version"
)

// CmdClaudeHook integrates ScoutTrace with Claude Code hooks. This complements
// the MCP stdio proxy: Claude Code built-in tools and plugin-provided MCP tools
// (for example plugin:playwright:playwright) do not necessarily launch through a
// user-editable MCP server command, so the proxy never sees their JSON-RPC. A
// PostToolUse hook is the reliable capture point for those tool executions.
func CmdClaudeHook(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "claude-hook: subcommand required (post-tool-use|stop|install|snippet)")
		return 64
	}
	switch args[0] {
	case "post-tool-use":
		return claudeHookPostToolUse(ctx, g, args[1:])
	case "stop":
		return claudeHookStop(ctx, g, args[1:])
	case "install":
		return claudeHookInstall(g, args[1:])
	case "snippet":
		return claudeHookSnippet(g, args[1:])
	default:
		fmt.Fprintf(g.Stderr, "claude-hook: unknown subcommand %q\n", args[0])
		return 64
	}
}

type claudeToolHookPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	ToolResult     json.RawMessage `json:"tool_result"`
	ToolOutput     json.RawMessage `json:"tool_output"`
}

func claudeHookPostToolUse(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("claude-hook post-tool-use", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	destFlag := fs.String("destination", "", "destination name (defaults to config default)")
	flush := fs.Bool("flush", false, "attempt a best-effort dispatch after enqueue")
	failClosed := fs.Bool("fail-closed", false, "return non-zero if capture or flush fails")
	hostVersion := fs.String("host-version", "", "optional Claude Code version label")
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
		fmt.Fprintln(g.Stderr, "claude-hook: empty or unreadable hook payload")
		if *failClosed {
			return 1
		}
		return 0
	}

	env, err := buildClaudeHookEvent(body, c, *hostVersion)
	if err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}

	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	payload, err := json.Marshal(env)
	if err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	if err := q.Enqueue(env.ID, dest, payload); err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]string{"id": env.ID, "destination": dest}, false)
	} else if g.Verbose > 0 {
		fmt.Fprintf(g.Stderr, "claude-hook: enqueued %s → %s\n", env.ID, dest)
	}

	if *flush {
		if err := flushQueueOnce(ctx, g, c, q, dest, 3*time.Second); err != nil {
			fmt.Fprintln(g.Stderr, "claude-hook: flush skipped/failed:", err)
			if *failClosed {
				return 1
			}
		}
	}
	return 0
}

type claudeStopHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

func claudeHookStop(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("claude-hook stop", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	destFlag := fs.String("destination", "", "destination name (defaults to config default)")
	flush := fs.Bool("flush", false, "attempt a best-effort dispatch after enqueue")
	failClosed := fs.Bool("fail-closed", false, "return non-zero if capture or flush fails")
	hostVersion := fs.String("host-version", "", "optional Claude Code version label")
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
		fmt.Fprintln(g.Stderr, "claude-hook: empty or unreadable hook payload")
		if *failClosed {
			return 1
		}
		return 0
	}

	events, err := buildClaudeStopEvents(body, c, *hostVersion, g.Home)
	if err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
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
			fmt.Fprintln(g.Stderr, "claude-hook stop: no new assistant turns with usage in transcript; skipping")
		}
		return 0
	}

	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	for _, env := range events {
		payload, err := json.Marshal(env)
		if err != nil {
			fmt.Fprintln(g.Stderr, "claude-hook:", err)
			if *failClosed {
				return 1
			}
			continue
		}
		if err := q.Enqueue(env.ID, dest, payload); err != nil {
			fmt.Fprintln(g.Stderr, "claude-hook:", err)
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
		fmt.Fprintf(g.Stderr, "claude-hook: enqueued %d llm_turn event(s) → %s\n", len(ids), dest)
	}

	if *flush {
		if err := flushQueueOnce(ctx, g, c, q, dest, 3*time.Second); err != nil {
			fmt.Fprintln(g.Stderr, "claude-hook: flush skipped/failed:", err)
			if *failClosed {
				return 1
			}
		}
	}
	return 0
}

// buildClaudeStopEvents reads the Claude Code Stop hook payload and emits one
// llm_turn ToolCallEvent per NEW assistant transcript line that carries both
// message.model and message.usage. A persistent per-transcript byte-offset
// cursor under <scoutHome>/claude_hook/cursors/ ensures repeated Stop hook
// invocations only enqueue events for lines appended since the last call —
// without it, every Stop fires would re-emit the entire transcript.
//
// Each event carries only billing metadata; the transcript's user and
// assistant content is never copied into Request/Response so prompts and
// replies cannot leak into capture. Returns an empty slice when nothing new
// is available.
func buildClaudeStopEvents(body []byte, c *config.Config, hostVersion, scoutHome string) ([]*event.ToolCallEvent, error) {
	var hp claudeStopHookPayload
	if err := json.Unmarshal(body, &hp); err != nil {
		return nil, fmt.Errorf("invalid Claude Code Stop hook JSON: %w", err)
	}
	if hp.TranscriptPath == "" {
		return nil, fmt.Errorf("missing transcript_path")
	}

	cursorPath := claudeStopCursorPath(scoutHome, hp.SessionID, hp.TranscriptPath)
	startOffset := readClaudeStopCursor(cursorPath)

	turns, endOffset, err := readNewAssistantUsageLines(hp.TranscriptPath, startOffset)
	if err != nil {
		return nil, err
	}

	const serverName = "claude-code"
	const toolName = "llm_turn"

	sessionID := hp.SessionID
	if sessionID == "" {
		sessionID = event.NewULID()
	}

	out := make([]*event.ToolCallEvent, 0, len(turns))
	for _, t := range turns {
		syntheticRaw, err := json.Marshal(map[string]any{
			"model": t.Model,
			"usage": json.RawMessage(t.Usage),
		})
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		ev := &event.ToolCallEvent{
			ID:         event.NewULIDAt(now),
			Schema:     event.SchemaVersion,
			CapturedAt: now,
			SessionID:  sessionID,
			TraceID:    event.NewTraceID(),
			SpanID:     event.NewSpanID(),
			Source: event.SourceBlock{
				Kind:              "claude_code_hook",
				Host:              "claude-code",
				HostVersion:       hostVersion,
				ScoutTraceVersion: version.Version,
			},
			Server:   event.ServerBlock{Name: serverName},
			Tool:     event.ToolBlock{Name: toolName},
			Request:  event.RequestBlock{JSONRPCID: "claude-code-hook"},
			Response: event.ResponseBlock{OK: true},
			Timing:   event.TimingBlock{StartedAt: now, EndedAt: now, LatencyMS: 0},
		}
		if bb := billing.Enrich(syntheticRaw, serverName, toolName, staticLookup(c)); !bb.Empty() {
			ev.Billing = eventBillingBlock(bb)
		}
		final, err := finalizeWithRedaction(ev, c)
		if err != nil {
			return nil, err
		}
		out = append(out, final)
	}

	// Always advance the cursor to the end of the consumed prefix, even when
	// no qualifying assistant lines were present, so non-assistant lines do
	// not get re-scanned on the next invocation.
	if endOffset > startOffset {
		_ = writeClaudeStopCursor(cursorPath, endOffset)
	}

	return out, nil
}

// claudeStopTurn is one assistant transcript entry's billing-relevant fields.
type claudeStopTurn struct {
	Model string
	Usage json.RawMessage
}

// readNewAssistantUsageLines scans the transcript starting at startOffset and
// returns one claudeStopTurn per fully-terminated assistant line carrying
// model+usage. It returns the byte offset at the end of the last fully-read
// line so callers can persist the cursor; partial trailing lines (no \n) are
// not consumed and will be picked up on a future invocation.
func readNewAssistantUsageLines(path string, startOffset int64) ([]claudeStopTurn, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat transcript: %w", err)
	}
	if startOffset > info.Size() {
		// File was truncated or rotated; reset.
		startOffset = 0
	}
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("seek transcript: %w", err)
	}

	r := bufio.NewReader(f)
	consumed := startOffset
	var out []claudeStopTurn
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// Trailing partial line (no \n yet) — do not consume.
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read transcript: %w", err)
		}
		consumed += int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Model string          `json:"model"`
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(trimmed, &entry); err != nil {
			continue
		}
		if entry.Type != "assistant" {
			continue
		}
		if entry.Message.Model == "" || len(entry.Message.Usage) == 0 || string(entry.Message.Usage) == "null" {
			continue
		}
		usageCopy := make(json.RawMessage, len(entry.Message.Usage))
		copy(usageCopy, entry.Message.Usage)
		out = append(out, claudeStopTurn{Model: entry.Message.Model, Usage: usageCopy})
	}
	return out, consumed, nil
}

// claudeStopCursorPath derives a deterministic file path for the per-transcript
// cursor. The hash mixes session id and transcript path so two sessions
// pointing at the same path do not collide, and so the filename is filesystem-
// safe regardless of the original transcript location.
func claudeStopCursorPath(scoutHome, sessionID, transcriptPath string) string {
	if scoutHome == "" {
		return ""
	}
	h := sha256.Sum256([]byte(sessionID + "::" + transcriptPath))
	return filepath.Join(scoutHome, "claude_hook", "cursors", hex.EncodeToString(h[:])+".cursor")
}

// readClaudeStopCursor returns the previously-saved byte offset for path, or
// 0 if no cursor exists or the on-disk content is unparsable.
func readClaudeStopCursor(path string) int64 {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeClaudeStopCursor persists the byte offset to the cursor file, creating
// any missing parent directories. Errors are non-fatal (cursor will reset on
// the next invocation), so callers may discard the return value.
func writeClaudeStopCursor(path string, offset int64) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(offset, 10)), 0o600)
}

func buildClaudeHookEvent(body []byte, c *config.Config, hostVersion string) (*event.ToolCallEvent, error) {
	var hp claudeToolHookPayload
	if err := json.Unmarshal(body, &hp); err != nil {
		return nil, fmt.Errorf("invalid Claude Code hook JSON: %w", err)
	}
	if hp.ToolName == "" {
		return nil, fmt.Errorf("missing tool_name")
	}
	serverName, toolName := splitClaudeToolName(hp.ToolName)
	sessionID := hp.SessionID
	if sessionID == "" {
		sessionID = event.NewULID()
	}
	now := time.Now().UTC()
	ev := &event.ToolCallEvent{
		ID:         event.NewULIDAt(now),
		Schema:     event.SchemaVersion,
		CapturedAt: now,
		SessionID:  sessionID,
		TraceID:    event.NewTraceID(),
		SpanID:     event.NewSpanID(),
		Source: event.SourceBlock{
			Kind:              "claude_code_hook",
			Host:              "claude-code",
			HostVersion:       hostVersion,
			ScoutTraceVersion: version.Version,
		},
		Server: event.ServerBlock{Name: serverName},
		Tool:   event.ToolBlock{Name: toolName},
		Request: event.RequestBlock{
			JSONRPCID:         "claude-code-hook",
			Args:              normalizeRaw(hp.ToolInput),
			ArgsBytesOriginal: len(hp.ToolInput),
		},
		Response: event.ResponseBlock{
			OK:                  !claudeHookResponseIsError(hp),
			Result:              normalizeRaw(firstRaw(hp.ToolResponse, hp.ToolResult, hp.ToolOutput)),
			ResultBytesOriginal: len(firstRaw(hp.ToolResponse, hp.ToolResult, hp.ToolOutput)),
		},
		Timing: event.TimingBlock{StartedAt: now, EndedAt: now, LatencyMS: 0},
	}
	if bb := enrichClaudeHookBilling(&hp, serverName, toolName, c); !bb.Empty() {
		ev.Billing = eventBillingBlock(bb)
	}

	if !capturePolicyFromConfig(c).ShouldCaptureArgs(serverName) {
		ev.Request.Args = nil
	}
	if !capturePolicyFromConfig(c).ShouldCaptureResult(serverName) {
		ev.Response.Result = nil
	}
	if c != nil {
		if c.Capture.MaxArgBytes > 0 && len(ev.Request.Args) > c.Capture.MaxArgBytes {
			ev.Request.Args = json.RawMessage(fmt.Sprintf("\"[truncated:%d->%d]\"", len(ev.Request.Args), c.Capture.MaxArgBytes))
			ev.Request.ArgsTruncated = true
		}
		if c.Capture.MaxResultBytes > 0 && len(ev.Response.Result) > c.Capture.MaxResultBytes {
			ev.Response.Result = json.RawMessage(fmt.Sprintf("\"[truncated:%d->%d]\"", len(ev.Response.Result), c.Capture.MaxResultBytes))
			ev.Response.ResultTruncated = true
		}
	}

	return finalizeWithRedaction(ev, c)
}

// finalizeWithRedaction runs the configured redaction engine over a fully-built
// ToolCallEvent and returns the post-redaction copy with the Redaction block
// populated. Shared between the post-tool-use and stop hook builders.
func finalizeWithRedaction(ev *event.ToolCallEvent, c *config.Config) (*event.ToolCallEvent, error) {
	profile := "standard"
	if c != nil && c.Redaction.Profile != "" {
		profile = c.Redaction.Profile
	}
	pol := redact.ByName(profile)
	if pol == nil {
		return nil, fmt.Errorf("unknown redaction profile: %s", profile)
	}
	eng, err := redact.NewEngine(pol, nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	red, res, err := eng.Apply(raw)
	if err != nil {
		return nil, err
	}
	var out event.ToolCallEvent
	if err := json.Unmarshal(red, &out); err != nil {
		return nil, err
	}
	out.Redaction = event.RedactionBlock{
		PolicyName:     eng.Policy().Name,
		PolicyHash:     eng.Policy().Hash(),
		FieldsRedacted: res.FieldsRedacted,
		RulesApplied:   res.RulesApplied,
	}
	return &out, nil
}

func enrichClaudeHookBilling(hp *claudeToolHookPayload, serverName, toolName string, c *config.Config) billing.Block {
	// Claude Code transcript usage belongs to the assistant LLM turn, not to the
	// individual PostToolUse event. Attaching transcript tokens to every tool
	// event creates misleading per-tool token counts (often input_tokens=1 due to
	// cache accounting) and inflated estimated costs. Only metadata reported by
	// the tool response itself, or an explicit static tool price, is safe to
	// attribute to this tool event.
	return billing.Enrich(firstRaw(hp.ToolResponse, hp.ToolResult, hp.ToolOutput), serverName, toolName, staticLookup(c))
}

func staticLookup(c *config.Config) billing.StaticPriceLookup {
	if c == nil {
		return nil
	}
	return c.StaticPriceLookup()
}

func eventBillingBlock(bb billing.Block) *event.BillingBlock {
	return &event.BillingBlock{
		CostUSD:       bb.CostUSD,
		TokensIn:      bb.TokensIn,
		TokensOut:     bb.TokensOut,
		Model:         bb.Model,
		Provider:      bb.Provider,
		PricingSource: bb.PricingSource,
	}
}

func splitClaudeToolName(name string) (server string, tool string) {
	parts := strings.Split(name, "__")
	if len(parts) >= 3 && parts[0] == "mcp" {
		return parts[1], strings.Join(parts[2:], "__")
	}
	if len(parts) >= 3 && parts[0] == "plugin" {
		return parts[1], strings.Join(parts[2:], "__")
	}
	return "claude-code", name
}

func claudeHookResponseIsError(hp claudeToolHookPayload) bool {
	for _, raw := range []json.RawMessage{hp.ToolResponse, hp.ToolResult, hp.ToolOutput} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			for _, key := range []string{"is_error", "isError", "error"} {
				if v, ok := m[key]; ok {
					switch t := v.(type) {
					case bool:
						if t {
							return true
						}
					case string:
						if t != "" {
							return true
						}
					case map[string]any:
						return true
					}
				}
			}
		}
	}
	return false
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(v) > 0 && string(v) != "null" {
			return v
		}
	}
	return nil
}

func normalizeRaw(v json.RawMessage) json.RawMessage {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}

func flushQueueOnce(ctx context.Context, g *Globals, c *config.Config, q *queue.Queue, dest string, timeout time.Duration) error {
	if err := enforceFirstSendApproval(g, c, false); err != nil {
		return err
	}
	reg, err := buildRegistry(c, newResolver(g))
	if err != nil {
		return err
	}
	if dest != "" {
		reg, err = filterRegistry(reg, dest)
		if err != nil {
			return err
		}
	}
	d := dispatch.New(dispatch.Options{
		Queue: q, Registry: reg, BatchMax: 25,
		Backoff: dispatch.BackoffConfig{
			InitialMS:  nonZero(c.Delivery.InitialBackoffMS, 500),
			MaxMS:      nonZero(c.Delivery.MaxBackoffMS, 60_000),
			MaxRetries: nonZero(c.Delivery.MaxRetries, 8),
			Jitter:     c.Delivery.Jitter,
		},
		Now: time.Now,
	})
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return d.RunOnce(fctx)
}

func claudeHookCommand(g *Globals, subcommand string, flush bool, destination string) string {
	bin := g.ScoutBinary
	if bin == "" {
		bin = "scouttrace"
	}
	parts := []string{shellQuote(bin)}
	if g.Home != "" {
		parts = append(parts, "--home", shellQuote(g.Home))
	}
	parts = append(parts, "claude-hook", subcommand)
	if destination != "" {
		parts = append(parts, "--destination", shellQuote(destination))
	}
	if flush {
		parts = append(parts, "--flush")
	}
	return strings.Join(parts, " ")
}

func claudeHookSnippet(g *Globals, args []string) int {
	fs := flag.NewFlagSet("claude-hook snippet", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	dest := fs.String("destination", "", "destination name")
	flush := fs.Bool("flush", true, "include --flush in the hook command")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	postCmd := claudeHookCommand(g, "post-tool-use", *flush, *dest)
	stopCmd := claudeHookCommand(g, "stop", *flush, *dest)
	snippet := map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []any{
				map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": postCmd},
					},
				},
			},
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

func claudeHookInstall(g *Globals, args []string) int {
	fs := flag.NewFlagSet("claude-hook install", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	scope := fs.String("scope", "local", "Claude Code settings scope: local, project, or user")
	projectDir := fs.String("project-dir", ".", "project directory for local/project scopes")
	dest := fs.String("destination", "", "destination name")
	flush := fs.Bool("flush", true, "attempt dispatch from the hook after enqueue")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	settingsPath, err := claudeSettingsPath(*scope, *projectDir)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 64
	}
	postCmd := claudeHookCommand(g, "post-tool-use", *flush, *dest)
	stopCmd := claudeHookCommand(g, "stop", *flush, *dest)
	if err := appendClaudeHook(settingsPath, "PostToolUse", postCmd, true); err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook install:", err)
		return 1
	}
	if err := appendClaudeHook(settingsPath, "Stop", stopCmd, false); err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook install:", err)
		return 1
	}
	fmt.Fprintf(g.Stdout, "Installed Claude Code PostToolUse and Stop hooks in %s\n", settingsPath)
	fmt.Fprintln(g.Stdout, "Restart Claude Code or reopen the project for hook settings to take effect.")
	return 0
}

func claudeSettingsPath(scope, projectDir string) (string, error) {
	switch scope {
	case "local":
		return filepath.Join(projectDir, ".claude", "settings.local.json"), nil
	case "project":
		return filepath.Join(projectDir, ".claude", "settings.json"), nil
	case "user":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	default:
		return "", fmt.Errorf("unknown --scope %q (want local, project, or user)", scope)
	}
}

func appendClaudeHook(path, eventName, command string, withMatcher bool) error {
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	arr, _ := hooks[eventName].([]any)
	entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}}
	if withMatcher {
		entry["matcher"] = "*"
	}
	for _, existing := range arr {
		if m, ok := existing.(map[string]any); ok {
			if hs, ok := m["hooks"].([]any); ok {
				for _, h := range hs {
					if hm, ok := h.(map[string]any); ok && hm["command"] == command {
						return nil
					}
				}
			}
		}
	}
	hooks[eventName] = append(arr, entry)
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_+-=/:.,", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
