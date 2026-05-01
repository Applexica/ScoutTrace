package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
)

// writeConfigYAML writes a minimal YAML config that points at the supplied
// HTTP destination. The caller must enqueue at least one record before
// calling flush so the dispatcher actually runs Send.
func writeConfigYAML(t *testing.T, home, url string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `
default_destination: net
servers:
  - name_glob: "*"
    destination: net
destinations:
  - name: net
    type: http
    url: ` + url + `
queue:
  path: ` + filepath.Join(home, "queue") + `
delivery:
  initial_backoff_ms: 1
  max_backoff_ms: 10
  max_retries: 1
  jitter: false
`
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func enqueueOne(t *testing.T, home string) {
	t.Helper()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(home, "queue")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	defer q.Close()
	id := event.NewULID()
	payload, _ := json.Marshal(map[string]any{"id": id, "schema": event.SchemaVersion})
	if err := q.Enqueue(id, "net", payload); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

func TestFlushBlocksUnapprovedHTTPDestination(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer srv.Close()

	home := t.TempDir()
	writeConfigYAML(t, home, srv.URL)
	enqueueOne(t, home)

	exit, _, stderr := runCLI(t, home, "flush")
	if exit == 0 {
		t.Fatalf("expected non-zero exit when destination not approved; stderr: %s", stderr)
	}
	if !strings.Contains(stderr, "approval") {
		t.Errorf("stderr missing approval mention: %s", stderr)
	}
	if hits != 0 {
		t.Errorf("destination was called %d times despite no approval", hits)
	}
}

func TestFlushAutoApprovesWithYesFlag(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer srv.Close()

	home := t.TempDir()
	writeConfigYAML(t, home, srv.URL)
	enqueueOne(t, home)

	exit, stdout, stderr := runCLI(t, home, "flush", "--yes")
	if exit != 0 {
		t.Fatalf("flush --yes exit = %d; stderr: %s", exit, stderr)
	}
	if hits == 0 {
		t.Errorf("expected at least one HTTP request; got 0\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	// Subsequent flush without --yes should now succeed because approval was recorded.
	enqueueOne(t, home)
	exit, _, stderr = runCLI(t, home, "flush")
	if exit != 0 {
		t.Fatalf("second flush (post-approval) exit = %d; stderr: %s", exit, stderr)
	}
	if hits < 2 {
		t.Errorf("expected ≥2 HTTP hits after second flush, got %d", hits)
	}
	// destinations_seen.json must exist with our destination.
	b, err := os.ReadFile(filepath.Join(home, "destinations_seen.json"))
	if err != nil {
		t.Fatalf("destinations_seen missing: %v", err)
	}
	if !bytes.Contains(b, []byte(`"net"`)) {
		t.Errorf("destinations_seen missing approved entry: %s", b)
	}
}

func TestFlushToAliasMatchesDestination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	home := t.TempDir()
	writeConfigYAML(t, home, srv.URL)
	enqueueOne(t, home)
	exit, _, stderr := runCLI(t, home, "flush", "--yes", "--to", "net")
	if exit != 0 {
		t.Fatalf("flush --to exit = %d; stderr: %s", exit, stderr)
	}
}

func TestYAMLConfigParse(t *testing.T) {
	yaml := `
default_destination: out
servers:
  - name_glob: "*"
    destination: out
destinations:
  - name: out
    type: stdout
redaction:
  profile: strict
`
	c, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse YAML: %v", err)
	}
	if c.DefaultDestination != "out" {
		t.Errorf("default_destination = %q, want out", c.DefaultDestination)
	}
	if len(c.Destinations) != 1 || c.Destinations[0].Type != "stdout" {
		t.Errorf("destinations: %+v", c.Destinations)
	}
	if c.Redaction.Profile != "strict" {
		t.Errorf("redaction.profile = %q, want strict", c.Redaction.Profile)
	}
}

func TestYAMLConfigRejectsPlaintextAuth(t *testing.T) {
	yaml := `
destinations:
  - name: bad
    type: http
    url: https://x
    headers:
      Authorization: "Bearer xyz"
`
	_, err := config.Parse([]byte(yaml))
	if err == nil {
		t.Fatalf("expected plaintext-auth error")
	}
	if !strings.Contains(err.Error(), "PLAINTEXT_AUTH") {
		t.Errorf("err = %v, want PLAINTEXT_AUTH", err)
	}
}
