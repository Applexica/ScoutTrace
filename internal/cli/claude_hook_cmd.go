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

	var hp claudeToolHookPayload
	if err := json.Unmarshal(body, &hp); err != nil {
		fmt.Fprintln(g.Stderr, "claude-hook:", err)
		if *failClosed {
			return 1
		}
		return 0
	}
	env, err := buildClaudeHookEvent(body, c, *hostVersion, g.Home)
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

	// Beside the per-tool event, scan the transcript for any new assistant
	// LLM turns that completed since the last hook fire. In a long Claude
	// tool loop dozens of PostToolUse events arrive before Stop; emitting
	// llm_turn events here makes their billing visible in real time. The
	// shared cursor in buildClaudeLLMTurnEvents dedupes against the final
	// Stop catch-up so events are never enqueued twice.
	llmEvents := buildClaudeLLMTurnEventsFromPostTool(g, &hp, c, *hostVersion)
	llmIDs := make([]string, 0, len(llmEvents))
	for _, lev := range llmEvents {
		lpayload, err := json.Marshal(lev)
		if err != nil {
			fmt.Fprintln(g.Stderr, "claude-hook:", err)
			if *failClosed {
				return 1
			}
			continue
		}
		if err := q.Enqueue(lev.ID, dest, lpayload); err != nil {
			fmt.Fprintln(g.Stderr, "claude-hook:", err)
			if *failClosed {
				return 1
			}
			continue
		}
		llmIDs = append(llmIDs, lev.ID)
	}

	if g.JSON {
		out := map[string]any{"id": env.ID, "destination": dest}
		if len(llmIDs) > 0 {
			out["llm_turn_ids"] = llmIDs
			out["llm_turn_count"] = len(llmIDs)
		}
		_ = printJSON(g.Stdout, out, false)
	} else if g.Verbose > 0 {
		fmt.Fprintf(g.Stderr, "claude-hook: enqueued %s → %s\n", env.ID, dest)
		if len(llmIDs) > 0 {
			fmt.Fprintf(g.Stderr, "claude-hook: enqueued %d llm_turn event(s) → %s\n", len(llmIDs), dest)
		}
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

// buildClaudeLLMTurnEventsFromPostTool runs the transcript scan during a
// PostToolUse invocation. Errors here are non-fatal: capture continues with
// the tool event already enqueued and the cursor will catch up on the next
// fire (or on Stop). The hook payload may legitimately omit transcript_path
// for synthetic test inputs, in which case we silently skip.
func buildClaudeLLMTurnEventsFromPostTool(g *Globals, hp *claudeToolHookPayload, c *config.Config, hostVersion string) []*event.ToolCallEvent {
	if hp == nil || hp.TranscriptPath == "" || g == nil || g.Home == "" {
		return nil
	}
	events, err := buildClaudeLLMTurnEvents(hp.TranscriptPath, hp.SessionID, c, hostVersion, g.Home)
	if err != nil {
		if g.Verbose > 0 {
			fmt.Fprintln(g.Stderr, "claude-hook: transcript scan skipped:", err)
		}
		return nil
	}
	return events
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
// message.model and message.usage. The same scan is also driven by every
// PostToolUse invocation (see claudeHookPostToolUse) so that long Claude tool
// loops stream LLM turns to the destination as they happen rather than being
// held until Stop fires. A persistent per-transcript cursor under
// <scoutHome>/claude_hook/cursors/ ensures repeated invocations only enqueue
// events for lines appended since the last call.
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
	return buildClaudeLLMTurnEvents(hp.TranscriptPath, hp.SessionID, c, hostVersion, scoutHome)
}

// buildClaudeLLMTurnEvents scans transcriptPath from the persisted cursor and
// returns one llm_turn event per new assistant entry that carries model+usage.
// Both the Stop hook and each PostToolUse hook drive this so events stream out
// during a long tool loop. The cursor — a JSON {offset, prior_effective_in}
// blob — both prevents re-emitting old lines and supplies the running
// effectiveIn baseline used to compute incremental tokens_in deltas.
func buildClaudeLLMTurnEvents(transcriptPath, hookSessionID string, c *config.Config, hostVersion, scoutHome string) ([]*event.ToolCallEvent, error) {
	if transcriptPath == "" {
		return nil, fmt.Errorf("missing transcript_path")
	}

	cursorPath := claudeTranscriptCursorPath(scoutHome, hookSessionID, transcriptPath)
	startCursor := readClaudeTranscriptCursor(cursorPath)

	turns, endCursor, err := readNewAssistantUsageLines(transcriptPath, startCursor)
	if err != nil {
		return nil, err
	}

	const serverName = "claude-code"
	const toolName = "llm_turn"

	sessionID := hookSessionID
	if sessionID == "" {
		sessionID = event.NewULID()
	}

	live, liveSource := liveLookup(c, scoutHome)
	out := make([]*event.ToolCallEvent, 0, len(turns))
	for _, t := range turns {
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
		if bb := buildLLMTurnBilling(t, live, liveSource); !bb.Empty() {
			ev.Billing = eventBillingBlock(bb)
		}
		final, err := finalizeWithRedaction(ev, c)
		if err != nil {
			return nil, err
		}
		out = append(out, final)
	}

	// Persist the cursor whenever the offset advanced — even with no new
	// qualifying assistant lines — so non-assistant lines are not re-scanned.
	// PriorEffectiveIn is also persisted so the next invocation can compute
	// the next incremental delta correctly.
	if endCursor.Offset > startCursor.Offset || endCursor.PriorEffectiveIn != startCursor.PriorEffectiveIn {
		_ = writeClaudeTranscriptCursor(cursorPath, endCursor)
	}

	return out, nil
}

// buildLLMTurnBilling constructs the billing.Block for a single transcript
// turn. It preserves the displayed/incremental tokens_in convention
// (TokensInDelta) but apportions that delta across input / cache_creation /
// cache_read using THIS turn's per-call breakdown so the live provider's
// cache rates apply to the cache portion. When live pricing is unavailable,
// the apportionment is irrelevant — EstimateUsage's static fallback charges
// the full delta at input_per_1m, exactly matching the legacy behavior.
//
// Returns an empty Block when the model is unknown to both live and static
// pricing — callers can drop the Billing pointer in that case.
func buildLLMTurnBilling(t claudeStopTurn, live billing.LiveLookup, liveSource string) billing.Block {
	usage := apportionUsage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cost, source, ok := billing.EstimateUsage(ctx, live, liveSource, t.Model, usage)
	if !ok {
		return billing.Block{}
	}
	tokensIn := t.TokensInDelta
	tokensOut := t.OutputTokens
	c := cost
	return billing.Block{
		CostUSD:       &c,
		TokensIn:      &tokensIn,
		TokensOut:     &tokensOut,
		Model:         t.Model,
		Provider:      billing.LookupProvider(t.Model),
		PricingSource: source,
	}
}

// apportionUsage splits the delta tokens_in across the per-call breakdown so
// cache_creation / cache_read each get their own (typically cheaper) rate.
// Apportionment uses THIS turn's component ratios — a reasonable proxy when
// the cumulative deltas don't expose the per-component split directly.
//
// Edge cases: when no cache fields were reported (CurCacheCreation=0 and
// CurCacheRead=0) the entire delta is billed as Input — preserving exactly
// the legacy behavior. When the apportioned input + cache_creation rounds
// would lose tokens to floor() error, the residue is folded into CacheRead so
// the total stays equal to TokensInDelta.
func apportionUsage(t claudeStopTurn) billing.Usage {
	if t.TokensInDelta <= 0 || (t.CurCacheCreation == 0 && t.CurCacheRead == 0) {
		return billing.Usage{Input: t.TokensInDelta, Output: t.OutputTokens}
	}
	totalCur := t.CurInput + t.CurCacheCreation + t.CurCacheRead
	if totalCur <= 0 {
		return billing.Usage{Input: t.TokensInDelta, Output: t.OutputTokens}
	}
	delta := t.TokensInDelta
	inDelta := delta * t.CurInput / totalCur
	cwDelta := delta * t.CurCacheCreation / totalCur
	crDelta := delta - inDelta - cwDelta
	return billing.Usage{
		Input:         inDelta,
		CacheCreation: cwDelta,
		CacheRead:     crDelta,
		Output:        t.OutputTokens,
	}
}

// claudeStopTurn is one assistant transcript entry's billing-relevant fields,
// already adjusted to per-turn incremental token counts. CurInput,
// CurCacheCreation and CurCacheRead carry the per-call breakdown reported by
// THIS line's usage object. They are used to apportion TokensInDelta across
// input / cache_creation / cache_read at billing time so the live provider's
// cache rates (when known) actually apply to the cache portion of the new
// tokens. They are not persisted in the cursor — they are derived afresh on
// every scan from the line itself.
type claudeStopTurn struct {
	Model            string
	TokensInDelta    int
	OutputTokens     int
	CurInput         int
	CurCacheCreation int
	CurCacheRead     int
}

// claudeTranscriptCursor is the persisted state for a transcript scan.
// Offset is the byte offset just past the last fully-consumed line.
// PriorEffectiveIn is the cumulative effective input-token total reported by
// the most recent assistant line we have already emitted; it is the baseline
// the next scan subtracts from to compute an incremental tokens_in.
type claudeTranscriptCursor struct {
	Offset           int64    `json:"offset"`
	PriorEffectiveIn int      `json:"prior_effective_in"`
	SeenMessageKeys  []string `json:"seen_message_keys,omitempty"`
}

// readNewAssistantUsageLines scans the transcript starting at start.Offset and
// returns one claudeStopTurn per fully-terminated assistant line carrying
// model+usage. Each turn's TokensInDelta is computed as
// effectiveIn(line) - runningEffectiveIn, where effectiveIn sums input_tokens
// + cache_creation_input_tokens + cache_read_input_tokens (both snake_case and
// camelCase variants). When the delta would be zero or negative — for example
// after a context reset that shrinks the cumulative total — the current
// effectiveIn is used directly so we do not under-bill the new context.
//
// The returned cursor reflects the byte offset of the last fully-read line
// and the running effectiveIn carried forward to the next scan. Partial
// trailing lines (no \n) are not consumed and will be picked up later.
func readNewAssistantUsageLines(path string, start claudeTranscriptCursor) ([]claudeStopTurn, claudeTranscriptCursor, error) {
	end := start
	f, err := os.Open(path)
	if err != nil {
		return nil, end, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, end, fmt.Errorf("stat transcript: %w", err)
	}
	if start.Offset > info.Size() {
		// File was truncated or rotated; reset both offset and prior so the
		// new transcript bills from zero.
		start = claudeTranscriptCursor{}
		end = start
	}
	if _, err := f.Seek(start.Offset, io.SeekStart); err != nil {
		return nil, end, fmt.Errorf("seek transcript: %w", err)
	}

	r := bufio.NewReader(f)
	end.Offset = start.Offset
	runningEffectiveIn := start.PriorEffectiveIn
	seenMessageKeys := map[string]bool{}
	end.SeenMessageKeys = nil
	for _, key := range start.SeenMessageKeys {
		if key != "" {
			seenMessageKeys[key] = true
			end.SeenMessageKeys = appendSeenClaudeMessageKey(end.SeenMessageKeys, key)
		}
	}
	var out []claudeStopTurn
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// Trailing partial line (no \n yet) — do not consume.
			break
		}
		if err != nil {
			return nil, end, fmt.Errorf("read transcript: %w", err)
		}
		end.Offset += int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var entry struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			Message   struct {
				ID    string          `json:"id"`
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
		usage := transcriptUsageBreakdownOf(entry.Message.Usage)
		effIn, outTokens := usage.EffectiveIn(), usage.Output
		messageKey := claudeAssistantMessageKey(entry.Message.ID, entry.RequestID)
		if messageKey != "" && seenMessageKeys[messageKey] {
			// Claude Code can append the same assistant message multiple times as
			// the transcript is updated around tool execution. Those rows have the
			// same message/request id and usage; emitting them again would double-bill
			// output tokens and, before this guard, could re-charge the full cached
			// context as tokens_in.
			continue
		}
		var delta int
		if effIn > runningEffectiveIn {
			delta = effIn - runningEffectiveIn
		} else if effIn == runningEffectiveIn {
			// Same effective context size as the previous emitted assistant turn.
			// This can be a legitimate distinct API call with only output tokens to
			// charge, or a duplicate already caught above. Never treat equal as a
			// context reset; doing so re-bills the full context and causes 60k+
			// token spikes between small deltas.
			delta = 0
		} else {
			// Context shrank or reset — bill only the current turn's tokens.
			delta = effIn
		}
		runningEffectiveIn = effIn
		if messageKey != "" {
			seenMessageKeys[messageKey] = true
			end.SeenMessageKeys = appendSeenClaudeMessageKey(end.SeenMessageKeys, messageKey)
		}
		out = append(out, claudeStopTurn{
			Model:            entry.Message.Model,
			TokensInDelta:    delta,
			OutputTokens:     outTokens,
			CurInput:         usage.Input,
			CurCacheCreation: usage.CacheCreation,
			CurCacheRead:     usage.CacheRead,
		})
	}
	end.PriorEffectiveIn = runningEffectiveIn
	return out, end, nil
}

// transcriptUsageBreakdown holds the four fields we recognise on a single
// assistant transcript usage object, plus their effective input sum.
type transcriptUsageBreakdown struct {
	Input         int
	CacheCreation int
	CacheRead     int
	Output        int
}

// EffectiveIn returns input + cache_creation + cache_read.
func (t transcriptUsageBreakdown) EffectiveIn() int {
	return t.Input + t.CacheCreation + t.CacheRead
}

// transcriptUsageTokens extracts the cumulative effective input-token total
// (input_tokens + cache_creation_input_tokens + cache_read_input_tokens, with
// camelCase aliases) and the current output_tokens from a single assistant
// transcript usage object. Unknown fields are ignored so a future Anthropic
// usage shape change cannot crash the scan.
func transcriptUsageTokens(usage json.RawMessage) (effectiveIn, outputTokens int) {
	b := transcriptUsageBreakdownOf(usage)
	return b.EffectiveIn(), b.Output
}

// transcriptUsageBreakdownOf returns the per-component breakdown of a single
// assistant turn's usage object. Missing keys default to zero. Both snake_case
// and camelCase aliases are accepted to match Anthropic's documented shapes.
func transcriptUsageBreakdownOf(usage json.RawMessage) transcriptUsageBreakdown {
	var u map[string]any
	if err := json.Unmarshal(usage, &u); err != nil {
		return transcriptUsageBreakdown{}
	}
	pick := func(keys ...string) int {
		for _, key := range keys {
			if v, ok := u[key]; ok {
				if n, ok := jsonNumberToInt(v); ok {
					return n
				}
			}
		}
		return 0
	}
	return transcriptUsageBreakdown{
		Input:         pick("input_tokens", "inputTokens"),
		CacheCreation: pick("cache_creation_input_tokens", "cacheCreationInputTokens"),
		CacheRead:     pick("cache_read_input_tokens", "cacheReadInputTokens"),
		Output:        pick("output_tokens", "outputTokens"),
	}
}

func jsonNumberToInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

func claudeAssistantMessageKey(messageID, requestID string) string {
	if messageID != "" {
		return "message:" + messageID
	}
	if requestID != "" {
		return "request:" + requestID
	}
	return ""
}

func appendSeenClaudeMessageKey(keys []string, key string) []string {
	if key == "" {
		return keys
	}
	for _, existing := range keys {
		if existing == key {
			return keys
		}
	}
	keys = append(keys, key)
	const maxSeenClaudeMessageKeys = 1024
	if len(keys) > maxSeenClaudeMessageKeys {
		keys = keys[len(keys)-maxSeenClaudeMessageKeys:]
	}
	return keys
}

// claudeTranscriptCursorPath derives a deterministic file path for the
// per-transcript cursor. The hash mixes session id and transcript path so two
// sessions pointing at the same path do not collide, and so the filename is
// filesystem-safe regardless of the original transcript location.
func claudeTranscriptCursorPath(scoutHome, sessionID, transcriptPath string) string {
	if scoutHome == "" {
		return ""
	}
	h := sha256.Sum256([]byte(sessionID + "::" + transcriptPath))
	return filepath.Join(scoutHome, "claude_hook", "cursors", hex.EncodeToString(h[:])+".cursor")
}

// readClaudeTranscriptCursor returns the persisted cursor state for path. The
// on-disk format is a JSON object {offset, prior_effective_in}; for backward
// compatibility with v0.1.11 and earlier (which wrote a plain decimal offset)
// a bare integer is parsed as offset with PriorEffectiveIn=0. A missing or
// unparsable file yields a zero-valued cursor.
func readClaudeTranscriptCursor(path string) claudeTranscriptCursor {
	if path == "" {
		return claudeTranscriptCursor{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return claudeTranscriptCursor{}
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return claudeTranscriptCursor{}
	}
	var cur claudeTranscriptCursor
	if err := json.Unmarshal([]byte(trimmed), &cur); err == nil && cur.Offset >= 0 && cur.PriorEffectiveIn >= 0 {
		return cur
	}
	// Legacy plain-integer cursor.
	if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil && n >= 0 {
		return claudeTranscriptCursor{Offset: n}
	}
	return claudeTranscriptCursor{}
}

// writeClaudeTranscriptCursor persists the cursor state, creating any missing
// parent directories. Errors are non-fatal (cursor will reset on the next
// invocation), so callers may discard the return value.
func writeClaudeTranscriptCursor(path string, cur claudeTranscriptCursor) error {
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

func buildClaudeHookEvent(body []byte, c *config.Config, hostVersion, scoutHome string) (*event.ToolCallEvent, error) {
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
	if bb := enrichClaudeHookBilling(&hp, serverName, toolName, c, scoutHome); !bb.Empty() {
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

func enrichClaudeHookBilling(hp *claudeToolHookPayload, serverName, toolName string, c *config.Config, scoutHome string) billing.Block {
	// Claude Code transcript usage belongs to the assistant LLM turn, not to the
	// individual PostToolUse event. Attaching transcript tokens to every tool
	// event creates misleading per-tool token counts (often input_tokens=1 due to
	// cache accounting) and inflated estimated costs. Only metadata reported by
	// the tool response itself, or an explicit static tool price, is safe to
	// attribute to this tool event.
	live, liveSource := liveLookup(c, scoutHome)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return billing.EnrichLive(ctx, firstRaw(hp.ToolResponse, hp.ToolResult, hp.ToolOutput), serverName, toolName, staticLookup(c), live, liveSource)
}

func staticLookup(c *config.Config) billing.StaticPriceLookup {
	if c == nil {
		return nil
	}
	return c.StaticPriceLookup()
}

// liveLookup returns the configured live-pricing lookup and source identifier
// for use inside the Claude hook capture path. A nil lookup means live pricing
// is disabled (or no config was loaded) and callers should fall back to the
// static estimate exactly as before.
func liveLookup(c *config.Config, scoutHome string) (billing.LiveLookup, string) {
	if c == nil {
		return nil, ""
	}
	return c.LiveLookupWithHome(scoutHome)
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
