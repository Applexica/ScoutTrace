package proxy

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/queue"
	"github.com/webhookscout/scouttrace/internal/redact"
)

func TestEmitServerCrashedQueuesEnvelope(t *testing.T) {
	dir := t.TempDir()
	q, err := queue.Open(queue.Options{Dir: filepath.Join(dir, "q")})
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	pol := redact.StrictProfile()
	eng, _ := redact.NewEngine(pol, nil)
	cw := NewCaptureWorker(CaptureWorker{
		Session:      event.NewSession("filesystem"),
		Engine:       eng,
		Queue:        q,
		Destination:  "default",
		Host:         "test-host",
		ScoutVersion: "0.1.0",
	})
	cw.EmitServerCrashed(137, "killed", []string{"read_file", "write_file"}, "")
	st, err := q.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Pending != 1 {
		t.Fatalf("pending = %d, want 1", st.Pending)
	}
	got, err := q.ClaimPending("default", 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claim len = %d", len(got))
	}
	var env map[string]any
	if err := json.Unmarshal(got[0].Payload, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env["schema"] != event.CrashSchemaVersion {
		t.Errorf("schema = %v, want %s", env["schema"], event.CrashSchemaVersion)
	}
	if env["exit_code"].(float64) != 137 {
		t.Errorf("exit_code = %v, want 137", env["exit_code"])
	}
	if names, _ := env["last_tool_names"].([]any); len(names) != 2 {
		t.Errorf("last_tool_names = %v", env["last_tool_names"])
	}
}
