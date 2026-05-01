package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/wire"
)

// TestRunReturnsWhenUpstreamCrashesEvenIfHostStdinOpen verifies the
// blocker fix: if the upstream exits before the host sends another frame,
// proxy.Run must still return promptly (the host-side reader is closed
// via the io.Pipe shim).
func TestRunReturnsWhenUpstreamCrashesEvenIfHostStdinOpen(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	pol := redact.StrictProfile()
	eng, _ := redact.NewEngine(pol, nil)
	cw := NewCaptureWorker(CaptureWorker{
		Session:      event.NewSession("crashy"),
		Engine:       eng,
		Queue:        q,
		Destination:  "default",
		Host:         "test",
		ScoutVersion: "0.1.0",
	})

	// Host stdin: a pipe that we never write to, never close.
	hostStdinR, _ := io.Pipe()
	defer hostStdinR.Close()
	var hostStdout bytes.Buffer

	capCh := make(chan wire.Frame, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		exit int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		exit, err := Run(ctx, Options{
			ServerName: "crashy",
			Upstream:   []string{"sh", "-c", "exit 42"},
			HostStdin:  hostStdinR,
			HostStdout: &hostStdout,
			Stderr:     io.Discard,
			CaptureCh:  capCh,
			Worker:     cw,
			GraceMS:    250,
		})
		done <- result{exit: exit, err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("proxy.Run: %v", r.err)
		}
		if r.exit != 42 {
			t.Errorf("exit = %d, want 42", r.exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("proxy.Run did not return within 5s after upstream exited")
	}

	// server_crashed envelope must be queued.
	got, err := q.ClaimPending("default", 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("queued %d records, want 1", len(got))
	}
	var env map[string]any
	if err := json.Unmarshal(got[0].Payload, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env["schema"] != event.CrashSchemaVersion {
		t.Errorf("schema = %v, want %s", env["schema"], event.CrashSchemaVersion)
	}
	if env["exit_code"].(float64) != 42 {
		t.Errorf("exit_code = %v, want 42", env["exit_code"])
	}
}
