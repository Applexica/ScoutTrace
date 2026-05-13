package halt

import (
	"encoding/json"
	"fmt"

	"github.com/webhookscout/scouttrace/internal/jsonrpc"
)

// JSONRPCBlockedCode is the JSON-RPC error code returned to the host
// when a tools/call is refused because the agent is halted by a
// WebhookScout cost gate. -32099 sits inside the JSON-RPC implementation-
// defined server-error range (-32000 to -32099, per the JSON-RPC 2.0
// spec) and is reserved for "halted by cost gate".
const JSONRPCBlockedCode = -32099

// Gate is the wire.Gate that consults the in-memory halt Cache and
// refuses tools/call requests for halted agents. Any other JSON-RPC
// method (initialize, tools/list, resources/read, …) is always
// forwarded — halts only block tool execution, never observation.
type Gate struct {
	Cache   *Cache
	AgentID string
}

// gateDecision is the local equivalent of wire.Decision. We don't
// import wire here to avoid an import cycle (proxy → wire → halt
// would be cycle-prone if wire ever needed halt types). Instead the
// proxy CLI converts halt.GateDecision → wire.Decision.
type gateDecision struct {
	Forward bool
	Reply   []byte
	Capture bool
}

// Decide is the lower-level decision API. The CLI wraps this into a
// wire.Gate using a small adapter so wire stays decoupled from halt.
func (g *Gate) Decide(frame []byte) (forward bool, reply []byte, capture bool) {
	d := g.decide(frame)
	return d.Forward, d.Reply, d.Capture
}

func (g *Gate) decide(frame []byte) gateDecision {
	allow := gateDecision{Forward: true, Capture: true}
	if g == nil || g.Cache == nil || g.AgentID == "" {
		return allow
	}
	msg, err := jsonrpc.Parse(frame)
	if err != nil {
		// Unparseable frames bypass the gate — the wire layer will
		// still forward them and capture will record a parse error.
		return allow
	}
	if !msg.IsRequest() || msg.Method != jsonrpc.MethodToolsCall {
		return allow
	}
	state := g.Cache.Get(g.AgentID)
	if !state.Halted {
		return allow
	}
	reason := state.HaltReason
	if reason == "" {
		reason = "WebhookScout cost gate halt"
	}
	reply := buildBlockedResponse(msg.ID, reason, state.ManualClearRequired)
	return gateDecision{Forward: false, Reply: reply, Capture: true}
}

// buildBlockedResponse synthesizes the JSON-RPC error response that
// the host will see in place of the upstream's real reply. The shape
// matches the JSON-RPC 2.0 §5.1 error object; the `data` field carries
// machine-readable context the host (or its LLM) can act on.
func buildBlockedResponse(id json.RawMessage, reason string, manualClearRequired bool) []byte {
	idCopy := id
	if len(idCopy) == 0 {
		idCopy = json.RawMessage(`null`)
	}
	type errData struct {
		Origin              string `json:"origin"`
		Reason              string `json:"reason"`
		ManualClearRequired bool   `json:"manual_clear_required"`
	}
	type rpcError struct {
		Code    int     `json:"code"`
		Message string  `json:"message"`
		Data    errData `json:"data"`
	}
	type envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcError        `json:"error"`
	}
	env := envelope{
		JSONRPC: "2.0",
		ID:      idCopy,
		Error: rpcError{
			Code:    JSONRPCBlockedCode,
			Message: fmt.Sprintf("WebhookScout cost gate halt: %s", reason),
			Data: errData{
				Origin:              "scouttrace.cost_gate",
				Reason:              reason,
				ManualClearRequired: manualClearRequired,
			},
		},
	}
	out, _ := json.Marshal(env)
	return out
}
