package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func runCLIWithInput(t *testing.T, home, input string, argv ...string) (int, string, string) {
	t.Helper()
	full := []string{"scouttrace", "--home", home}
	full = append(full, argv...)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exit := Run(full, strings.NewReader(input), stdout, stderr)
	return exit, stdout.String(), stderr.String()
}

func TestInitInteractiveWizardStdout(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLIWithInput(t, home, strings.Join([]string{
		"stdout", // destination
		"none",   // hosts
		"strict", // redaction profile
		"y",      // write config
	}, "\n")+"\n", "init")
	if exit != 0 {
		t.Fatalf("interactive init exit = %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "ScoutTrace setup wizard") || !strings.Contains(stdout, "Wrote") {
		t.Fatalf("wizard output missing expected text:\n%s", stdout)
	}
	exit, out, stderr := runCLI(t, home, "config", "show", "--json")
	if exit != 0 {
		t.Fatalf("config show exit = %d stderr=%s", exit, stderr)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config not JSON: %v\n%s", err, out)
	}
	dests := cfg["destinations"].([]any)
	first := dests[0].(map[string]any)
	if first["type"] != "stdout" {
		t.Fatalf("destination type = %v, want stdout\nconfig: %s", first["type"], out)
	}
}

func TestInitInteractiveWizardWebhookScoutUsesEnvCredentialRef(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLIWithInput(t, home, strings.Join([]string{
		"webhookscout",                          // destination
		"https://api.webhookscout.test",         // API base
		"agent_test_123",                        // agent id
		"env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY", // credential reference
		"claude-code,cursor",                    // hosts
		"standard",                              // redaction profile
		"y",                                     // write config
	}, "\n")+"\n", "init")
	if exit != 0 {
		t.Fatalf("interactive WebhookScout init exit = %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, "interactive wizard not implemented") {
		t.Fatalf("saw old MVP error:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	exit, out, stderr := runCLI(t, home, "config", "show", "--json")
	if exit != 0 {
		t.Fatalf("config show exit = %d stderr=%s", exit, stderr)
	}
	if !strings.Contains(out, `"type": "webhookscout"`) ||
		!strings.Contains(out, `"api_base": "https://api.webhookscout.test"`) ||
		!strings.Contains(out, `"agent_id": "agent_test_123"`) ||
		!strings.Contains(out, `"auth_header_ref": "env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY"`) {
		t.Fatalf("config missing WebhookScout wizard fields:\n%s", out)
	}
}

func TestInitWarnsWhenSelectedHostsWereNotPatched(t *testing.T) {
	home := t.TempDir()
	fakeUserHome := t.TempDir()
	t.Setenv("HOME", fakeUserHome)

	exit, stdout, stderr := runCLIWithInput(t, home, strings.Join([]string{
		"stdout",
		"claude-code",
		"strict",
		"y",
	}, "\n")+"\n", "init")
	if exit != 0 {
		t.Fatalf("init exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "No selected MCP host configs were patched") {
		t.Fatalf("expected no-host-patched warning, stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "ScoutTrace will not capture Claude Code or MCP traffic until at least one MCP server is wrapped") {
		t.Fatalf("expected capture guidance warning, stderr=%s", stderr)
	}
}

func TestInitWebhookScoutPrintsApprovalNextStep(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--agent-id", "agent_next", "--auth-header-ref", "env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY", "--hosts", "none")
	if exit != 0 {
		t.Fatalf("init exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "Next: approve network delivery with `scouttrace destination approve default`") {
		t.Fatalf("expected approval next-step guidance, stdout=%s", stdout)
	}
}

func TestInitInteractiveWebhookScoutRawAPIKeyStoresCredentialRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	rawKey := "whs_test_raw_key_for_wizard"
	exit, stdout, stderr := runCLIWithInput(t, home, strings.Join([]string{
		"webhookscout",
		"https://api.webhookscout.test",
		"agent_raw_key",
		rawKey,
		"none",
		"strict",
		"y",
	}, "\n")+"\n", "init")
	if exit != 0 {
		t.Fatalf("interactive raw-key init exit = %d\nstdout:%s\nstderr:%s", exit, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, rawKey) {
		t.Fatalf("raw API key leaked in wizard output\nstdout:%s\nstderr:%s", stdout, stderr)
	}
	exit, out, stderr := runCLI(t, home, "config", "show", "--json")
	if exit != 0 {
		t.Fatalf("config show exit=%d stderr=%s", exit, stderr)
	}
	if strings.Contains(out, rawKey) {
		t.Fatalf("raw API key leaked in config:\n%s", out)
	}
	if !strings.Contains(out, `"auth_header_ref": "encfile://default-api-key"`) {
		t.Fatalf("expected encfile auth ref for pasted API key, config:\n%s", out)
	}
}

func TestInitInteractiveWebhookScoutRawAPIKeyWithoutStorageFailsSafely(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	rawKey := "whs_test_raw_key_without_storage"
	exit, stdout, stderr := runCLIWithInput(t, home, strings.Join([]string{
		"webhookscout",
		"https://api.webhookscout.test",
		"agent_raw_key",
		rawKey,
		"none",
		"strict",
		"y",
	}, "\n")+"\n", "init")
	if exit == 0 {
		t.Fatalf("expected raw API key without secure storage to fail")
	}
	if strings.Contains(stdout+stderr, rawKey) {
		t.Fatalf("raw API key leaked after storage failure\nstdout:%s\nstderr:%s", stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config should not be written when raw key cannot be stored; stat err=%v", err)
	}
}

func TestInitInteractiveEmptyStdinDoesNotWriteConfig(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLIWithInput(t, home, "", "init")
	if exit == 0 {
		t.Fatalf("expected non-zero exit for empty non-interactive stdin; stdout=%s stderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "interactive input unavailable") {
		t.Fatalf("stderr missing non-interactive guidance: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config should not be written on empty stdin; stat err=%v", err)
	}
}

func TestInitSetupTokenDryRunDoesNotExchangeOrWriteCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatalf("setup token exchange should not be called during dry-run")
	}))
	defer srv.Close()
	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--dry-run", "--destination", "webhookscout", "--api-base", srv.URL, "--setup-token", "setup_test_secret", "--hosts", "none")
	if exit != 0 {
		t.Fatalf("dry-run setup-token init exit = %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}
	if called {
		t.Fatalf("setup token endpoint was called during dry-run")
	}
	if _, err := os.Stat(filepath.Join(home, "credentials.enc")); !os.IsNotExist(err) {
		t.Fatalf("credentials should not be written during dry-run; stat err=%v", err)
	}
	if strings.Contains(stdout+stderr, "setup_test_secret") {
		t.Fatalf("setup token leaked during dry-run\nstdout:%s\nstderr:%s", stdout, stderr)
	}
}

func TestInitSetupTokenExchangeErrorDoesNotLeakResponseBody(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token setup_test_secret produced api key whs_secret", http.StatusBadRequest)
	}))
	defer srv.Close()
	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--api-base", srv.URL, "--setup-token", "setup_test_secret", "--hosts", "none")
	if exit == 0 {
		t.Fatalf("expected exchange failure")
	}
	if strings.Contains(stdout+stderr, "setup_test_secret") || strings.Contains(stdout+stderr, "whs_secret") {
		t.Fatalf("exchange failure leaked secret\nstdout:%s\nstderr:%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "HTTP 400") {
		t.Fatalf("stderr missing sanitized HTTP status: %s", stderr)
	}
}

func TestInitSetupTokenRejectsPlainHTTPNonLocalhost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	for _, apiBase := range []string{"http://api.example.com", "http://127.evil.com", "http://127.0.0.1.evil.com"} {
		exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--api-base", apiBase, "--setup-token", "setup_test_secret", "--hosts", "none")
		if exit == 0 {
			t.Fatalf("expected plain HTTP api base rejection for %s\nstdout:%s\nstderr:%s", apiBase, stdout, stderr)
		}
		if !strings.Contains(stderr, "https") {
			t.Fatalf("stderr missing https guidance for %s: %s", apiBase, stderr)
		}
	}
}

func TestInitSetupTokenRejectsRedirects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalled = true
		http.Error(w, "should not receive setup token", http.StatusTeapot)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--api-base", redirector.URL, "--setup-token", "setup_test_secret", "--hosts", "none")
	if exit == 0 {
		t.Fatalf("expected redirect rejection\nstdout:%s\nstderr:%s", stdout, stderr)
	}
	if targetCalled {
		t.Fatalf("redirect target received setup token request")
	}
	if strings.Contains(stdout+stderr, "setup_test_secret") {
		t.Fatalf("setup token leaked in redirect failure\nstdout:%s\nstderr:%s", stdout, stderr)
	}
}

func TestInitSetupTokenExchangesAndStoresAPIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/setup-tokens/exchange" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		gotToken = body["token"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent_id":"agent_from_setup","api_key":"whs_test_secret","scopes":["mcp:write"]}`))
	}))
	defer srv.Close()

	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--api-base", srv.URL, "--setup-token", "setup_test_secret", "--agent-name", "local", "--hosts", "none")
	if exit != 0 {
		t.Fatalf("init setup-token exit = %d\nstdout: %s\nstderr: %s", exit, stdout, stderr)
	}
	if gotToken != "setup_test_secret" {
		t.Fatalf("setup exchange token = %q", gotToken)
	}
	if strings.Contains(stdout+stderr, "setup_test_secret") || strings.Contains(stdout+stderr, "whs_test_secret") {
		t.Fatalf("secret leaked in output\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	exit, out, stderr := runCLI(t, home, "config", "show", "--json")
	if exit != 0 {
		t.Fatalf("config show exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(out, `"agent_id": "agent_from_setup"`) || !strings.Contains(out, `"auth_header_ref": "encfile://default-api-key"`) {
		t.Fatalf("setup exchange config missing fields:\n%s", out)
	}
	if strings.Contains(out, "whs_test_secret") || strings.Contains(out, "setup_test_secret") {
		t.Fatalf("secret leaked in config:\n%s", out)
	}
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

func TestWebhookScoutPreviewInjectFlushPostsMcpEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_WEBHOOKSCOUT_API_KEY", "whs_cli_flush_test_key")
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		reader := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Fatalf("gzip reader: %v", err)
			}
			defer gz.Close()
			reader = gz
		}
		b, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("request body not JSON: %v\n%s", err, b)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--api-base", srv.URL, "--agent-id", "agent-cli", "--auth-header-ref", "env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY", "--hosts", "none")
	if exit != 0 {
		t.Fatalf("init exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	exit, _, stderr = runCLI(t, home, "destination", "approve", "default")
	if exit != 0 {
		t.Fatalf("destination approve exit=%d stderr=%s", exit, stderr)
	}
	exit, previewOut, stderr := runCLI(t, home, "preview", "--json")
	if exit != 0 {
		t.Fatalf("preview exit=%d stderr=%s", exit, stderr)
	}
	previewPath := filepath.Join(home, "preview.json")
	if err := os.WriteFile(previewPath, []byte(previewOut), 0o600); err != nil {
		t.Fatalf("write preview: %v", err)
	}
	exit, _, stderr = runCLI(t, home, "queue", "inject", "--from", previewPath, "--destination", "default")
	if exit != 0 {
		t.Fatalf("queue inject exit=%d stderr=%s", exit, stderr)
	}
	exit, flushOut, stderr := runCLI(t, home, "flush", "--destination", "default")
	if exit != 0 {
		t.Fatalf("flush exit=%d stdout=%s stderr=%s", exit, flushOut, stderr)
	}
	if gotPath != "/api/mcp/agent-cli/events" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotAuth != "Bearer whs_cli_flush_test_key" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if gotBody["tool"] == "" || gotBody["status"] != "ok" {
		t.Fatalf("unexpected body: %#v", gotBody)
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

func TestClaudeHookPostToolUseCapturesPluginMCPTool(t *testing.T) {
	home := t.TempDir()
	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "stdout", "--hosts", "none", "--profile", "standard")
	if exit != 0 {
		t.Fatalf("init exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	payload := `{
		"session_id":"claude-session-1",
		"hook_event_name":"PostToolUse",
		"tool_name":"mcp__playwright__browser_navigate",
		"tool_input":{"url":"https://example.com/?token=whs_test_abcdefghijklmnop"},
		"tool_response":{"content":[{"type":"text","text":"navigated"}]}
	}`
	exit, stdout, stderr = runCLIWithInput(t, home, payload, "--json", "claude-hook", "post-tool-use", "--destination", "default")
	if exit != 0 {
		t.Fatalf("hook exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"destination":"default"`) {
		t.Fatalf("hook JSON missing destination: %s", stdout)
	}
	exit, out, stderr := runCLI(t, home, "queue", "list", "--destination", "default", "--limit", "1")
	if exit != 0 {
		t.Fatalf("queue list exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(out, `"kind":"claude_code_hook"`) || !strings.Contains(out, `"name":"playwright"`) || !strings.Contains(out, `"name":"browser_navigate"`) {
		t.Fatalf("hook event missing source/server/tool fields:\n%s", out)
	}
	if strings.Contains(out, "whs_test_abcdefghijklmnop") {
		t.Fatalf("hook event leaked secret in queued payload:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED:") {
		t.Fatalf("expected redaction marker in queued payload:\n%s", out)
	}
}

func TestClaudeHookInstallWritesLocalSettings(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	exit, stdout, stderr := runCLI(t, home, "claude-hook", "install", "--scope", "local", "--project-dir", project, "--destination", "default")
	if exit != 0 {
		t.Fatalf("install exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	settingsPath := filepath.Join(project, ".claude", "settings.local.json")
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "PostToolUse") || !strings.Contains(text, "claude-hook post-tool-use") || !strings.Contains(text, "--flush") {
		t.Fatalf("settings missing hook command:\n%s", text)
	}
	// Reinstall is idempotent for the same generated command.
	exit, _, stderr = runCLI(t, home, "claude-hook", "install", "--scope", "local", "--project-dir", project, "--destination", "default")
	if exit != 0 {
		t.Fatalf("second install exit=%d stderr=%s", exit, stderr)
	}
	b2, _ := os.ReadFile(settingsPath)
	if strings.Count(string(b2), "claude-hook post-tool-use") != 1 {
		t.Fatalf("hook command duplicated:\n%s", string(b2))
	}
}

func TestClaudeHookPostToolUseFlushesToWebhookScout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_WEBHOOKSCOUT_API_KEY", "whs_hook_flush_test_key")
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		reader := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Fatalf("gzip reader: %v", err)
			}
			defer gz.Close()
			reader = gz
		}
		b, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("body not JSON: %v\n%s", err, b)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	exit, stdout, stderr := runCLI(t, home, "init", "--yes", "--destination", "webhookscout", "--api-base", srv.URL, "--agent-id", "agent-hook", "--auth-header-ref", "env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY", "--hosts", "none")
	if exit != 0 {
		t.Fatalf("init exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	exit, _, stderr = runCLI(t, home, "destination", "approve", "default")
	if exit != 0 {
		t.Fatalf("approve exit=%d stderr=%s", exit, stderr)
	}
	payload := `{"session_id":"claude-session-2","hook_event_name":"PostToolUse","tool_name":"mcp__playwright__browser_click","tool_input":{"selector":"button:text('Buy')"},"tool_response":{"content":[{"type":"text","text":"clicked"}]}}`
	exit, _, stderr = runCLIWithInput(t, home, payload, "claude-hook", "post-tool-use", "--destination", "default", "--flush")
	if exit != 0 {
		t.Fatalf("hook exit=%d stderr=%s", exit, stderr)
	}
	if gotPath != "/api/mcp/agent-hook/events" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotAuth != "Bearer whs_hook_flush_test_key" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if gotBody["tool"] != "browser_click" || gotBody["status"] != "ok" {
		t.Fatalf("unexpected body: %#v", gotBody)
	}
}
