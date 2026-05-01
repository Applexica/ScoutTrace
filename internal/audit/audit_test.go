package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendWritesLine(t *testing.T) {
	dir := t.TempDir()
	log, err := NewLogger(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if err := log.Append("cli", "hosts_patch", map[string]any{"host": "claude-desktop"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event"] != "hosts_patch" {
		t.Errorf("event = %v, want hosts_patch", got["event"])
	}
	if got["host"] != "claude-desktop" {
		t.Errorf("host = %v", got["host"])
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	// Pre-fill the log to over 10 MiB.
	big := make([]byte, MaxLogBytes+10)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	log, _ := NewLogger(path)
	if err := log.Append("cli", "x", nil); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated file; %v", err)
	}
}
