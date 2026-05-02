package webhookscout

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/webhookscout/scouttrace/internal/destinations"
	"github.com/webhookscout/scouttrace/internal/event"
)

type staticResolver map[string]string

func (r staticResolver) Resolve(ref string) (string, error) { return r[ref], nil }

func testToolCallEvent(t *testing.T, ok bool) json.RawMessage {
	t.Helper()
	ev := event.ToolCallEvent{
		ID:         "evt_1",
		Schema:     event.SchemaVersion,
		CapturedAt: time.Now().UTC(),
		SessionID:  "sess_1",
		Source:     event.SourceBlock{Kind: "mcp_stdio", Host: "claude-code", ScoutTraceVersion: "test"},
		Server:     event.ServerBlock{Name: "filesystem"},
		Tool:       event.ToolBlock{Name: "read_file"},
		Request: event.RequestBlock{
			JSONRPCID: "1",
			Args:      json.RawMessage(`{"path":"/tmp/demo.txt"}`),
		},
		Response: event.ResponseBlock{
			OK:     ok,
			Result: json.RawMessage(`{"content":"ok"}`),
		},
		Timing: event.TimingBlock{LatencyMS: 42},
	}
	if !ok {
		ev.Response.Error = json.RawMessage(`{"message":"not found"}`)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWebhookScoutPostsToMcpEventsEndpointWithBearerKey(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var reader io.Reader = r.Body
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
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	a, err := New(Config{Name: "default", APIBase: srv.URL, AgentID: "agent-1", AuthHeaderRef: "env://KEY"}, staticResolver{"env://KEY": "whs_test_key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := a.Send(context.Background(), destinations.Batch{ID: "batch-1", Events: []json.RawMessage{testToolCallEvent(t, true)}, PreparedAt: time.Now()})
	if !res.OK {
		t.Fatalf("Send result = %+v", res)
	}
	if gotPath != "/api/mcp/agent-1/events" {
		t.Fatalf("path = %q, want /api/mcp/agent-1/events", gotPath)
	}
	if gotAuth != "Bearer whs_test_key" {
		t.Fatalf("Authorization = %q, want Bearer whs_test_key", gotAuth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, gotBody)
	}
	if body["tool"] != "read_file" || body["status"] != "ok" || body["latencyMs"] != float64(42) {
		t.Fatalf("unexpected WebhookScout event body: %#v", body)
	}
	if _, hasBatchSchema := body["schema"]; hasBatchSchema {
		t.Fatalf("WebhookScout ingest body should be single MCP event, got batch/schema body: %#v", body)
	}
}

func TestWebhookScoutSendsEachBatchEventSeparately(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	a, err := New(Config{Name: "default", APIBase: srv.URL, AgentID: "agent-1"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := a.Send(context.Background(), destinations.Batch{ID: "batch-1", Events: []json.RawMessage{testToolCallEvent(t, true), testToolCallEvent(t, false)}, PreparedAt: time.Now()})
	if !res.OK {
		t.Fatalf("Send result = %+v", res)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}
