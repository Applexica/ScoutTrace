package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/webhookscout/scouttrace/internal/dispatch"
)

const dispatchPIDFile = "dispatch.pid"

type pidRecord struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	Version   string `json:"version"`
}

// CmdStart runs the optional dispatcher sidecar. It registers a PID file
// at ~/.scouttrace/dispatch.pid; refuses to start a second instance.
func CmdStart(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	once := fs.Bool("once", false, "exit after one drain pass (testing)")
	timeout := fs.Duration("timeout", 0, "exit after this duration (testing); 0 = run forever")
	yes := fs.Bool("yes", false, "auto-approve any first-send destinations")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	c, err := loadConfig(g)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if err := ensureHome(g); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	// AC-S2: refuse to start the dispatcher loop until every network
	// destination is approved. With --yes, auto-approve and audit-log the
	// decision.
	if err := enforceFirstSendApproval(g, c, *yes); err != nil {
		fmt.Fprintln(g.Stderr, "scouttrace start:", err)
		return 1
	}
	pidPath := filepath.Join(g.Home, dispatchPIDFile)
	if existing, err := os.ReadFile(pidPath); err == nil {
		var rec pidRecord
		if json.Unmarshal(existing, &rec) == nil && rec.PID > 0 && pidAlive(rec.PID) {
			fmt.Fprintf(g.Stderr, "scouttrace start: already running (pid=%d)\n", rec.PID)
			return 0
		}
	}
	q, err := openQueue(g, c)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	reg, err := buildRegistry(c, newResolver(g))
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	d := dispatch.New(dispatch.Options{
		Queue: q, Registry: reg, BatchMax: 25,
		PollInterval: 250 * time.Millisecond,
		Backoff: dispatch.BackoffConfig{
			InitialMS:  nonZero(c.Delivery.InitialBackoffMS, 500),
			MaxMS:      nonZero(c.Delivery.MaxBackoffMS, 60_000),
			MaxRetries: nonZero(c.Delivery.MaxRetries, 8),
			Jitter:     c.Delivery.Jitter,
		},
	})
	rec := pidRecord{PID: os.Getpid(), StartedAt: nowRFC3339(), Version: "0.1.0"}
	pidBytes, _ := json.Marshal(rec)
	if err := os.WriteFile(pidPath, pidBytes, 0o600); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	defer os.Remove(pidPath)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *timeout > 0 {
		t := time.AfterFunc(*timeout, cancel)
		defer t.Stop()
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	if !g.JSON {
		fmt.Fprintf(g.Stdout, "scouttrace start: dispatching (pid=%d)\n", rec.PID)
	}
	if *once {
		err = d.RunOnce(runCtx)
	} else {
		err = d.RunForever(runCtx)
	}
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	return 0
}

// CmdStop sends SIGTERM to the running sidecar.
func CmdStop(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	timeout := fs.Duration("timeout", 5*time.Second, "max wait for graceful exit")
	force := fs.Bool("force", false, "send SIGKILL after timeout")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	pidPath := filepath.Join(g.Home, dispatchPIDFile)
	b, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			if !g.JSON {
				fmt.Fprintln(g.Stdout, "scouttrace stop: not running")
			}
			return 0
		}
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	var rec pidRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(rec.PID) {
			os.Remove(pidPath)
			if !g.JSON {
				fmt.Fprintln(g.Stdout, "stopped")
			}
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	if *force {
		_ = p.Signal(syscall.SIGKILL)
		os.Remove(pidPath)
		if !g.JSON {
			fmt.Fprintln(g.Stdout, "force-stopped")
		}
		return 0
	}
	fmt.Fprintln(g.Stderr, "scouttrace stop: timeout; pid still alive")
	return 75
}

// CmdRestart is `stop` then `start`.
func CmdRestart(ctx context.Context, g *Globals, args []string) int {
	if exit := CmdStop(ctx, g, nil); exit != 0 {
		return exit
	}
	return CmdStart(ctx, g, args)
}
