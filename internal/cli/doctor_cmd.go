package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/webhookscout/scouttrace/internal/event"
)

type checkResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

// CmdDoctor runs the §18.5 self-check sequence.
func CmdDoctor(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	results := []checkResult{}
	add := func(name string, ok bool, note string) { results = append(results, checkResult{name, ok, note}) }

	// 1. config.
	c, err := loadConfig(g)
	if err != nil {
		add("config", false, err.Error())
	} else {
		add("config", true, fmt.Sprintf("destinations=%d", len(c.Destinations)))
	}
	// 2. queue self-test.
	q, err := openQueue(g, c)
	if err != nil {
		add("queue.open", false, err.Error())
	} else {
		add("queue.open", true, "")
		// Enqueue a synthetic event, claim it, ack it.
		ev := event.New(event.NewSession("doctor"), "0.1.0", "doctor", "0.0.0")
		ev.Tool = event.ToolBlock{Name: "_doctor"}
		ev.Server = event.ServerBlock{Name: "doctor"}
		raw, _ := json.Marshal(ev)
		if err := q.Enqueue(ev.ID, "doctor", raw); err != nil {
			add("queue.enqueue", false, err.Error())
		} else {
			add("queue.enqueue", true, "")
			if got, err := q.ClaimPending("doctor", 1); err != nil {
				add("queue.claim", false, err.Error())
			} else if len(got) != 1 {
				add("queue.claim", false, fmt.Sprintf("got %d records", len(got)))
			} else {
				add("queue.claim", true, "")
				if err := q.Ack(got[0].ID); err != nil {
					add("queue.ack", false, err.Error())
				} else {
					add("queue.ack", true, "")
				}
			}
		}
	}
	// 3. Destinations: try to construct adapters; we don't actually send.
	if c != nil {
		_, err := buildRegistry(c, newResolver(g))
		if err != nil {
			add("destinations", false, err.Error())
		} else {
			add("destinations", true, "all adapters configured OK")
		}
	}

	exit := 0
	for _, r := range results {
		if !r.OK {
			exit = 1
		}
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]any{"checks": results, "ok": exit == 0}, true)
		return exit
	}
	for _, r := range results {
		mark := "OK "
		if !r.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(g.Stdout, "[%s] %s %s\n", mark, r.Name, r.Note)
	}
	return exit
}
