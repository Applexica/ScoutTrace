package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/hosts"
)

// CmdUndo restores the most recent host backup.
func CmdUndo(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	hostID := fs.String("host", "", "host id (defaults to applying to all configured hosts)")
	cfgPath := fs.String("config-path", "", "override host config path")
	all := fs.Bool("all", false, "iterate every host that has a backup directory")
	list := fs.Bool("list", false, "list available backups and exit")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	c, _ := loadConfig(g)
	targets := []string{}
	if *hostID != "" {
		targets = []string{*hostID}
	} else if *all {
		// All hosts under ~/.scouttrace/backups/.
		entries, _ := readSubdirs(filepath.Join(g.Home, "backups"))
		targets = entries
	} else if c != nil {
		for h := range c.Hosts {
			targets = append(targets, h)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(g.Stderr, "undo: no host targets; pass --host or --all")
		return 64
	}
	exit := 0
	for _, id := range targets {
		h, err := hosts.LookupHost(id)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			exit = 1
			continue
		}
		path := *cfgPath
		if path == "" && c != nil {
			if hr, ok := c.Hosts[id]; ok {
				path = hr.ConfigPath
			}
		}
		if path == "" {
			path, err = h.DefaultPath()
			if err != nil {
				fmt.Fprintln(g.Stderr, err)
				exit = 1
				continue
			}
		}
		bak := filepath.Join(g.Home, "backups", id)
		if *list {
			items, _ := listBackups(bak)
			for _, b := range items {
				fmt.Fprintln(g.Stdout, b)
			}
			continue
		}
		used, err := hosts.UndoFromBackup(h, path, bak)
		if err != nil {
			fmt.Fprintln(g.Stderr, err)
			exit = 1
			continue
		}
		if c != nil {
			if hr, ok := c.Hosts[id]; ok {
				hr.LastPatchedAt = ""
				hr.LastPatchedHash = ""
				c.Hosts[id] = hr
			}
		}
		if a := newAudit(g); a != nil {
			_ = a.Append("cli", "undo", map[string]any{"host": id, "backup": used})
		}
		fmt.Fprintf(g.Stdout, "Restored %s from %s\n", id, used)
	}
	if c != nil {
		_ = config.Save(g.ConfigPath, c)
	}
	return exit
}
