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

func TestCaptureMaxArgBytesEnforced(t *testing.T) {
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	pol := redact.StandardProfile()
	eng, _ := redact.NewEngine(pol, nil)
	cw := NewCaptureWorker(CaptureWorker{
		Session:      event.NewSession("filesystem"),
		Engine:       eng,
		Queue:        q,
		Destination:  "default",
		Host:         "test",
		ScoutVersion: "0.1.0",
		MaxArgBytes:  16,
	})
	in := make(chan wire.Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cw.Run(ctx, in); close(done) }()

	in <- wire.Frame{Dir: wire.DirHostToUpstream, Bytes: []byte(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"big":"` +
			strings.Repeat("X", 1024) + `"}}}`,
	)}
	in <- wire.Frame{Dir: wire.DirUpstreamToHost, Bytes: []byte(
		`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
	)}
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	got, err := q.ClaimPending("default", 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("queued %d, want 1", len(got))
	}
	payload := string(got[0].Payload)
	if !strings.Contains(payload, `"args_truncated":true`) {
		t.Errorf("args_truncated flag missing:\n%s", payload)
	}
	if !strings.Contains(payload, "truncated:") {
		t.Errorf("truncation marker missing:\n%s", payload)
	}
	// Original bytes captured.
	if !strings.Contains(payload, `"args_bytes_original"`) {
		t.Errorf("args_bytes_original missing:\n%s", payload)
	}
	if strings.Contains(payload, strings.Repeat("X", 100)) {
		t.Errorf("oversize args bytes leaked into payload")
	}
}

func TestCaptureMaxResultBytesEnforced(t *testing.T) {
	dir := t.TempDir()
	q, _ := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	pol := redact.StandardProfile()
	eng, _ := redact.NewEngine(pol, nil)
	cw := NewCaptureWorker(CaptureWorker{
		Session:        event.NewSession("filesystem"),
		Engine:         eng,
		Queue:          q,
		Destination:    "default",
		ScoutVersion:   "0.1.0",
		MaxResultBytes: 16,
	})
	in := make(chan wire.Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cw.Run(ctx, in); close(done) }()

	in <- wire.Frame{Dir: wire.DirHostToUpstream, Bytes: []byte(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","arguments":{}}}`,
	)}
	in <- wire.Frame{Dir: wire.DirUpstreamToHost, Bytes: []byte(
		`{"jsonrpc":"2.0","id":2,"result":{"big":"` + strings.Repeat("Y", 4096) + `"}}`,
	)}
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	got, _ := q.ClaimPending("default", 10)
	if len(got) != 1 {
		t.Fatalf("queued %d", len(got))
	}
	payload := string(got[0].Payload)
	if !strings.Contains(payload, `"result_truncated":true`) {
		t.Errorf("result_truncated flag missing:\n%s", payload)
	}
	if strings.Contains(payload, strings.Repeat("Y", 100)) {
		t.Errorf("oversize result leaked into payload")
	}
}
