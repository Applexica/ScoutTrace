package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/version"
)

// statusReport mirrors the §17.2 snapshot.
type statusReport struct {
	Version      string         `json:"version"`
	Home         string         `json:"home"`
	ConfigPath   string         `json:"config_path"`
	Queue        queue.Stats    `json:"queue"`
	Dispatcher   dispatcherInfo `json:"dispatcher"`
	Destinations []string       `json:"destinations,omitempty"`
}

type dispatcherInfo struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// CmdStatus prints a status report.
func CmdStatus(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if err := ensureHome(g); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	c, _ := loadConfig(g) // optional
	report := statusReport{
		Version: version.Version, Home: g.Home, ConfigPath: g.ConfigPath,
	}
	q, err := openQueue(g, c)
	if err == nil {
		st, _ := q.Stats()
		report.Queue = st
	}
	if c != nil {
		for _, d := range c.Destinations {
			report.Destinations = append(report.Destinations, d.Name)
		}
	}
	report.Dispatcher = readDispatcherInfo(g.Home)
	if g.JSON {
		_ = printJSON(g.Stdout, report, true)
		return 0
	}
	fmt.Fprintf(g.Stdout, "ScoutTrace %s\n", report.Version)
	fmt.Fprintf(g.Stdout, "Home: %s\n", report.Home)
	fmt.Fprintf(g.Stdout, "Queue: pending=%d inflight=%d dead=%d\n",
		report.Queue.Pending, report.Queue.Inflight, report.Queue.Dead)
	if report.Dispatcher.Running {
		fmt.Fprintf(g.Stdout, "Dispatcher: running pid=%d started=%s\n",
			report.Dispatcher.PID, report.Dispatcher.StartedAt)
	} else {
		fmt.Fprintln(g.Stdout, "Dispatcher: not running (queued events flush via proxy)")
	}
	if len(report.Destinations) > 0 {
		fmt.Fprintf(g.Stdout, "Destinations: %d configured\n", len(report.Destinations))
	}
	return 0
}

// readDispatcherInfo inspects ~/.scouttrace/dispatch.pid.
func readDispatcherInfo(home string) dispatcherInfo {
	pidPath := filepath.Join(home, "dispatch.pid")
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return dispatcherInfo{}
	}
	var info struct {
		PID       int    `json:"pid"`
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal(b, &info); err != nil {
		// Fallback: plain pid integer.
		if pid, err := strconv.Atoi(string(b)); err == nil {
			return dispatcherInfo{Running: pidAlive(pid), PID: pid}
		}
		return dispatcherInfo{}
	}
	return dispatcherInfo{
		Running:   pidAlive(info.PID),
		PID:       info.PID,
		StartedAt: info.StartedAt,
	}
}

// pidAlive returns true if signalling 0 succeeds (Unix). On Windows,
// FindProcess returns a process handle for live and dead pids alike, so
// callers should treat the return value as a hint.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// nowRFC3339 returns the current time formatted for status output.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
