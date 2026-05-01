package proxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
	"github.com/webhookscout/scouttrace/internal/wire"
)

func TestCaptureToolCallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	pol := redact.StrictProfile()
	eng, err := redact.NewEngine(pol, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	sess := event.NewSession("filesystem")
	cw := NewCaptureWorker(CaptureWorker{
		Session:      sess,
		Engine:       eng,
		Queue:        q,
		Destination:  "default",
		Host:         "test-host",
		HostVersion:  "0.0.1",
		ScoutVersion: "0.1.0",
	})
	in := make(chan wire.Frame, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cw.Run(ctx, in)
		close(done)
	}()

	// Drive a tools/call request and a successful response.
	in <- wire.Frame{Dir: wire.DirHostToUpstream, Bytes: []byte(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"~/x"}}}`,
	)}
	in <- wire.Frame{Dir: wire.DirUpstreamToHost, Bytes: []byte(
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hello"}]}}`,
	)}
	// Give the worker a moment.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	st, err := q.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Pending != 1 {
		t.Fatalf("expected 1 pending event; got %+v", st)
	}
	got, err := q.ClaimPending("default", 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Claim len = %d, want 1", len(got))
	}
}

func TestCaptureIgnoresNonMatchingMessages(t *testing.T) {
	dir := t.TempDir()
	q, _ := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	pol := redact.StrictProfile()
	eng, _ := redact.NewEngine(pol, nil)
	cw := NewCaptureWorker(CaptureWorker{
		Session: event.NewSession("x"), Engine: eng, Queue: q,
		Destination: "default", ScoutVersion: "test",
	})
	in := make(chan wire.Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cw.Run(ctx, in); close(done) }()

	// Notification → should not produce an event.
	in <- wire.Frame{Dir: wire.DirHostToUpstream, Bytes: []byte(
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"id":99}}`,
	)}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	st, _ := q.Stats()
	if st.Pending != 0 {
		t.Errorf("notifications should not produce events: %+v", st)
	}
}
