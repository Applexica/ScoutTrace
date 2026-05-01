package queue

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func newQ(t *testing.T) *Queue {
	t.Helper()
	dir := t.TempDir()
	q, err := Open(Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return q
}

func TestEnqueueClaimAck(t *testing.T) {
	q := newQ(t)
	for i := 0; i < 5; i++ {
		payload, _ := json.Marshal(map[string]any{"i": i})
		if err := q.Enqueue("EV"+string(rune('0'+i))+"00000000000000000000000", "default", payload); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	got, err := q.ClaimPending("default", 10)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("claim len = %d, want 5", len(got))
	}
	for _, r := range got {
		if err := q.Ack(r.ID); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}
	st, err := q.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Pending != 0 || st.Inflight != 0 {
		t.Fatalf("stats = %+v, want all zero", st)
	}
}

func TestRetryAndRecover(t *testing.T) {
	q := newQ(t)
	payload := []byte(`{"x":1}`)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := q.Enqueue(id, "d", payload); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := q.ClaimPending("d", 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claim len = %d", len(got))
	}
	// Simulate "process died" — RecoverInflight should bring it back.
	rec, err := q.RecoverInflight()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rec != 1 {
		t.Fatalf("recovered = %d, want 1", rec)
	}
	// Reclaim and ack normally.
	got, _ = q.ClaimPending("d", 10)
	if len(got) != 1 {
		t.Fatalf("claim after recover len = %d", len(got))
	}
	if err := q.Retry(got[0].ID, time.Now().Add(-time.Hour), "503"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	got, _ = q.ClaimPending("d", 10)
	if len(got) != 1 {
		t.Fatalf("claim after retry len = %d", len(got))
	}
	if got[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got[0].Attempts)
	}
	if err := q.MarkDead(got[0].ID, "permanent"); err != nil {
		t.Fatalf("MarkDead: %v", err)
	}
	st, _ := q.Stats()
	if st.Dead != 1 {
		t.Errorf("dead = %d, want 1", st.Dead)
	}
}

func TestEnqueuePayloadCap(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(Options{Dir: dir, MaxRowBytes: 32})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'A'
	}
	err = q.Enqueue("01ARZ3NDEKTSV4RRFFQ69G5FAV", "d", big)
	if err == nil {
		t.Fatalf("expected payload-too-large error")
	}
}

func TestPrune(t *testing.T) {
	q := newQ(t)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := q.Enqueue(id, "d", []byte(`{"x":1}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, _ := q.ClaimPending("d", 10)
	if err := q.MarkDead(got[0].ID, "test"); err != nil {
		t.Fatalf("MarkDead: %v", err)
	}
	// Force "old" by moving the clock forward.
	q.SetClock(func() time.Time { return time.Now().Add(48 * time.Hour) })
	if err := q.Prune(time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	st, _ := q.Stats()
	if st.Dead != 0 {
		t.Errorf("after prune dead = %d, want 0", st.Dead)
	}
}
