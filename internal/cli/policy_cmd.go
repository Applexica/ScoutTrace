package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/webhookscout/scouttrace/internal/redact"
)

// CmdPolicy dispatches policy subcommands: show, lint, test.
func CmdPolicy(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "policy: subcommand required (show|lint|test)")
		return 64
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		return policyShow(g, rest)
	case "lint":
		return policyLint(g, rest)
	case "test":
		return policyTest(g, rest)
	}
	fmt.Fprintf(g.Stderr, "policy: unknown subcommand %q\n", sub)
	return 64
}

func policyShow(g *Globals, args []string) int {
	fs := flag.NewFlagSet("policy show", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	profile := fs.String("profile", "standard", "built-in profile name")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	pol := redact.ByName(*profile)
	if pol == nil {
		fmt.Fprintf(g.Stderr, "policy: unknown profile %q\n", *profile)
		return 64
	}
	if err := pol.Validate(); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	out := map[string]any{
		"name":  pol.Name,
		"hash":  pol.Hash(),
		"rules": pol.Rules,
	}
	return printOrFail(g, out)
}

func policyLint(g *Globals, args []string) int {
	fs := flag.NewFlagSet("policy lint", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	path := fs.String("path", "", "path to policy JSON")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if *path == "" {
		fmt.Fprintln(g.Stderr, "policy lint: --path required")
		return 64
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 77
	}
	var pol redact.Policy
	if err := json.Unmarshal(b, &pol); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 78
	}
	if err := pol.Validate(); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 78
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]any{"ok": true, "rules": len(pol.Rules)}, false)
	} else {
		fmt.Fprintf(g.Stdout, "policy %s OK (%d rules)\n", pol.Name, len(pol.Rules))
	}
	return 0
}

// policyTest runs a profile against a JSON file and prints the redacted
// output side-by-side with which patterns hit. Exit code is 0 even on
// matches; users care about output content, not exit semantics here.
func policyTest(g *Globals, args []string) int {
	fs := flag.NewFlagSet("policy test", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	profile := fs.String("profile", "standard", "built-in profile name")
	path := fs.String("path", "", "path to JSON to test (default: stdin)")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	pol := redact.ByName(*profile)
	if pol == nil {
		fmt.Fprintf(g.Stderr, "policy: unknown profile %q\n", *profile)
		return 64
	}
	eng, err := redact.NewEngine(pol, nil)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	var input []byte
	if *path != "" {
		input, err = os.ReadFile(*path)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 77
		}
	} else {
		input, err = io.ReadAll(g.Stdin)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
	}
	out, res, err := eng.Apply(input)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if g.JSON {
		_ = printJSON(g.Stdout, map[string]any{
			"redacted": json.RawMessage(out),
			"applied":  res.RulesApplied,
			"fields":   res.FieldsRedacted,
		}, true)
		return 0
	}
	fmt.Fprintf(g.Stdout, "Profile: %s (%s)\n", pol.Name, pol.Hash()[:19])
	fmt.Fprintf(g.Stdout, "Rules applied: %v\n", res.RulesApplied)
	fmt.Fprintf(g.Stdout, "Fields redacted: %v\n", res.FieldsRedacted)
	fmt.Fprintln(g.Stdout, "Redacted JSON:")
	g.Stdout.Write(out)
	fmt.Fprintln(g.Stdout)
	return 0
}

func printOrFail(g *Globals, obj any) int {
	if err := printJSON(g.Stdout, obj, true); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	return 0
}
