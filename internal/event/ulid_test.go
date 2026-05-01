package event

import (
	"sort"
	"testing"
	"time"
)

func TestULIDFormat(t *testing.T) {
	id := NewULID()
	if len(id) != 26 {
		t.Fatalf("ULID length = %d, want 26", len(id))
	}
	if err := ValidateULID(id); err != nil {
		t.Fatalf("ValidateULID: %v", err)
	}
}

func TestULIDMonotonic(t *testing.T) {
	const N = 10000
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = NewULID()
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ULIDs not monotonic at index %d: gen=%s sort=%s", i, ids[i], sorted[i])
		}
	}
}

func TestULIDAtFixedTime(t *testing.T) {
	tm := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	a := NewULIDAt(tm)
	b := NewULIDAt(tm)
	if a == b {
		t.Fatalf("expected different ULIDs even at the same time")
	}
	if a >= b {
		t.Fatalf("expected monotonic order: %s >= %s", a, b)
	}
}
