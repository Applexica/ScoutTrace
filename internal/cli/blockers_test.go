package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/destinations/httpdest"
	"github.com/webhookscout/scouttrace/internal/dispatch"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
)

// TestApproveHostMatchesOnlyMatchingTypeAndHost verifies that an
// approve-host record for one (type, host) pair does NOT auto-approve a
// different destination on a different host.
func TestApproveHostMatchesOnlyMatchingTypeAndHost(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seen := []destSeenEntry{{
		Name: "*", Type: "http", Host: "api-a.example.com", URLHash: "any",
		FirstUsedAt: time.Now().Unix(),
	}}
	b, _ := json.MarshalIndent(seen, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "destinations_seen.json"), b, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := `
default_destination: alpha
servers:
  - name_glob: "*"
    destination: alpha
destinations:
  - name: alpha
    type: http
    url: https://api-a.example.com/in
  - name: bravo
    type: http
    url: https://api-b.example.com/in
queue:
  path: ` + filepath.Join(home, "queue") + `
`
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	exit, _, stderr := runCLI(t, home, "flush")
	if exit == 0 {
		t.Fatalf("expected non-zero exit because bravo is unapproved; got 0\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "bravo") {
		t.Errorf("error should name the unapproved destination bravo: %s", stderr)
	}
	// alpha should NOT appear in the blocked list (it matches the wildcard).
	// Checking against the part of the message after "approval:".
	if i := strings.Index(stderr, "require approval:"); i >= 0 {
		blockedSection := stderr[i:]
		if strings.Contains(blockedSection, "alpha") {
			t.Errorf("alpha was listed as blocked despite host-level approval: %s", stderr)
		}
	}
}

// TestStartSidecarRefusesUnapprovedDestinations verifies the start gate.
func TestStartSidecarRefusesUnapprovedDestinations(t *testing.T) {
	home := t.TempDir()
	cfg := `
default_destination: net
servers:
  - name_glob: "*"
    destination: net
destinations:
  - name: net
    type: http
    url: https://example.invalid/in
queue:
  path: ` + filepath.Join(home, "queue") + `
delivery:
  initial_backoff_ms: 1
  max_backoff_ms: 10
  max_retries: 1
`
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	exit, _, stderr := runCLI(t, home, "start", "--once")
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0; stderr: %s", stderr)
	}
	if !strings.Contains(stderr, "approval") {
		t.Errorf("stderr missing approval mention: %s", stderr)
	}
}

// TestDispatcherPreservesEnvelopeBytes verifies a payload that is NOT a
// ToolCallEvent (e.g. server_crashed) flows to the destination intact.
func TestDispatcherPreservesEnvelopeBytes(t *testing.T) {
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	id := event.NewULID()
	envelope := map[string]any{
		"id":              id,
		"schema":          event.CrashSchemaVersion,
		"exit_code":       137,
		"last_tool_names": []string{"alpha", "bravo"},
		"stderr_tail":     "killed",
	}
	raw, _ := json.Marshal(envelope)
	if err := q.Enqueue(id, "out", raw); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	captured := make(chan json.RawMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Schema string            `json:"schema"`
			Events []json.RawMessage `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		if len(body.Events) != 1 {
			t.Errorf("events len = %d", len(body.Events))
			return
		}
		captured <- body.Events[0]
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a, err := httpdest.New(httpdest.Config{Name: "out", URL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	reg := destinations.NewRegistry()
	if err := reg.Add(a); err != nil {
		t.Fatalf("registry add: %v", err)
	}
	d := dispatch.New(dispatch.Options{
		Queue: q, Registry: reg, BatchMax: 25,
		Backoff: dispatch.BackoffConfig{InitialMS: 1, MaxMS: 10, MaxRetries: 1},
	})
	if err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	select {
	case got := <-captured:
		var inner map[string]any
		if err := json.Unmarshal(got, &inner); err != nil {
			t.Fatalf("inner: %v", err)
		}
		if inner["schema"] != event.CrashSchemaVersion {
			t.Errorf("schema lost: %v", inner["schema"])
		}
		if inner["exit_code"].(float64) != 137 {
			t.Errorf("exit_code lost: %v", inner["exit_code"])
		}
		names, _ := inner["last_tool_names"].([]any)
		if len(names) != 2 {
			t.Errorf("last_tool_names lost: %v", inner["last_tool_names"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("destination never received the event")
	}
}

func TestInitWebhookScoutSynthesizesAgentID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "test-passphrase-correct-horse")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	exit, _, stderr := runCLI(t, home, "init", "--yes",
		"--destination", "webhookscout",
		"--api-key", "whs_live_abcdefghijklmnop",
		"--api-base", "https://api.webhookscout.test",
	)
	if exit != 0 {
		t.Fatalf("init exit = %d; stderr: %s", exit, stderr)
	}
	cfgBytes, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("read cfg: %v", err)
	}
	cfg := string(cfgBytes)
	if !strings.Contains(cfg, `"agent_id"`) || strings.Contains(cfg, `"agent_id": ""`) {
		t.Errorf("agent_id not synthesized; cfg:\n%s", cfg)
	}
	if !strings.Contains(cfg, `"webhookscout"`) {
		t.Errorf("type webhookscout not selected; cfg:\n%s", cfg)
	}
	if strings.Contains(cfg, "encfile://default-setup-token") {
		t.Errorf("setup-token misused as auth_header_ref")
	}
	if !strings.Contains(cfg, "encfile://default-api-key") {
		t.Errorf("api-key auth_header_ref missing; cfg:\n%s", cfg)
	}
	if strings.Contains(cfg, "whs_live_abcdefghijklmnop") {
		t.Errorf("plaintext api key written to config:\n%s", cfg)
	}
}

func TestInitSetupTokenExchangedNotPromotedToAuthRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOUTTRACE_ENCFILE_PASSPHRASE", "another-passphrase-here")
	t.Setenv("SCOUTTRACE_DISABLE_KEYCHAIN", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/setup-tokens/exchange" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent_id":"agent_setup_blocker","api_key":"whs_setup_secret"}`))
	}))
	defer srv.Close()
	exit, stdout, stderr := runCLI(t, home, "init", "--yes",
		"--destination", "webhookscout",
		"--api-base", srv.URL,
		"--setup-token", "wst_test_abcdef1234567890",
	)
	if exit != 0 {
		t.Fatalf("init exit = %d; stdout: %s stderr: %s", exit, stdout, stderr)
	}
	cfgBytes, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	cfg := string(cfgBytes)
	if strings.Contains(cfg, "encfile://default-setup-token") {
		t.Errorf("setup-token must not be auth_header_ref:\n%s", cfg)
	}
	if !strings.Contains(cfg, "encfile://default-api-key") {
		t.Errorf("exchanged api key auth ref missing:\n%s", cfg)
	}
	if !strings.Contains(cfg, "agent_setup_blocker") {
		t.Errorf("exchanged agent id missing:\n%s", cfg)
	}
	if strings.Contains(cfg+stdout+stderr, "wst_test_abcdef1234567890") || strings.Contains(cfg+stdout+stderr, "whs_setup_secret") {
		t.Errorf("setup token or exchanged api key leaked\nconfig:%s\nstdout:%s\nstderr:%s", cfg, stdout, stderr)
	}
}
