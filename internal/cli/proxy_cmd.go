package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/dispatch"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/proxy"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/wire"
)

// capturePolicyFromConfig converts the user's capture.servers config into
// a redact.CapturePolicy that the worker can consult before any args/result
// bytes enter the in-memory envelope (the AC-R2 backstop).
func capturePolicyFromConfig(c *config.Config) *redact.CapturePolicy {
	if c == nil {
		return nil
	}
	out := &redact.CapturePolicy{}
	for _, s := range c.Capture.Servers {
		out.Servers = append(out.Servers, redact.CaptureServer{
			NameGlob:      s.NameGlob,
			CaptureArgs:   s.CaptureArgs,
			CaptureResult: s.CaptureResult,
		})
	}
	return out
}

// CmdProxy implements `scouttrace proxy --server-name X -- <upstream>`.
func CmdProxy(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	serverName := fs.String("server-name", "", "logical name for this MCP server")
	noCapture := fs.Bool("no-capture", false, "byte-tee only; skip capture pipeline")
	failClosed := fs.Bool("fail-closed", false, "exit if capture cannot start")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(g.Stderr, "proxy: upstream command required after `--`")
		return 64
	}
	if *serverName == "" {
		*serverName = filepath.Base(rest[0])
	}
	c, _ := loadConfig(g) // optional; proxy can run with defaults

	// Set up redaction.
	profile := "standard"
	if c != nil && c.Redaction.Profile != "" {
		profile = c.Redaction.Profile
	}
	pol := redact.ByName(profile)
	if pol == nil {
		fmt.Fprintln(g.Stderr, "proxy: unknown redaction profile:", profile)
		return 78
	}
	eng, engErr := redact.NewEngine(pol, nil)
	q, qErr := openQueue(g, c)
	if engErr != nil || qErr != nil {
		if *failClosed {
			fmt.Fprintln(g.Stderr, "proxy: --fail-closed: capture init failed:", firstErr(engErr, qErr))
			return 2
		}
		fmt.Fprintln(g.Stderr, "proxy: capture disabled:", firstErr(engErr, qErr))
	}

	dest := "default"
	if c != nil && c.DefaultDestination != "" {
		dest = c.DefaultDestination
	}

	capturePolicy := capturePolicyFromConfig(c)
	captureOK := eng != nil && q != nil
	var worker *proxy.CaptureWorker
	if captureOK {
		sess := event.NewSession(*serverName)
		var maxArg, maxRes int
		if c != nil {
			maxArg = c.Capture.MaxArgBytes
			maxRes = c.Capture.MaxResultBytes
		}
		live, liveSource := c.LiveLookup()
		worker = proxy.NewCaptureWorker(proxy.CaptureWorker{
			Session:        sess,
			Engine:         eng,
			Capture:        capturePolicy,
			Queue:          q,
			Destination:    dest,
			Host:           "unknown",
			ScoutVersion:   "0.1.0",
			MaxArgBytes:    maxArg,
			MaxResultBytes: maxRes,
			StaticPrices:   c.StaticPriceLookup(),
			LivePrices:     live,
			LiveSource:     liveSource,
		})
	}

	var capCh chan wire.Frame
	var workerArg *proxy.CaptureWorker
	if !*noCapture && captureOK {
		capCh = make(chan wire.Frame, 1024)
		workerArg = worker
	}

	// Spawn an in-process dispatcher when capture is on and no sidecar
	// holds the lock. We don't try to be perfectly atomic about the lock —
	// the queue's pending→inflight rename is atomic, so concurrent
	// dispatchers can't double-deliver an event id.
	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()
	dispatcherDrain := func() {}
	if captureOK && !*noCapture {
		dispatcherDrain = startInProxyDispatcher(dctx, g, c, q)
	}

	exit, err := proxy.Run(ctx, proxy.Options{
		ServerName: *serverName,
		Upstream:   rest,
		Stdin:      g.Stdin,
		Stdout:     g.Stdout,
		Stderr:     g.Stderr,
		CaptureCh:  capCh,
		Worker:     workerArg,
	})
	if err != nil {
		fmt.Fprintln(g.Stderr, "proxy:", err)
		if exit == 0 {
			exit = 1
		}
	}
	dcancel()
	dispatcherDrain()
	return exit
}

// startInProxyDispatcher launches a dispatcher goroutine that drains the
// queue while the proxy is alive. Returns a "drain" function the caller
// runs after the wire path exits, which performs one final synchronous
// dispatch pass so events queued near the end are delivered before exit.
func startInProxyDispatcher(ctx context.Context, g *Globals, c *config.Config, q *queue.Queue) func() {
	if c == nil || q == nil {
		return func() {}
	}
	if err := enforceFirstSendApproval(g, c, false); err != nil {
		fmt.Fprintf(g.Stderr, "proxy: in-process dispatcher disabled — %v\n", err)
		return func() {}
	}
	reg, err := buildRegistry(c, newResolver(g))
	if err != nil {
		fmt.Fprintf(g.Stderr, "proxy: dispatcher build failed: %v\n", err)
		return func() {}
	}
	d := dispatch.New(dispatch.Options{
		Queue:        q,
		Registry:     reg,
		BatchMax:     25,
		PollInterval: 250 * time.Millisecond,
		Backoff: dispatch.BackoffConfig{
			InitialMS:  nonZero(c.Delivery.InitialBackoffMS, 500),
			MaxMS:      nonZero(c.Delivery.MaxBackoffMS, 60_000),
			MaxRetries: nonZero(c.Delivery.MaxRetries, 8),
			Jitter:     c.Delivery.Jitter,
		},
	})
	go func() { _ = d.RunForever(ctx) }()
	return func() {
		// One synchronous pass on a fresh, short-lived context.
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = d.RunOnce(shutdown)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
