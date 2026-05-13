package halt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGateAllowsWhenNotHalted(t *testing.T) {
	c := NewCache("")
	g := &Gate{Cache: c, AgentID: "a1"}
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"foo"}}`)
	f, reply, _ := g.Decide(frame)
	if !f {
		t.Fatalf("expected forward when not halted")
	}
	if len(reply) != 0 {
		t.Fatalf("expected empty reply when allowed, got %s", reply)
	}
}

func TestGateAllowsNonToolCall(t *testing.T) {
	c := NewCache("")
	_ = c.Set("a1", State{Halted: true, HaltReason: "hourly $5 crossed"})
	g := &Gate{Cache: c, AgentID: "a1"}
	// initialize / tools/list / ping must always forward — halt only
	// stops tool execution, not observation.
	cases := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping","params":{}}`,
	}
	for _, frame := range cases {
		f, _, _ := g.Decide([]byte(frame))
		if !f {
			t.Fatalf("expected forward for non-tools/call frame: %s", frame)
		}
	}
}

func TestGateBlocksToolCallWhenHalted(t *testing.T) {
	c := NewCache("")
	_ = c.Set("a1", State{Halted: true, HaltReason: "hourly $5 crossed", ManualClearRequired: false})
	g := &Gate{Cache: c, AgentID: "a1"}
	frame := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"db.write"}}`)
	f, reply, capture := g.Decide(frame)
	if f {
		t.Fatalf("expected block when halted")
	}
	if !capture {
		t.Fatalf("blocked frames should still be captured")
	}
	// Reply must be valid JSON-RPC error with code -32099 and the
	// original id preserved.
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Origin              string `json:"origin"`
				Reason              string `json:"reason"`
				ManualClearRequired bool   `json:"manual_clear_required"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &env); err != nil {
		t.Fatalf("reply not valid JSON: %v", err)
	}
	if env.JSONRPC != "2.0" {
		t.Fatalf("expected jsonrpc=2.0, got %q", env.JSONRPC)
	}
	if string(env.ID) != "42" {
		t.Fatalf("expected id=42, got %s", env.ID)
	}
	if env.Error.Code != JSONRPCBlockedCode {
		t.Fatalf("expected code %d, got %d", JSONRPCBlockedCode, env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "hourly $5 crossed") {
		t.Fatalf("error message missing reason: %s", env.Error.Message)
	}
	if env.Error.Data.Origin != "scouttrace.cost_gate" {
		t.Fatalf("expected origin scouttrace.cost_gate, got %q", env.Error.Data.Origin)
	}
}

func TestGateMissingAgentIDNoOps(t *testing.T) {
	c := NewCache("")
	_ = c.Set("a1", State{Halted: true})
	g := &Gate{Cache: c, AgentID: ""}
	f, _, _ := g.Decide([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	if !f {
		t.Fatalf("expected forward when agentID is empty")
	}
}

func TestGateAllowsToolCallNotification(t *testing.T) {
	c := NewCache("")
	_ = c.Set("a1", State{Halted: true, HaltReason: "x"})
	g := &Gate{Cache: c, AgentID: "a1"}
	// MCP tools/call must have an id (it's a request, not a notification).
	// A malformed notification can't receive a synthetic response — no id
	// means no way to correlate the reply — so we forward and let upstream
	// reject it. This is defensive: in practice MCP hosts never send this.
	frame := []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{}}`)
	f, _, _ := g.Decide(frame)
	if !f {
		t.Fatalf("expected forward for notification (no id)")
	}
}

func TestGatePreservesStringID(t *testing.T) {
	c := NewCache("")
	_ = c.Set("a1", State{Halted: true, HaltReason: "x"})
	g := &Gate{Cache: c, AgentID: "a1"}
	frame := []byte(`{"jsonrpc":"2.0","id":"call-abc","method":"tools/call","params":{"name":"foo"}}`)
	f, reply, _ := g.Decide(frame)
	if f {
		t.Fatalf("expected block")
	}
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(reply, &env); err != nil {
		t.Fatalf("reply not valid JSON: %v", err)
	}
	if string(env.ID) != `"call-abc"` {
		t.Fatalf("expected id=\"call-abc\", got %s", env.ID)
	}
}

func TestGateUnparseableFrameForwards(t *testing.T) {
	c := NewCache("")
	_ = c.Set("a1", State{Halted: true})
	g := &Gate{Cache: c, AgentID: "a1"}
	f, _, _ := g.Decide([]byte("not json"))
	if !f {
		t.Fatalf("unparseable frames should forward (capture catches parse errors)")
	}
}
