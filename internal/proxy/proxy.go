package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/wire"
)

// Options drives a single proxy lifecycle.
type Options struct {
	ServerName string
	Upstream   []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	HostStdin  io.Reader // overridable for tests
	HostStdout io.Writer
	CaptureCh  chan wire.Frame
	Worker     *CaptureWorker
	OnReady    func(*event.SessionState)
	Logger     func(format string, args ...any)
	GraceMS    int
}

// Run executes the proxy until either the host or upstream closes its side.
// Returns the upstream exit code if known (or 0 / 1).
func Run(ctx context.Context, opts Options) (int, error) {
	if len(opts.Upstream) == 0 {
		return 1, errors.New("proxy: upstream command required")
	}
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	if opts.HostStdin == nil {
		opts.HostStdin = opts.Stdin
	}
	if opts.HostStdout == nil {
		opts.HostStdout = opts.Stdout
	}
	if opts.GraceMS == 0 {
		opts.GraceMS = 3000
	}
	cmd := exec.CommandContext(ctx, opts.Upstream[0], opts.Upstream[1:]...)
	cmd.Stderr = opts.Stderr
	upStdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, fmt.Errorf("proxy: stdin pipe: %w", err)
	}
	upStdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, fmt.Errorf("proxy: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("proxy: upstream start: %w", err)
	}
	stats := &wire.TeeStats{}
	captureEnabled := opts.Worker != nil && opts.CaptureCh != nil
	var capForWire chan wire.Frame
	if captureEnabled {
		capForWire = opts.CaptureCh
	}
	hostToUp := &wire.Tee{Src: opts.HostStdin, Dst: upStdin, Dir: wire.DirHostToUpstream, CapCh: capForWire, Stats: stats}
	upToHost := &wire.Tee{Src: upStdout, Dst: opts.HostStdout, Dir: wire.DirUpstreamToHost, CapCh: capForWire, Stats: stats}

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	workerDone := make(chan struct{})
	if captureEnabled {
		go func() {
			defer close(workerDone)
			opts.Worker.Run(workerCtx, opts.CaptureCh)
		}()
	} else {
		close(workerDone)
	}

	if opts.OnReady != nil && opts.Worker != nil {
		opts.OnReady(opts.Worker.Session)
	}

	// Indirection layer: host stdin → io.Pipe → Tee.
	// Closing pipeR unblocks the Tee's Read; we use this when the upstream
	// dies before the host has sent another frame.
	pipeR, pipeW := io.Pipe()
	hostToUp.Src = pipeR
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		_, _ = io.Copy(pipeW, opts.HostStdin)
		_ = pipeW.Close()
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := hostToUp.Run()
		_ = upStdin.Close()
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		err := upToHost.Run()
		// Upstream's stdout closed → upstream is gone. Unblock the host→up
		// reader so we don't hang forever waiting for the host to send one
		// more frame.
		_ = pipeR.Close()
		errCh <- err
	}()

	wg.Wait()
	close(errCh)
	// Allow the pump to exit if it hasn't already.
	_ = pipeW.Close()

	// Wire goroutines done. Close the capture channel to signal the worker
	// no more frames are coming, then wait up to GraceMS for it to drain.
	if captureEnabled {
		close(opts.CaptureCh)
	}
	graceTimer := time.NewTimer(time.Duration(opts.GraceMS) * time.Millisecond)
	defer graceTimer.Stop()
	select {
	case <-workerDone:
	case <-graceTimer.C:
	}
	workerCancel()
	waitCmd(cmd, opts.Logger)
	exit := cmdExit(cmd)
	if exit != 0 && captureEnabled {
		opts.Worker.EmitServerCrashed(exit, "", nil, "")
	}
	return exit, nil
}

func waitCmd(cmd *exec.Cmd, logger func(string, ...any)) {
	if cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		logger("proxy: upstream did not exit; killing")
		_ = cmd.Process.Kill()
		<-done
	}
}

func cmdExit(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 0
	}
	return cmd.ProcessState.ExitCode()
}

// MustGetenv panics if the supplied env variable is missing. Used for early
// startup invariants.
func MustGetenv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic("missing env: " + name)
	}
	return v
}
