package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/webhookscout/scouttrace/internal/event"
)

// CmdRun execs a child process with SCOUTTRACE_ENABLED=1 and a session id
// in the environment so SDK shims (post-MVP) can observe it. The MVP
// build is a thin convenience wrapper; it does not intercept the child's
// stdio.
func CmdRun(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(g.Stderr, "run: command required after `--`")
		return 64
	}
	cmd := exec.CommandContext(ctx, rest[0], rest[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = g.Stdout
	cmd.Stderr = g.Stderr
	cmd.Env = append(os.Environ(),
		"SCOUTTRACE_ENABLED=1",
		"SCOUTTRACE_SESSION_ID="+event.NewULID(),
	)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	return 0
}
