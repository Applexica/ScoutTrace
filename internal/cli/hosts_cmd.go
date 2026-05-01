package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/hosts"
)

// CmdHosts dispatches host subcommands: list, patch, unpatch.
func CmdHosts(ctx context.Context, g *Globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(g.Stderr, "hosts: subcommand required (list|patch|unpatch)")
		return 64
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return hostsList(g, rest)
	case "patch":
		return hostsPatch(g, rest)
	case "unpatch":
		return hostsUnpatch(g, rest)
	}
	fmt.Fprintf(g.Stderr, "hosts: unknown subcommand %q\n", sub)
	return 64
}

func hostsList(g *Globals, args []string) int {
	fs := flag.NewFlagSet("hosts list", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	type hostInfo struct {
		ID         string `json:"id"`
		Display    string `json:"display"`
		Installed  bool   `json:"installed"`
		ConfigPath string `json:"config_path"`
		Parsable   bool   `json:"parsable"`
	}
	infos := []hostInfo{}
	for id, h := range hosts.Registry() {
		dr, _ := hosts.Detect(h)
		infos = append(infos, hostInfo{
			ID: id, Display: h.DisplayName,
			Installed: dr.Installed, ConfigPath: dr.ConfigPath, Parsable: dr.Parsable,
		})
	}
	if g.JSON {
		_ = printJSON(g.Stdout, infos, true)
		return 0
	}
	for _, i := range infos {
		mark := "[ ]"
		if i.Installed {
			mark = "[x]"
		}
		fmt.Fprintf(g.Stdout, "%s %-16s %s\n", mark, i.ID, i.ConfigPath)
	}
	return 0
}

func hostsPatch(g *Globals, args []string) int {
	fs := flag.NewFlagSet("hosts patch", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	hostID := fs.String("host", "", "host id (e.g. claude-desktop)")
	cfgPath := fs.String("config-path", "", "override host config path")
	servers := fs.String("servers", "", "comma-separated server names; empty = all")
	force := fs.Bool("force", false, "ignore drift since last patch")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *hostID == "" {
		fmt.Fprintln(g.Stderr, "hosts patch: --host required")
		return 64
	}
	h, err := hosts.LookupHost(*hostID)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 64
	}
	c, _ := loadConfig(g)
	path := *cfgPath
	if path == "" {
		path, err = h.DefaultPath()
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
	}
	bak := filepath.Join(g.Home, "backups", *hostID)
	var srvs []string
	if *servers != "" {
		for _, s := range strings.Split(*servers, ",") {
			srvs = append(srvs, strings.TrimSpace(s))
		}
	}
	rec := ""
	if c != nil {
		if hr, ok := c.Hosts[*hostID]; ok {
			rec = hr.LastPatchedHash
		}
	}
	res, err := hosts.Patch(h, path, srvs, g.ScoutBinary, bak, *force, rec)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return classifyExit(err)
	}
	// Update config bookkeeping if config exists.
	if c != nil {
		if c.Hosts == nil {
			c.Hosts = map[string]config.HostRef{}
		}
		c.Hosts[*hostID] = config.HostRef{
			ConfigPath:      path,
			LastPatchedAt:   res.WrittenAt.Format("2006-01-02T15-04-05Z"),
			LastPatchedHash: res.HashAfter,
			BackupPath:      res.BackupPath,
		}
		_ = config.Save(g.ConfigPath, c)
	}
	if a := newAudit(g); a != nil {
		_ = a.Append("cli", "hosts_patch", map[string]any{
			"host": *hostID, "servers": srvs, "backup": res.BackupPath,
		})
	}
	if g.JSON {
		_ = printJSON(g.Stdout, res, true)
		return 0
	}
	fmt.Fprintf(g.Stdout, "Patched %s (%d servers). Backup: %s\n", *hostID, len(res.Servers), res.BackupPath)
	return 0
}

func hostsUnpatch(g *Globals, args []string) int {
	fs := flag.NewFlagSet("hosts unpatch", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	hostID := fs.String("host", "", "host id")
	cfgPath := fs.String("config-path", "", "override host config path")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *hostID == "" {
		fmt.Fprintln(g.Stderr, "hosts unpatch: --host required")
		return 64
	}
	h, err := hosts.LookupHost(*hostID)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 64
	}
	path := *cfgPath
	if path == "" {
		path, err = h.DefaultPath()
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			return 1
		}
	}
	res, err := hosts.Unpatch(h, path)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if a := newAudit(g); a != nil {
		_ = a.Append("cli", "hosts_unpatch", map[string]any{"host": *hostID, "servers": res.Servers})
	}
	if g.JSON {
		_ = printJSON(g.Stdout, res, true)
		return 0
	}
	fmt.Fprintf(g.Stdout, "Unpatched %s (%d servers)\n", *hostID, len(res.Servers))
	return 0
}

func classifyExit(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "E_HOST_CONFIG_DRIFT"):
		return 65
	case strings.Contains(msg, "E_HOST_MARKER_MISSING"):
		return 65
	case strings.Contains(msg, "permission"):
		return 77
	}
	return 1
}
