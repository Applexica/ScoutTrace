package hosts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// --- TOML (Codex) ---

func TestPatchUnpatchTOML_RoundTrip(t *testing.T) {
	orig := mustReadFixture(t, "codex.toml")
	dir := t.TempDir()

	patched, err := patchBytesTOML(orig, "mcp_servers", []string{"filesystem", "github"}, "/usr/local/bin/scouttrace", dir)
	if err != nil {
		t.Fatalf("patchBytesTOML: %v", err)
	}
	if !bytes.Contains(patched, []byte("/usr/local/bin/scouttrace")) {
		t.Fatalf("patched output missing proxy exe:\n%s", patched)
	}
	if !bytes.Contains(patched, []byte(`"--server-name"`)) {
		t.Fatalf("patched output missing --server-name flag:\n%s", patched)
	}
	// env block must remain untouched in the patched file.
	if !bytes.Contains(patched, []byte(`GITHUB_TOKEN = "ghp_secret_value_should_never_be_copied"`)) {
		t.Fatalf("env block was disturbed:\n%s", patched)
	}
	// Other section preserved.
	if !bytes.Contains(patched, []byte("[other_section]")) {
		t.Fatalf("non-mcp section dropped:\n%s", patched)
	}
	// markers.json must NOT contain the env value.
	mb, err := os.ReadFile(filepath.Join(dir, "markers.json"))
	if err != nil {
		t.Fatalf("read markers.json: %v", err)
	}
	if bytes.Contains(mb, []byte("ghp_secret_value_should_never_be_copied")) {
		t.Fatalf("markers.json contains raw env secret:\n%s", mb)
	}
	if !bytes.Contains(mb, []byte(`"command"`)) || !bytes.Contains(mb, []byte(`"args"`)) {
		t.Fatalf("markers.json missing command/args:\n%s", mb)
	}

	unpatched, restored, err := unpatchBytesTOML(patched, "mcp_servers", dir)
	if err != nil {
		t.Fatalf("unpatchBytesTOML: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored = %d, want 2", len(restored))
	}
	if !bytes.Equal(orig, unpatched) {
		t.Fatalf("byte equality failed.\nORIG:\n%s\nGOT:\n%s", orig, unpatched)
	}
}

func TestPatchTOML_Idempotent(t *testing.T) {
	orig := mustReadFixture(t, "codex.toml")
	dir := t.TempDir()
	once, err := patchBytesTOML(orig, "mcp_servers", nil, "scouttrace", dir)
	if err != nil {
		t.Fatalf("first patch: %v", err)
	}
	twice, err := patchBytesTOML(once, "mcp_servers", nil, "scouttrace", dir)
	if err != nil {
		t.Fatalf("second patch: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("toml patch is not idempotent.\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

func TestPatchTOML_OnlySpecifiedServers(t *testing.T) {
	orig := mustReadFixture(t, "codex.toml")
	dir := t.TempDir()
	patched, err := patchBytesTOML(orig, "mcp_servers", []string{"filesystem"}, "scouttrace", dir)
	if err != nil {
		t.Fatalf("patchBytesTOML: %v", err)
	}
	if bytes.Count(patched, []byte("--server-name")) != 1 {
		t.Errorf("expected exactly one --server-name; got %d", bytes.Count(patched, []byte("--server-name")))
	}
	if !bytes.Contains(patched, []byte(`server-name", "filesystem"`)) {
		t.Errorf("filesystem not patched:\n%s", patched)
	}
}

// --- YAML (Hermes) ---

func TestPatchUnpatchYAML_RoundTrip(t *testing.T) {
	orig := mustReadFixture(t, "hermes.yaml")
	dir := t.TempDir()

	patched, err := patchBytesYAML(orig, "mcp_servers", []string{"filesystem", "github"}, "/usr/local/bin/scouttrace", dir)
	if err != nil {
		t.Fatalf("patchBytesYAML: %v", err)
	}
	if !bytes.Contains(patched, []byte("/usr/local/bin/scouttrace")) {
		t.Fatalf("patched output missing proxy exe:\n%s", patched)
	}
	if !bytes.Contains(patched, []byte("--server-name")) {
		t.Fatalf("patched output missing --server-name flag:\n%s", patched)
	}
	// env block must remain untouched.
	if !bytes.Contains(patched, []byte("GITHUB_TOKEN: \"ghp_secret_value_should_never_be_copied\"")) {
		t.Fatalf("env block was disturbed:\n%s", patched)
	}
	// Other section preserved.
	if !bytes.Contains(patched, []byte("other_section:")) {
		t.Fatalf("non-mcp section dropped:\n%s", patched)
	}
	// markers.json must NOT contain the env value.
	mb, err := os.ReadFile(filepath.Join(dir, "markers.json"))
	if err != nil {
		t.Fatalf("read markers.json: %v", err)
	}
	if bytes.Contains(mb, []byte("ghp_secret_value_should_never_be_copied")) {
		t.Fatalf("markers.json contains raw env secret:\n%s", mb)
	}

	unpatched, restored, err := unpatchBytesYAML(patched, "mcp_servers", dir)
	if err != nil {
		t.Fatalf("unpatchBytesYAML: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored = %d, want 2", len(restored))
	}
	if !bytes.Equal(orig, unpatched) {
		t.Fatalf("byte equality failed.\nORIG:\n%s\nGOT:\n%s", orig, unpatched)
	}
}

func TestPatchYAML_Idempotent(t *testing.T) {
	orig := mustReadFixture(t, "hermes.yaml")
	dir := t.TempDir()
	once, err := patchBytesYAML(orig, "mcp_servers", nil, "scouttrace", dir)
	if err != nil {
		t.Fatalf("first patch: %v", err)
	}
	twice, err := patchBytesYAML(once, "mcp_servers", nil, "scouttrace", dir)
	if err != nil {
		t.Fatalf("second patch: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("yaml patch is not idempotent.\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

// --- JSON (OpenCode/OpenClaw) ---

func TestPatchUnpatchOpenCodeJSON_RoundTrip(t *testing.T) {
	orig := mustReadFixture(t, "opencode.json")
	patched, err := patchBytes(orig, "mcp", []string{"filesystem", "github"}, "/usr/local/bin/scouttrace")
	if err != nil {
		t.Fatalf("patchBytes (mcp): %v", err)
	}
	if !bytes.Contains(patched, []byte("_scouttrace")) {
		t.Fatalf("patched output missing inline marker:\n%s", patched)
	}
	if !bytes.Contains(patched, []byte("/usr/local/bin/scouttrace")) {
		t.Fatalf("patched output missing proxy exe:\n%s", patched)
	}

	// Even though the JSON inline marker stores `original`, secrets in env
	// must not be duplicated into the marker's original block.
	type root struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	var rt root
	if err := json.Unmarshal(patched, &rt); err != nil {
		t.Fatalf("unmarshal patched: %v", err)
	}
	gh := rt.MCP["github"]
	mk, ok := gh["_scouttrace"].(map[string]any)
	if !ok {
		t.Fatalf("github missing _scouttrace marker")
	}
	origBlock, _ := mk["original"].(map[string]any)
	if _, hasEnv := origBlock["env"]; hasEnv {
		t.Errorf("marker.original duplicated env (raw secrets); want env omitted")
	}
	// env should still be present at the entry level (untouched).
	if _, hasEnvOnEntry := gh["env"]; !hasEnvOnEntry {
		t.Errorf("env was stripped from patched entry; should remain")
	}

	unpatched, restored, err := unpatchBytes(patched, "mcp")
	if err != nil {
		t.Fatalf("unpatchBytes: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored = %d, want 2", len(restored))
	}
	var wantJSON, gotJSON any
	if err := json.Unmarshal(orig, &wantJSON); err != nil {
		t.Fatalf("unmarshal orig: %v", err)
	}
	if err := json.Unmarshal(unpatched, &gotJSON); err != nil {
		t.Fatalf("unmarshal unpatched: %v", err)
	}
	if !reflect.DeepEqual(wantJSON, gotJSON) {
		t.Fatalf("semantic JSON equality failed.\nORIG:\n%s\nGOT:\n%s", orig, unpatched)
	}
}

func TestRegistryIncludesNewHosts(t *testing.T) {
	r := Registry()
	for _, id := range []string{"codex", "opencode", "openclaw", "hermes"} {
		h, ok := r[id]
		if !ok {
			t.Errorf("Registry missing host %q", id)
			continue
		}
		if h.ID != id {
			t.Errorf("host %q: ID = %q", id, h.ID)
		}
		if strings.TrimSpace(h.DisplayName) == "" {
			t.Errorf("host %q: DisplayName empty", id)
		}
	}
}

// --- End-to-end Patch/Unpatch via Host dispatch ---

func TestPatchDispatchByFormat_TOML(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, mustReadFixture(t, "codex.toml"), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	bak := filepath.Join(dir, "backups")
	h := &Host{ID: "codex", DisplayName: "Codex", Format: FormatTOML, ServersKey: "mcp_servers", Marker: MarkerExternal}
	res, err := Patch(h, cfg, nil, "/usr/local/bin/scouttrace", bak, false, "")
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(res.Servers) != 2 {
		t.Errorf("Patch reported %d servers; want 2", len(res.Servers))
	}
	got, _ := os.ReadFile(cfg)
	if !bytes.Contains(got, []byte("scouttrace")) {
		t.Errorf("config not patched:\n%s", got)
	}
	res2, err := Unpatch(h, cfg, bak)
	if err != nil {
		t.Fatalf("Unpatch: %v", err)
	}
	if len(res2.Servers) != 2 {
		t.Errorf("Unpatch reported %d servers; want 2", len(res2.Servers))
	}
	got2, _ := os.ReadFile(cfg)
	if !bytes.Equal(got2, mustReadFixture(t, "codex.toml")) {
		t.Errorf("Unpatch did not restore original.\nGOT:\n%s", got2)
	}
}

func TestPatchDispatchByFormat_YAML(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, mustReadFixture(t, "hermes.yaml"), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	bak := filepath.Join(dir, "backups")
	h := &Host{ID: "hermes", DisplayName: "Hermes", Format: FormatYAML, ServersKey: "mcp_servers", Marker: MarkerExternal}
	if _, err := Patch(h, cfg, nil, "/usr/local/bin/scouttrace", bak, false, ""); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if _, err := Unpatch(h, cfg, bak); err != nil {
		t.Fatalf("Unpatch: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if !bytes.Equal(got, mustReadFixture(t, "hermes.yaml")) {
		t.Errorf("Unpatch did not restore original.\nGOT:\n%s", got)
	}
}
