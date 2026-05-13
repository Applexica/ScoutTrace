package halt

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheInMemory(t *testing.T) {
	c := NewCache("")
	if (c.Get("a1") != State{}) {
		t.Fatalf("expected empty state for unknown agent")
	}
	if err := c.Set("a1", State{Halted: true, HaltReason: "hourly $5 crossed"}); err != nil {
		t.Fatal(err)
	}
	got := c.Get("a1")
	if !got.Halted || got.HaltReason != "hourly $5 crossed" {
		t.Fatalf("unexpected state: %+v", got)
	}
}

func TestCachePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halt-state.json")

	c1 := NewCache(path)
	if err := c1.Set("a1", State{Halted: true, HaltReason: "hourly", ManualClearRequired: false}); err != nil {
		t.Fatal(err)
	}
	if err := c1.Set("a2", State{Halted: true, HaltReason: "daily", ManualClearRequired: true}); err != nil {
		t.Fatal(err)
	}

	c2 := NewCache(path)
	a1 := c2.Get("a1")
	a2 := c2.Get("a2")
	if !a1.Halted || a1.HaltReason != "hourly" {
		t.Fatalf("a1 round-trip lost: %+v", a1)
	}
	if !a2.ManualClearRequired {
		t.Fatalf("a2 manual-clear flag lost: %+v", a2)
	}
}

func TestSetIfChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halt-state.json")
	c := NewCache(path)
	wrote, err := c.SetIfChanged("a1", State{Halted: true, HaltReason: "first"})
	if err != nil || !wrote {
		t.Fatalf("first set should write: wrote=%v err=%v", wrote, err)
	}
	wrote, err = c.SetIfChanged("a1", State{Halted: true, HaltReason: "first"})
	if err != nil || wrote {
		t.Fatalf("identical set should NOT write: wrote=%v err=%v", wrote, err)
	}
	wrote, err = c.SetIfChanged("a1", State{Halted: false})
	if err != nil || !wrote {
		t.Fatalf("transition to clear should write: wrote=%v err=%v", wrote, err)
	}
}

func TestUpdatedAtAutoFilled(t *testing.T) {
	c := NewCache("")
	before := time.Now()
	_ = c.Set("a1", State{Halted: true})
	after := time.Now()
	got := c.Get("a1")
	if got.UpdatedAt.Before(before) || got.UpdatedAt.After(after.Add(time.Second)) {
		t.Fatalf("UpdatedAt out of window: %v not in [%v..%v]", got.UpdatedAt, before, after)
	}
}

func TestCacheTolerantToCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halt-state.json")
	if err := os.WriteFile(path, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCache(path)
	// Should return zero value, not panic. Then accept writes.
	if (c.Get("a1") != State{}) {
		t.Fatalf("expected zero value from corrupt file")
	}
	if err := c.Set("a1", State{Halted: true}); err != nil {
		t.Fatalf("write after corrupt read failed: %v", err)
	}
	c2 := NewCache(path)
	if !c2.Get("a1").Halted {
		t.Fatalf("recovery write did not persist")
	}
}

func TestDefaultPath(t *testing.T) {
	if DefaultPath("") != "" {
		t.Fatalf("empty home should yield empty path")
	}
	got := DefaultPath("/x/y")
	want := filepath.Join("/x/y", "halt-state.json")
	if got != want {
		t.Fatalf("DefaultPath(%q) = %q, want %q", "/x/y", got, want)
	}
}
