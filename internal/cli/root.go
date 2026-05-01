// Package cli wires the scouttrace top-level CLI. Each subcommand owns a
// dedicated `Cmd*` function with its own flag.FlagSet so `--help` text
// stays focused. A tiny dispatcher in Run() picks the right subcommand
// based on argv[1] and delegates the remainder.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/webhookscout/scouttrace/internal/version"
)

// Globals exposes shared CLI options. They are populated by the dispatcher
// before any subcommand runs.
type Globals struct {
	Home        string // ~/.scouttrace
	ConfigPath  string // override via --config
	Verbose     int
	JSON        bool
	Stdout      io.Writer
	Stderr      io.Writer
	Stdin       io.Reader
	Args        []string
	Now         func() string
	ScoutBinary string // path to this binary; used by hosts patch
}

// Run is the entry point. argv must include the program name at [0].
// Returns the process exit code.
func Run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		printRootHelp(stdout)
		return 0
	}
	g, rest, err := parseGlobals(argv[1:], stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 64
	}
	if len(rest) == 0 || rest[0] == "help-root" {
		printRootHelp(stdout)
		return 0
	}
	verb, args := rest[0], rest[1:]
	cmd, ok := commandByName(verb)
	if !ok {
		fmt.Fprintf(stderr, "scouttrace: unknown command %q\n", verb)
		printRootHelp(stderr)
		return 64
	}
	return cmd.Run(context.Background(), g, args)
}

// Command is a subcommand entry.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, g *Globals, args []string) int
}

func commandByName(name string) (Command, bool) {
	for _, c := range allCommands() {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

func allCommands() []Command {
	return []Command{
		{Name: "init", Summary: "set up ScoutTrace (non-interactive with --yes)", Run: CmdInit},
		{Name: "proxy", Summary: "stdio proxy: wraps an MCP server", Run: CmdProxy},
		{Name: "run", Summary: "exec a child with SCOUTTRACE_ENABLED=1", Run: CmdRun},
		{Name: "status", Summary: "show queue + dispatcher status", Run: CmdStatus},
		{Name: "doctor", Summary: "self-check: config + queue + destinations", Run: CmdDoctor},
		{Name: "tail", Summary: "stream queue events", Run: CmdTail},
		{Name: "replay", Summary: "replay events from a file or dead lane", Run: CmdReplay},
		{Name: "preview", Summary: "preview redaction on a synthetic event", Run: CmdPreview},
		{Name: "policy", Summary: "manage redaction policies", Run: CmdPolicy},
		{Name: "hosts", Summary: "list/patch/unpatch MCP hosts", Run: CmdHosts},
		{Name: "config", Summary: "show/validate/set config", Run: CmdConfig},
		{Name: "queue", Summary: "queue operations (list/inject/flush/stats)", Run: CmdQueue},
		{Name: "flush", Summary: "alias for `queue flush`", Run: CmdFlush},
		{Name: "destination", Summary: "list/approve destinations", Run: CmdDestination},
		{Name: "start", Summary: "start dispatcher sidecar", Run: CmdStart},
		{Name: "stop", Summary: "stop dispatcher sidecar", Run: CmdStop},
		{Name: "restart", Summary: "stop+start the dispatcher", Run: CmdRestart},
		{Name: "undo", Summary: "restore most recent host backup", Run: CmdUndo},
		{Name: "version", Summary: "print version", Run: CmdVersion},
	}
}

func printRootHelp(w io.Writer) {
	fmt.Fprintf(w, "scouttrace %s — observability for MCP tools\n\n", version.Version)
	fmt.Fprintln(w, "Usage: scouttrace [global flags] <command> [flags]")
	fmt.Fprintln(w, "\nGlobal flags:")
	fmt.Fprintln(w, "  --home <path>     ScoutTrace data dir (default ~/.scouttrace)")
	fmt.Fprintln(w, "  --config <path>   override config file path")
	fmt.Fprintln(w, "  --json            emit JSON where supported")
	fmt.Fprintln(w, "  -v / -vv          increase verbosity")
	fmt.Fprintln(w, "\nCommands:")
	cmds := allCommands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-12s  %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "\nRun `scouttrace <command> --help` for command-specific flags.")
}

func parseGlobals(args []string, stdin io.Reader, stdout, stderr io.Writer) (*Globals, []string, error) {
	g := &Globals{Stdin: stdin, Stdout: stdout, Stderr: stderr}
	rest := []string{}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--home":
			if i+1 >= len(args) {
				return nil, nil, errors.New("--home requires a value")
			}
			g.Home = args[i+1]
			i += 2
		case a == "--config":
			if i+1 >= len(args) {
				return nil, nil, errors.New("--config requires a value")
			}
			g.ConfigPath = args[i+1]
			i += 2
		case a == "--json":
			g.JSON = true
			i++
		case a == "-v":
			g.Verbose = 1
			i++
		case a == "-vv":
			g.Verbose = 2
			i++
		case a == "--help" || a == "-h":
			rest = append(rest, "help-root")
			i++
		default:
			rest = append(rest, args[i:]...)
			i = len(args)
		}
	}
	if g.Home == "" {
		if envHome := os.Getenv("SCOUTTRACE_HOME"); envHome != "" {
			g.Home = envHome
		}
	}
	if g.Home == "" {
		home, _ := os.UserHomeDir()
		g.Home = filepath.Join(home, ".scouttrace")
	}
	if g.ConfigPath == "" {
		if envConfig := os.Getenv("SCOUTTRACE_CONFIG"); envConfig != "" {
			g.ConfigPath = envConfig
		} else {
			g.ConfigPath = resolveConfigPath(g.Home)
		}
	}
	if exe, err := os.Executable(); err == nil {
		g.ScoutBinary = exe
	}
	return g, rest, nil
}

// queuePath returns the configured queue dir, or a default under home.
func (g *Globals) queuePath() string {
	return filepath.Join(g.Home, "queue")
}

// resolveConfigPath picks the canonical config path under home. We prefer
// `config.yaml` (the documented default) but transparently use existing
// legacy `config.json` files so older installs keep working.
func resolveConfigPath(home string) string {
	yaml := filepath.Join(home, "config.yaml")
	json := filepath.Join(home, "config.json")
	if _, err := os.Stat(yaml); err == nil {
		return yaml
	}
	if _, err := os.Stat(json); err == nil {
		return json
	}
	return yaml
}
