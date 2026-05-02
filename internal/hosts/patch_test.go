package hosts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fixture builds a normalized claude-desktop config so byte-equality after
// patch+unpatch is meaningful.
func fixture() []byte {
	tree := map[string]any{
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []any{"-y", "@modelcontextprotocol/server-filesystem", "~/code"},
			},
			"github": map[string]any{
				"command": "npx",
				"args":    []any{"-y", "@modelcontextprotocol/server-github"},
			},
		},
		"theme": "system",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(tree)
	return buf.Bytes()
}

func TestPatchUnpatchByteEquality(t *testing.T) {
	orig := fixture()
	patched, err := patchBytes(orig, "mcpServers", []string{"filesystem", "github"}, "/usr/local/bin/scouttrace")
	if err != nil {
		t.Fatalf("patchBytes: %v", err)
	}
	if !bytes.Contains(patched, []byte("scouttrace")) {
		t.Fatalf("patched output does not mention scouttrace: %s", patched)
	}
	if !bytes.Contains(patched, []byte("_scouttrace")) {
		t.Fatalf("patched output missing inline marker: %s", patched)
	}
	unpatched, restored, err := unpatchBytes(patched, "mcpServers")
	if err != nil {
		t.Fatalf("unpatchBytes: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored = %d, want 2", len(restored))
	}
	if !bytes.Equal(orig, unpatched) {
		t.Fatalf("byte equality failed.\nORIG:\n%s\nGOT:\n%s", orig, unpatched)
	}
}

func TestPatchOnlySpecifiedServers(t *testing.T) {
	orig := fixture()
	patched, err := patchBytes(orig, "mcpServers", []string{"filesystem"}, "scouttrace")
	if err != nil {
		t.Fatalf("patchBytes: %v", err)
	}
	if !bytes.Contains(patched, []byte(`"filesystem":`)) {
		t.Errorf("filesystem key missing")
	}
	// github should NOT be patched.
	if bytes.Count(patched, []byte("_scouttrace")) != 1 {
		t.Errorf("expected exactly one _scouttrace marker; got %d", bytes.Count(patched, []byte("_scouttrace")))
	}
}

func TestUndoFromBackup(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg.json")
	bak := filepath.Join(dir, "backups")
	os.MkdirAll(bak, 0o700)

	orig := fixture()
	if err := os.WriteFile(cfg, orig, 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	// Simulate a backup written before patching.
	if err := os.WriteFile(filepath.Join(bak, "2026-04-30T12-00-00Z.bak"), orig, 0o600); err != nil {
		t.Fatalf("write bak: %v", err)
	}
	// Modify cfg by patching.
	patched, _ := patchBytes(orig, "mcpServers", []string{"filesystem"}, "/usr/local/bin/scouttrace")
	if err := os.WriteFile(cfg, patched, 0o600); err != nil {
		t.Fatalf("write patched: %v", err)
	}
	used, err := UndoFromBackup(&Host{Format: FormatJSON}, cfg, bak)
	if err != nil {
		t.Fatalf("UndoFromBackup: %v", err)
	}
	if used == "" {
		t.Errorf("backup path empty")
	}
	got, _ := os.ReadFile(cfg)
	if !bytes.Equal(got, orig) {
		t.Errorf("undo did not restore original bytes")
	}
}

func TestPatchIsIdempotent(t *testing.T) {
	orig := fixture()
	once, _ := patchBytes(orig, "mcpServers", []string{"filesystem"}, "scouttrace")
	twice, _ := patchBytes(once, "mcpServers", []string{"filesystem"}, "scouttrace")
	if !bytes.Equal(once, twice) {
		t.Errorf("patch is not idempotent.\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}
