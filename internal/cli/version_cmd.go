package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/webhookscout/scouttrace/internal/version"
)

// CmdVersion prints the build version stamp.
func CmdVersion(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]string{"version": version.Version}, false)
		return 0
	}
	fmt.Fprintln(g.Stdout, version.Version)
	return 0
}
