package wire

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

type fakeGate struct {
	allow   bool
	reply   []byte
	capture bool
}

func (g fakeGate) Inspect(_ []byte) Decision {
	return Decision{Forward: g.allow, Reply: g.reply, Capture: g.capture}
}

func TestGatedForwarderForwardsWhenAllowed(t *testing.T) {
	src := strings.NewReader(`{"id":1}` + "\n" + `{"id":2}` + "\n")
	var up, back bytes.Buffer
	cap := make(chan Frame, 4)
	stats := &TeeStats{}
	fwd := &GatedForwarder{
		Src: src, Upstream: &up, HostBack: &back,
		Gate: fakeGate{allow: true, capture: true},
		CapCh: cap, Stats: stats,
	}
	if err := fwd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := `{"id":1}` + "\n" + `{"id":2}` + "\n"
	if up.String() != want {
		t.Fatalf("upstream got %q, want %q", up.String(), want)
	}
	if back.Len() != 0 {
		t.Fatalf("host-back should be empty when allowing, got %q", back.String())
	}
	close(cap)
	count := 0
	for range cap {
		count++
	}
	if count != 2 {
		t.Fatalf("expected 2 captures, got %d", count)
	}
}

func TestGatedForwarderBlocksWithReply(t *testing.T) {
	src := strings.NewReader(`{"id":1,"method":"tools/call"}` + "\n")
	var up, back bytes.Buffer
	reply := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32099,"message":"halted"}}`)
	fwd := &GatedForwarder{
		Src: src, Upstream: &up, HostBack: &back,
		Gate: fakeGate{allow: false, reply: reply, capture: true},
		Stats: &TeeStats{},
	}
	if err := fwd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if up.Len() != 0 {
		t.Fatalf("upstream should be empty when blocked, got %q", up.String())
	}
	want := string(reply) + "\n"
	if back.String() != want {
		t.Fatalf("host-back got %q, want %q", back.String(), want)
	}
}

func TestGatedForwarderRespectsLock(t *testing.T) {
	src := strings.NewReader(`{"id":1}` + "\n")
	var up, back bytes.Buffer
	mu := &sync.Mutex{}
	mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fwd := &GatedForwarder{
			Src: src, Upstream: &up, HostBack: &back,
			Gate: fakeGate{allow: false, reply: []byte("hi"), capture: false},
			Stats: &TeeStats{},
			HostBackLock: mu,
		}
		_ = fwd.Run()
	}()
	// The goroutine must be blocked on HostBackLock; back should still be empty.
	select {
	case <-done:
		t.Fatalf("Run returned before lock released")
	default:
	}
	mu.Unlock()
	<-done
	if back.String() != "hi\n" {
		t.Fatalf("expected reply after lock released, got %q", back.String())
	}
}
