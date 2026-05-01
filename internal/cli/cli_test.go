package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI invokes the public Run dispatcher with stdin/stdout/stderr piped
// through bytes.Buffer for assertions.
func runCLI(t *testing.T, home string, argv ...string) (int, string, string) {
	t.Helper()
	full := []string{"scouttrace", "--home", home}
	full = append(full, argv...)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit := Run(full, strings.NewReader(""), stdout, stderr)
	return exit, stdout.String(), stderr.String()
}

func TestRootHelpListsCommands(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if exit := Run([]string{"scouttrace"}, strings.NewReader(""), stdout, stderr); exit != 0 {
		t.Fatalf("no-arg run exit = %d, want 0", exit)
	}
	out := stdout.String()
	for _, want := range []string{"init", "proxy", "status", "doctor", "tail", "queue", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing command %q\nGot:\n%s", want, out)
		}
	}
}

func TestRootHelpFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit := Run([]string{"scouttrace", "--help"}, strings.NewReader(""), stdout, stderr)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout.String(), "Commands:") {
		t.Errorf("help missing Commands section: %q", stdout.String())
	}
}

func TestUnknownCommandExit64(t *testing.T) {
	exit := Run([]string{"scouttrace", "no-such-cmd"}, strings.NewReader(""), io.Discard, io.Discard)
	if exit != 64 {
		t.Errorf("exit = %d, want 64", exit)
	}
}

// TestSmokeFlow exercises the full smoke sequence the user spec'd:
//
//	init --hosts none --destination stdout --yes
//	config validate
//	config show --json
//	preview --json
//	queue stats
//	queue inject --from <preview>
//	tail --format ndjson --once
//	flush --destination default
//	destination list
//	destination approve default
//	status --json
//	doctor
//	policy show
//	hosts list --json
//	version
func TestSmokeFlow(t *testing.T) {
	home := t.TempDir()

	steps := []struct {
		name   string
		args   []string
		expect int // expected exit code
	}{
		{"init", []string{"init", "--yes", "--hosts", "none", "--destination", "stdout"}, 0},
		{"config validate", []string{"config", "validate"}, 0},
		{"config show --json", []string{"config", "show", "--json"}, 0},
		{"queue stats", []string{"queue", "stats"}, 0},
		{"destination list", []string{"destination", "list"}, 0},
		{"destination approve default", []string{"destination", "approve", "default"}, 0},
		{"status --json", []string{"status", "--json"}, 0},
		{"doctor", []string{"doctor"}, 0},
		{"policy show", []string{"policy", "show"}, 0},
		{"hosts list --json", []string{"hosts", "list", "--json"}, 0},
		{"version", []string{"version"}, 0},
	}
	for _, st := range steps {
		exit, stdout, stderr := runCLI(t, home, st.args...)
		if exit != st.expect {
			t.Fatalf("[%s] exit = %d, want %d\nstdout: %s\nstderr: %s",
				st.name, exit, st.expect, stdout, stderr)
		}
	}

	// Preview produces a redacted ToolCallEvent on stdout when --json is passed.
	exit, previewOut, stderr := runCLI(t, home, "preview", "--json")
	if exit != 0 {
		t.Fatalf("preview --json exit = %d (stderr: %s)", exit, stderr)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(previewOut), &ev); err != nil {
		t.Fatalf("preview --json output not valid JSON: %v\nout: %s", err, previewOut)
	}
	if _, ok := ev["redaction"]; !ok {
		t.Errorf("preview output missing redaction block")
	}

	// Write preview output to a file and inject it.
	previewPath := filepath.Join(home, "preview.json")
	if err := os.WriteFile(previewPath, []byte(previewOut), 0o600); err != nil {
		t.Fatalf("write preview: %v", err)
	}
	exit, _, stderr = runCLI(t, home, "queue", "inject", "--from", previewPath)
	if exit != 0 {
		t.Fatalf("queue inject exit = %d (stderr: %s)", exit, stderr)
	}

	// queue stats now reports one pending event.
	exit, statsOut, _ := runCLI(t, home, "queue", "stats", "--json")
	if exit != 0 {
		t.Fatalf("queue stats exit = %d", exit)
	}
	var stats map[string]any
	_ = json.Unmarshal([]byte(statsOut), &stats)
	if pending, _ := stats["Pending"].(float64); pending != 1 {
		t.Errorf("expected 1 pending event after inject, got %v\noutput: %s", stats["Pending"], statsOut)
	}

	// tail --format ndjson --once shows the pending record without consuming it.
	exit, tailOut, stderr := runCLI(t, home, "tail", "--format", "ndjson", "--once")
	if exit != 0 {
		t.Fatalf("tail exit = %d (stderr: %s)", exit, stderr)
	}
	if !strings.Contains(tailOut, `"redaction"`) {
		t.Errorf("tail output missing redaction block:\n%s", tailOut)
	}
	// ndjson: one line per record + trailing newline.
	if !strings.HasSuffix(tailOut, "\n") {
		t.Errorf("tail output missing trailing newline")
	}

	// flush --destination default drains the queue via the stdout adapter.
	exit, flushOut, stderr := runCLI(t, home, "flush", "--destination", "default")
	if exit != 0 {
		t.Fatalf("flush exit = %d (stderr: %s)\nstdout: %s", exit, stderr, flushOut)
	}
	if !strings.Contains(flushOut, "attempts=") {
		t.Errorf("flush summary missing: %s", flushOut)
	}
}

func TestVersionCommand(t *testing.T) {
	home := t.TempDir()
	exit, out, _ := runCLI(t, home, "version")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("version output empty")
	}
}

func TestStatusJSONShape(t *testing.T) {
	home := t.TempDir()
	exit, _, _ := runCLI(t, home, "init", "--yes", "--hosts", "none", "--destination", "stdout")
	if exit != 0 {
		t.Fatalf("init exit = %d", exit)
	}
	exit, out, _ := runCLI(t, home, "status", "--json")
	if exit != 0 {
		t.Fatalf("status exit = %d", exit)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("status output not JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"version", "home", "config_path", "queue", "dispatcher"} {
		if _, ok := s[key]; !ok {
			t.Errorf("status missing key %q\noutput: %s", key, out)
		}
	}
}

func TestConfigValidateRejectsPlaintextAuth(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.json")
	bad := `{
		"destinations":[
			{"name":"bad","type":"http","url":"https://x","headers":{"Authorization":"Bearer xyz"}}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(bad), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	exit, _, stderr := runCLI(t, home, "config", "validate")
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	if !strings.Contains(stderr, "PLAINTEXT_AUTH") {
		t.Errorf("stderr missing PLAINTEXT_AUTH: %s", stderr)
	}
}

func TestPolicyShowDefaultStandard(t *testing.T) {
	home := t.TempDir()
	exit, out, _ := runCLI(t, home, "policy", "show", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if p["name"] != "standard" {
		t.Errorf("default profile = %v, want standard", p["name"])
	}
}

func TestHostsListJSON(t *testing.T) {
	home := t.TempDir()
	exit, out, _ := runCLI(t, home, "hosts", "list", "--json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	ids := map[string]bool{}
	for _, h := range arr {
		ids[h["id"].(string)] = true
	}
	for _, want := range []string{"claude-desktop", "claude-code", "cursor"} {
		if !ids[want] {
			t.Errorf("hosts list missing %q", want)
		}
	}
}
