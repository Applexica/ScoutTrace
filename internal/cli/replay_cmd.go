package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/webhookscout/scouttrace/internal/event"
)

// CmdReplay enqueues envelopes from a NDJSON file (one event per line) for
// re-delivery. Useful when reprocessing exported events or seeding a
// destination during testing.
func CmdReplay(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	from := fs.String("from", "", "path to NDJSON file containing events")
	dest := fs.String("destination", "", "destination name (defaults to config default)")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if *from == "" {
		fmt.Fprintln(g.Stderr, "replay: --from required")
		return 64
	}
	c, _ := loadConfig(g)
	target := *dest
	if target == "" && c != nil {
		target = c.DefaultDestination
	}
	if target == "" {
		fmt.Fprintln(g.Stderr, "replay: no destination resolvable")
		return 64
	}
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	f, err := os.Open(*from)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 77
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	count := 0
	for dec.More() {
		var line json.RawMessage
		if err := dec.Decode(&line); err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
		id := event.NewULID()
		// Replace the id field if the envelope has one so downstream dedupe is preserved.
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err == nil {
			if existing, ok := ev["id"].(string); ok && existing != "" {
				id = existing
			}
		}
		if err := q.Enqueue(id, target, line); err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
		count++
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]any{"enqueued": count}, false)
	} else {
		fmt.Fprintf(g.Stdout, "Replayed %d events → %s\n", count, target)
	}
	return 0
}
