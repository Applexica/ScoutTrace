package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/webhookscout/scouttrace/internal/dispatch"
	"github.com/webhookscout/scouttrace/internal/event"
)

// CmdQueue dispatches queue subcommands: stats, list, inject, flush, prune.
func CmdQueue(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "queue: subcommand required (stats|list|inject|flush|prune)")
		return 64
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "stats":
		return queueStats(g, rest)
	case "list":
		return queueList(g, rest)
	case "inject":
		return queueInject(g, rest)
	case "flush":
		return queueFlush(g, rest)
	case "prune":
		return queuePrune(g, rest)
	}
	fmt.Fprintf(g.Stderr, "queue: unknown subcommand %q\n", sub)
	return 64
}

// CmdFlush is the top-level alias for `queue flush`.
func CmdFlush(ctx context.Context, g *Globals, args []string) int {
	return queueFlush(g, args)
}

func queueStats(g *Globals, args []string) int {
	fs := flag.NewFlagSet("queue stats", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	c, _ := loadConfig(g)
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	st, err := q.Stats()
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if g.JSON {
		_ = printJSON(g.Stdout, st, true)
		return 0
	}
	fmt.Fprintf(g.Stdout, "pending=%d inflight=%d dead=%d\n", st.Pending, st.Inflight, st.Dead)
	return 0
}

func queueList(g *Globals, args []string) int {
	fs := flag.NewFlagSet("queue list", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	dest := fs.String("destination", "", "destination filter")
	limit := fs.Int("limit", 50, "max records")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	c, _ := loadConfig(g)
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	recs, err := q.Peek(*dest, *limit)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	for _, r := range recs {
		var ev map[string]any
		if err := json.Unmarshal(r.Payload, &ev); err == nil {
			_ = printJSON(g.Stdout, map[string]any{
				"id":     r.ID,
				"dest":   r.Destination,
				"status": r.Status,
				"event":  ev,
			}, false)
		}
	}
	return 0
}

// queueInject reads a JSON envelope from stdin or --from <path> and
// enqueues it under the supplied destination.
func queueInject(g *Globals, args []string) int {
	fs := flag.NewFlagSet("queue inject", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	from := fs.String("from", "", "path to JSON envelope")
	dest := fs.String("destination", "", "destination name (defaults to config default)")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	c, _ := loadConfig(g)
	target := *dest
	if target == "" && c != nil {
		target = c.DefaultDestination
	}
	if target == "" {
		fmt.Fprintln(g.Stderr, "queue inject: --destination required")
		return 64
	}
	var b []byte
	var err error
	if *from != "" {
		b, err = os.ReadFile(*from)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 77
		}
	} else {
		b, err = readStdin(g)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
	}
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	id := event.NewULID()
	if err := q.Enqueue(id, target, b); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if !g.JSON {
		fmt.Fprintf(g.Stdout, "Enqueued %s → %s\n", id, target)
	} else {
		_ = printJSON(g.Stdout, map[string]string{"id": id, "destination": target}, false)
	}
	return 0
}

func queueFlush(g *Globals, args []string) int {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	dest := fs.String("destination", "", "limit dispatch to this destination")
	to := fs.String("to", "", "alias for --destination")
	yes := fs.Bool("yes", false, "auto-approve any first-send destination prompts")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if *to != "" && *dest == "" {
		*dest = *to
	}
	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if err := enforceFirstSendApproval(g, c, *yes); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	reg, err := buildRegistry(c, newResolver(g))
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if *dest != "" {
		filtered, err := filterRegistry(reg, *dest)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 78
		}
		reg = filtered
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.RunOnce(ctx); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	ctr := d.Counters()
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]any{
			"attempts": ctr.Attempts, "success": ctr.Success, "dead": ctr.Dead,
		}, false)
	} else {
		fmt.Fprintf(g.Stdout, "attempts=%d success=%d dead=%d\n", ctr.Attempts, ctr.Success, ctr.Dead)
	}
	return 0
}

func queuePrune(g *Globals, args []string) int {
	fs := flag.NewFlagSet("queue prune", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	maxAgeDays := fs.Int("max-age-days", 7, "max age of dead events")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	c, _ := loadConfig(g)
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if err := q.Prune(time.Duration(*maxAgeDays) * 24 * time.Hour); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if !g.JSON {
		fmt.Fprintln(g.Stdout, "OK")
	}
	return 0
}

func readStdin(g *Globals) ([]byte, error) {
	if g.Stdin == nil {
		return nil, nil
	}
	return io.ReadAll(g.Stdin)
}

func nonZero(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
