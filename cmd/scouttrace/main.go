// Command scouttrace is the single binary entrypoint that ScoutTrace's
// MCP-host patches, dispatcher sidecar, and CLI tooling all dispatch into.
//
// All work is delegated to internal/cli; this file is intentionally tiny.
package main

import (
	"os"

	"github.com/webhookscout/scouttrace/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
