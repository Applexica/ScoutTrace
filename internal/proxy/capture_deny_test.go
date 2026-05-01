package proxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/wire"
)

// TestCaptureLevelDenyDropsArgs verifies AC-R2: a capture policy that says
// capture_args=false MUST prevent args bytes from reaching the queue.
func TestCaptureLevelDenyDropsArgs(t *testing.T) {
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	pol := redact.StrictProfile()
	eng, _ := redact.NewEngine(pol, nil)

	off := false
	capPolicy := &redact.CapturePolicy{Servers: []redact.CaptureServer{
		{NameGlob: "*", CaptureArgs: &off},
	}}
	cw := NewCaptureWorker(CaptureWorker{
		Session:      event.NewSession("filesystem"),
		Engine:       eng,
		Capture:      capPolicy,
		Queue:        q,
		Destination:  "default",
		Host:         "test",
		ScoutVersion: "0.1.0",
	})
	in := make(chan wire.Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cw.Run(ctx, in); close(done) }()

	in <- wire.Frame{Dir: wire.DirHostToUpstream, Bytes: []byte(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"password":"supersecret123"}}}`,
	)}
	in <- wire.Frame{Dir: wire.DirUpstreamToHost, Bytes: []byte(
		`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
	)}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	got, err := q.ClaimPending("default", 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(got))
	}
	if strings.Contains(string(got[0].Payload), "supersecret123") {
		t.Fatalf("captured envelope leaked secret args:\n%s", got[0].Payload)
	}
}
