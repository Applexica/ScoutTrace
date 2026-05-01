package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/webhookscout/scouttrace/internal/audit"
	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/creds"
	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/destinations/filedest"
	"github.com/webhookscout/scouttrace/internal/destinations/httpdest"
	"github.com/webhookscout/scouttrace/internal/destinations/stdoutdest"
	"github.com/webhookscout/scouttrace/internal/destinations/webhookscout"
	"github.com/webhookscout/scouttrace/internal/queue"
)

// withSubJSON registers --json on fs. The returned applier should be called
// after fs.Parse and OR's the flag onto g.JSON, so subcommand-level
// `--json` works regardless of whether the user also passed the global
// `--json` before the verb.
func withSubJSON(fs *flag.FlagSet, g *Globals) func() {
	sub := fs.Bool("json", false, "emit JSON output where supported")
	return func() {
		if *sub {
			g.JSON = true
		}
	}
}

// loadConfig reads the user config or returns a friendly stderr message.
func loadConfig(g *Globals) (*config.Config, error) {
	if _, err := os.Stat(g.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s; run `scouttrace init` first", g.ConfigPath)
	}
	return config.Load(g.ConfigPath)
}

// openQueue returns the on-disk durable queue.
func openQueue(g *Globals, c *config.Config) (*queue.Queue, error) {
	dir := g.queuePath()
	if c != nil && c.Queue.Path != "" {
		dir = c.Queue.Path
	}
	return queue.Open(queue.Options{
		Dir:         dir,
		MaxRowBytes: orInt(c != nil && c.Queue.MaxRowBytes > 0, valIfTrue(c, func(c *config.Config) int { return c.Queue.MaxRowBytes }), 2*1024*1024),
		MaxBytes:    valIfTrue64(c, func(c *config.Config) int64 { return c.Queue.MaxBytes }),
		DropPolicy:  valIfTrueStr(c, func(c *config.Config) string { return c.Queue.DropWhenFull }),
	})
}

func orInt(cond bool, ifTrue, ifFalse int) int {
	if cond {
		return ifTrue
	}
	return ifFalse
}

func valIfTrue(c *config.Config, f func(*config.Config) int) int {
	if c == nil {
		return 0
	}
	return f(c)
}

func valIfTrue64(c *config.Config, f func(*config.Config) int64) int64 {
	if c == nil {
		return 0
	}
	return f(c)
}

func valIfTrueStr(c *config.Config, f func(*config.Config) string) string {
	if c == nil {
		return ""
	}
	return f(c)
}

// buildRegistry constructs the destination registry from config.
func buildRegistry(c *config.Config, res destinations.Resolver) (*destinations.Registry, error) {
	reg := destinations.NewRegistry()
	for i := range c.Destinations {
		d := c.Destinations[i]
		switch d.Type {
		case "http":
			a, err := httpdest.New(httpdest.Config{
				Name:          d.Name,
				URL:           d.URL,
				Headers:       d.Headers,
				AuthHeaderRef: d.AuthHeaderRef,
				TimeoutMS:     d.TimeoutMS,
				UseGzip:       d.UseGzip,
			}, res)
			if err != nil {
				return nil, fmt.Errorf("dest %s: %w", d.Name, err)
			}
			if err := reg.Add(a); err != nil {
				return nil, err
			}
		case "file":
			a, err := filedest.New(filedest.Config{Name: d.Name, Path: d.Path, RotateMB: d.RotateMB, Keep: d.Keep})
			if err != nil {
				return nil, err
			}
			if err := reg.Add(a); err != nil {
				return nil, err
			}
		case "stdout":
			a := stdoutdest.New(stdoutdest.Config{Name: d.Name})
			if err := reg.Add(a); err != nil {
				return nil, err
			}
		case "webhookscout":
			a, err := webhookscout.New(webhookscout.Config{
				Name: d.Name, APIBase: d.APIBase, AgentID: d.AgentID, AuthHeaderRef: d.AuthHeaderRef,
			}, res)
			if err != nil {
				return nil, err
			}
			if err := reg.Add(a); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("dest %s: unknown type %q", d.Name, d.Type)
		}
	}
	return reg, nil
}

// newResolver returns the credential resolver for this run.
func newResolver(g *Globals) destinations.Resolver {
	m := creds.NewMultiStore()
	encPath := filepath.Join(g.Home, "credentials.enc")
	if pp := os.Getenv("SCOUTTRACE_ENCFILE_PASSPHRASE"); pp != "" {
		m.EncFile = creds.NewEncFileStore(encPath, []byte(pp))
	}
	return m
}

// newAudit returns the audit logger, or nil if the home dir is not writable.
// Callers should treat nil as "no-op".
func newAudit(g *Globals) *audit.Logger {
	if err := os.MkdirAll(g.Home, 0o700); err != nil {
		return nil
	}
	l, err := audit.NewLogger(filepath.Join(g.Home, "audit.log"))
	if err != nil {
		return nil
	}
	return l
}

// printJSON writes obj as JSON to w with optional pretty-print.
func printJSON(w io.Writer, obj any, pretty bool) error {
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(obj)
}

// ensureHome makes sure the home directory exists with mode 0700.
func ensureHome(g *Globals) error {
	return os.MkdirAll(g.Home, 0o700)
}

// filterRegistry returns a registry containing only the named adapter.
func filterRegistry(in *destinations.Registry, name string) (*destinations.Registry, error) {
	a, err := in.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("flush: no destination %q in config", name)
	}
	out := destinations.NewRegistry()
	if err := out.Add(a); err != nil {
		return nil, err
	}
	return out, nil
}
