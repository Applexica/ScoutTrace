package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/redact"
)

// CmdPreview synthesizes a representative ToolCallEvent containing the
// secret patterns we care about and prints both the pre- and post-redaction
// envelope. With --json, it emits the redacted ToolCallEvent directly so
// it can be piped into `scouttrace queue inject`.
func CmdPreview(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	profile := fs.String("profile", "standard", "redaction profile name")
	withMeta := fs.Bool("with-meta", false, "with --json: emit wrapper {raw,redacted,fields,...}")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	pol := redact.ByName(*profile)
	if pol == nil {
		fmt.Fprintf(g.Stderr, "preview: unknown profile %q\n", *profile)
		return 64
	}
	eng, err := redact.NewEngine(pol, nil)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	sess := event.NewSession("preview")
	ev := event.New(sess, "0.1.0", "preview-host", "0.0.0")
	ev.Tool = event.ToolBlock{Name: "create_user"}
	ev.Server = event.ServerBlock{Name: "preview"}
	ev.Request.JSONRPCID = "n:42"
	ev.Request.Args = json.RawMessage(`{
		"email": "ada@example.com",
		"password": "AKIAABCDEFGHIJKLMNOP",
		"path": "/Users/ada/code/secret.txt"
	}`)
	ev.Response.OK = true
	ev.Response.Result = json.RawMessage(`{"created": true, "token": "ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`)
	ev.Timing = event.TimingBlock{
		StartedAt: time.Now().Add(-50 * time.Millisecond),
		EndedAt:   time.Now(),
		LatencyMS: 50,
	}
	raw, _ := json.Marshal(ev)
	out, res, err := eng.Apply(raw)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	// Re-decode and stamp the redaction block so the emitted event is a
	// faithful representation of what the capture pipeline would produce.
	var stamped event.ToolCallEvent
	if err := json.Unmarshal(out, &stamped); err == nil {
		stamped.Redaction = event.RedactionBlock{
			PolicyName:     pol.Name,
			PolicyHash:     pol.Hash(),
			FieldsRedacted: res.FieldsRedacted,
			RulesApplied:   res.RulesApplied,
		}
		if final, err := json.Marshal(&stamped); err == nil {
			out = final
		}
	}
	if g.JSON {
		if *withMeta {
			_ = printJSON(g.Stdout, map[string]any{
				"profile":  pol.Name,
				"hash":     pol.Hash(),
				"raw":      json.RawMessage(raw),
				"redacted": json.RawMessage(out),
				"applied":  res.RulesApplied,
				"fields":   res.FieldsRedacted,
			}, true)
			return 0
		}
		// Default: emit the redacted ToolCallEvent so callers can pipe
		// directly into `queue inject`.
		g.Stdout.Write(out)
		fmt.Fprintln(g.Stdout)
		return 0
	}
	fmt.Fprintf(g.Stdout, "Profile: %s (%s)\n", pol.Name, pol.Hash()[:19])
	fmt.Fprintln(g.Stdout, "--- Pre-redaction ---")
	g.Stdout.Write(prettyJSON(raw))
	fmt.Fprintln(g.Stdout)
	fmt.Fprintln(g.Stdout, "--- Post-redaction ---")
	g.Stdout.Write(prettyJSON(out))
	fmt.Fprintln(g.Stdout)
	fmt.Fprintf(g.Stdout, "Rules applied: %v\n", res.RulesApplied)
	return 0
}

func prettyJSON(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return append(out, '\n')
}
