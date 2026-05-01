package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/webhookscout/scouttrace/internal/config"
)

// CmdConfig dispatches config subcommands: show, validate, set, get.
func CmdConfig(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "config: subcommand required (show|validate|get|set)")
		return 64
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		return configShow(g, rest)
	case "validate":
		return configValidate(g, rest)
	case "get":
		return configGet(g, rest)
	case "set":
		return configSet(g, rest)
	}
	fmt.Fprintf(g.Stderr, "config: unknown subcommand %q\n", sub)
	return 64
}

func configShow(g *Globals, args []string) int {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
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
	return printOrFail(g, c)
}

func configValidate(g *Globals, args []string) int {
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	path := fs.String("config", "", "config path (default: --config global)")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	target := *path
	if target == "" {
		target = g.ConfigPath
	}
	b, err := os.ReadFile(target)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 77
	}
	if _, err := config.Parse(b); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return classifyConfigExit(err)
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]bool{"ok": true}, false)
	} else {
		fmt.Fprintln(g.Stdout, "OK")
	}
	return 0
}

func classifyConfigExit(err error) int {
	if e, ok := err.(*config.Error); ok {
		switch e.Code {
		case config.ErrCodePlaintextAuth, config.ErrCodeConfigRefInvalid,
			config.ErrCodeDestNotFound, config.ErrCodeDestDuplicate:
			return 78
		}
	}
	return 78
}

func configGet(g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "config get: key required (e.g. default_destination)")
		return 64
	}
	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	out, err := lookupConfigPath(c, args[0])
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	return printOrFail(g, out)
}

func configSet(g *Globals, args []string) int {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(g.Stderr, "config set: usage: scouttrace config set <key> <value>")
		return 64
	}
	key, value := rest[0], rest[1]
	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if err := setConfigPath(c, key, value); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 64
	}
	if err := c.Validate(); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 78
	}
	if err := config.Save(g.ConfigPath, c); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if a := newAudit(g); a != nil {
		_ = a.Append("cli", "config_set", map[string]any{"key": key})
	}
	if !g.JSON {
		fmt.Fprintln(g.Stdout, "OK")
	}
	return 0
}

// lookupConfigPath supports a small subset of dotted paths. JSON-marshal
// the config and walk the tree.
func lookupConfigPath(c *config.Config, dotted string) (any, error) {
	b, _ := json.Marshal(c)
	var tree any
	_ = json.Unmarshal(b, &tree)
	cur := tree
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config: cannot descend into %q (not an object)", seg)
		}
		v, ok := m[seg]
		if !ok {
			return nil, fmt.Errorf("config: no key %q", seg)
		}
		cur = v
	}
	return cur, nil
}

// setConfigPath supports a small subset of writable keys for the MVP.
func setConfigPath(c *config.Config, key, value string) error {
	switch key {
	case "default_destination":
		c.DefaultDestination = value
	case "redaction.profile":
		c.Redaction.Profile = value
	case "delivery.initial_backoff_ms":
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Delivery.InitialBackoffMS = v
	case "delivery.max_backoff_ms":
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Delivery.MaxBackoffMS = v
	case "queue.path":
		c.Queue.Path = value
	default:
		return fmt.Errorf("config: key %q is not settable via `config set` in MVP", key)
	}
	return nil
}
