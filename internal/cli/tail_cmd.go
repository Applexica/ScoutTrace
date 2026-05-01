package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/webhookscout/scouttrace/internal/queue"
)

func bytesTrimRight(b []byte, cutset string) []byte {
	return []byte(strings.TrimRight(string(b), cutset))
}

// CmdTail prints queue records as they arrive. With --once, prints the
// current pending list and exits — handy for tests and scripts.
//
// --format: ndjson (default; one JSON object per line),
//
//	pretty (indented JSON, one object per group),
//	json   (a single JSON array per poll).
func CmdTail(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	once := fs.Bool("once", false, "print current snapshot and exit")
	dest := fs.String("destination", "", "filter by destination name")
	limit := fs.Int("limit", 100, "max records per poll")
	raw := fs.Bool("raw", false, "show pre-redaction (requires tail.allow_raw=true)")
	format := fs.String("format", "ndjson", "output format: ndjson|json|pretty")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if *raw {
		fmt.Fprintln(g.Stderr, "tail: --raw refused; pre-redaction storage is off by default")
		return 78
	}
	switch *format {
	case "ndjson", "json", "pretty":
	default:
		fmt.Fprintf(g.Stderr, "tail: unknown --format %q (want ndjson|json|pretty)\n", *format)
		return 64
	}
	c, _ := loadConfig(g)
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	seen := map[string]struct{}{}
	for {
		recs, err := snapshot(q, *dest, *limit)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
		fresh := recs[:0]
		for _, r := range recs {
			if _, ok := seen[r.ID]; ok {
				continue
			}
			seen[r.ID] = struct{}{}
			fresh = append(fresh, r)
		}
		if err := writeFormatted(g.Stdout, fresh, *format); err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
		if *once {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func writeFormatted(w io.Writer, recs []queue.Record, format string) error {
	if len(recs) == 0 {
		return nil
	}
	switch format {
	case "ndjson":
		var buf bytes.Buffer
		for _, r := range recs {
			buf.Reset()
			if err := json.Compact(&buf, r.Payload); err != nil {
				// Payload isn't valid JSON; emit verbatim with one trailing newline.
				w.Write(bytesTrimRight(r.Payload, "\r\n\t "))
				w.Write([]byte{'\n'})
				continue
			}
			buf.WriteByte('\n')
			w.Write(buf.Bytes())
		}
	case "pretty":
		for _, r := range recs {
			var ev any
			if err := json.Unmarshal(r.Payload, &ev); err != nil {
				return err
			}
			b, err := json.MarshalIndent(ev, "", "  ")
			if err != nil {
				return err
			}
			w.Write(b)
			w.Write([]byte{'\n'})
		}
	case "json":
		batch := make([]json.RawMessage, len(recs))
		for i, r := range recs {
			batch[i] = r.Payload
		}
		b, err := json.MarshalIndent(batch, "", "  ")
		if err != nil {
			return err
		}
		w.Write(b)
		w.Write([]byte{'\n'})
	}
	return nil
}

func snapshot(q *queue.Queue, dest string, limit int) ([]queue.Record, error) {
	got, err := q.Peek(dest, limit)
	if err != nil {
		return nil, err
	}
	sort.Slice(got, func(i, j int) bool { return got[i].EnqueuedAt < got[j].EnqueuedAt })
	return got, nil
}
