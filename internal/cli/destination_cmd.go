package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/webhookscout/scouttrace/internal/config"
)

// CmdDestination dispatches destination subcommands.
func CmdDestination(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "destination: subcommand required (list|approve|approve-host)")
		return 64
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return destinationList(g, rest)
	case "approve":
		return destinationApprove(g, rest, false)
	case "approve-host":
		return destinationApprove(g, rest, true)
	}
	fmt.Fprintf(g.Stderr, "destination: unknown subcommand %q\n", sub)
	return 64
}

// destSeenEntry mirrors the §11.6 destinations_seen.json schema.
type destSeenEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Host        string `json:"host"`
	URLHash     string `json:"url_hash"`
	FirstUsedAt int64  `json:"first_used_at"`
}

func destinationsSeenPath(g *Globals) string {
	return filepath.Join(g.Home, "destinations_seen.json")
}

func loadDestSeen(g *Globals) ([]destSeenEntry, error) {
	b, err := os.ReadFile(destinationsSeenPath(g))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []destSeenEntry
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func saveDestSeen(g *Globals, entries []destSeenEntry) error {
	if err := ensureHome(g); err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(destinationsSeenPath(g), b, 0o600)
}

func destinationList(g *Globals, args []string) int {
	fs := flag.NewFlagSet("destination list", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	seen, _ := loadDestSeen(g)
	approved := map[string]bool{}
	for _, s := range seen {
		approved[s.Name+"/"+s.URLHash] = true
	}
	type row struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Host     string `json:"host"`
		URLHash  string `json:"url_hash"`
		Approved bool   `json:"approved"`
	}
	var rows []row
	for _, d := range c.Destinations {
		host, hash := destHostAndHash(d.URL, d.APIBase, d.Path)
		key := d.Name + "/" + hash
		rows = append(rows, row{Name: d.Name, Type: d.Type, Host: host, URLHash: hash, Approved: approved[key]})
	}
	if g.JSON {
		_ = printJSON(g.Stdout, rows, true)
		return 0
	}
	for _, r := range rows {
		mark := "[ ]"
		if r.Approved {
			mark = "[x]"
		}
		fmt.Fprintf(g.Stdout, "%s %-12s %-12s %s\n", mark, r.Name, r.Type, r.Host)
	}
	return 0
}

func destinationApprove(g *Globals, args []string, byHost bool) int {
	fs := flag.NewFlagSet("destination approve", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	rest := fs.Args()
	if byHost {
		if len(rest) != 2 {
			fmt.Fprintln(g.Stderr, "destination approve-host: usage: <type> <host>")
			return 64
		}
		typ, host := rest[0], rest[1]
		seen, _ := loadDestSeen(g)
		seen = append(seen, destSeenEntry{
			Name: "*", Type: typ, Host: host, URLHash: "any",
			FirstUsedAt: time.Now().Unix(),
		})
		if err := saveDestSeen(g, seen); err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
		if !g.JSON {
			fmt.Fprintf(g.Stdout, "Approved %s for type=%s\n", host, typ)
		}
		return 0
	}
	if len(rest) != 1 {
		fmt.Fprintln(g.Stderr, "destination approve: usage: <name>")
		return 64
	}
	name := rest[0]
	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	d := c.LookupDestination(name)
	if d == nil {
		fmt.Fprintf(g.Stderr, "destination approve: no such destination %q\n", name)
		return 78
	}
	host, hash := destHostAndHash(d.URL, d.APIBase, d.Path)
	seen, _ := loadDestSeen(g)
	seen = append(seen, destSeenEntry{
		Name: name, Type: d.Type, Host: host, URLHash: hash,
		FirstUsedAt: time.Now().Unix(),
	})
	if err := saveDestSeen(g, seen); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if a := newAudit(g); a != nil {
		_ = a.Append("cli", "destination_first_send", map[string]any{
			"name": name, "type": d.Type, "host": host,
		})
	}
	if !g.JSON {
		fmt.Fprintf(g.Stdout, "Approved %s (host=%s)\n", name, host)
	}
	return 0
}

// enforceFirstSendApproval checks each network destination in c against
// the recorded destinations_seen list. Returns nil when all are approved.
// With autoApprove=true, missing approvals are recorded and an audit
// entry is written; otherwise it returns an error naming the blocked
// destinations so callers can refuse to dispatch.
//
// Local-only destinations (stdout, file) bypass this check — they do not
// trigger the §11.6 first-send concern.
func enforceFirstSendApproval(g *Globals, c *config.Config, autoApprove bool) error {
	if c == nil {
		return nil
	}
	seen, _ := loadDestSeen(g)
	approved := func(name, dtype, dhost, dhash string) bool {
		for _, s := range seen {
			if s.Name == name && s.Type == dtype && s.URLHash == dhash {
				return true
			}
			// Wildcard host-level approval: type+host must both match. The
			// "any" sentinel only authorizes the (type, host) pair, never
			// every network destination indiscriminately.
			if s.Name == "*" && s.URLHash == "any" &&
				s.Type == dtype && s.Host == dhost && dhost != "" {
				return true
			}
		}
		return false
	}
	var newApprovals []destSeenEntry
	var blocked []string
	for _, d := range c.Destinations {
		if d.Type != "http" && d.Type != "webhookscout" {
			continue
		}
		host, hash := destHostAndHash(d.URL, d.APIBase, d.Path)
		if approved(d.Name, d.Type, host, hash) {
			continue
		}
		if autoApprove {
			newApprovals = append(newApprovals, destSeenEntry{
				Name: d.Name, Type: d.Type, Host: host, URLHash: hash,
				FirstUsedAt: time.Now().Unix(),
			})
			continue
		}
		blocked = append(blocked, fmt.Sprintf("%s (%s://%s)", d.Name, d.Type, host))
	}
	if len(blocked) > 0 {
		return fmt.Errorf("destination(s) require approval: %s. Run `scouttrace destination approve <name>` or re-run with --yes",
			joinStrings(blocked, ", "))
	}
	if len(newApprovals) > 0 {
		seen = append(seen, newApprovals...)
		if err := saveDestSeen(g, seen); err != nil {
			return err
		}
		if a := newAudit(g); a != nil {
			for _, e := range newApprovals {
				_ = a.Append("cli", "destination_first_send", map[string]any{
					"name": e.Name, "type": e.Type, "host": e.Host, "auto_approved": true,
				})
			}
		}
	}
	return nil
}

func joinStrings(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}

// destHostAndHash extracts the resolved host portion of a URL plus a
// SHA-256 hash of the full URL (or path/api_base for non-http types).
func destHostAndHash(u, apiBase, path string) (string, string) {
	target := u
	if target == "" {
		target = apiBase
	}
	if target == "" {
		target = path
	}
	if target == "" {
		return "", ""
	}
	host := target
	if parsed, err := url.Parse(target); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	sum := sha256.Sum256([]byte(target))
	return host, hex.EncodeToString(sum[:])
}
