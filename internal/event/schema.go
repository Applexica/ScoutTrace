// Package event defines the canonical ToolCallEvent envelope and related
// schema constants used end-to-end by the capture pipeline, queue, and
// destination adapters.
package event

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the current ToolCallEvent envelope schema. Append-only:
// any non-additive change requires a v2 constant and a schema-graduation PR.
const (
	SchemaVersion        = "scouttrace.toolcall.v1"
	SessionSchemaVersion = "scouttrace.session.v1"
	CrashSchemaVersion   = "scouttrace.server_crashed.v1"
	RecoverSchemaVersion = "scouttrace.queue_recovered.v1"
)

// SourceBlock captures provenance about the host & ScoutTrace build.
type SourceBlock struct {
	Kind              string `json:"kind"` // "mcp_stdio"
	Host              string `json:"host,omitempty"`
	HostVersion       string `json:"host_version,omitempty"`
	ScoutTraceVersion string `json:"scouttrace_version"`
}

// ServerBlock identifies the upstream MCP server.
type ServerBlock struct {
	Name            string   `json:"name"`
	CommandHash     string   `json:"command_hash,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// ToolBlock identifies a particular tool within a server.
type ToolBlock struct {
	Name       string `json:"name"`
	SchemaHash string `json:"schema_hash,omitempty"`
}

// RequestBlock is the request side of a tools/call exchange after redaction.
type RequestBlock struct {
	JSONRPCID         string          `json:"json_rpc_id"`
	Args              json.RawMessage `json:"args,omitempty"`
	ArgsTruncated     bool            `json:"args_truncated"`
	ArgsBytesOriginal int             `json:"args_bytes_original"`
}

// ResponseBlock is the response side of a tools/call exchange after redaction.
type ResponseBlock struct {
	OK                  bool            `json:"ok"`
	Result              json.RawMessage `json:"result,omitempty"`
	ResultTruncated     bool            `json:"result_truncated"`
	ResultBytesOriginal int             `json:"result_bytes_original"`
	Error               json.RawMessage `json:"error,omitempty"`
}

// TimingBlock holds millisecond-resolution timing data.
type TimingBlock struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	LatencyMS int64     `json:"latency_ms"`
}

// RedactionBlock records which policy ran and which fields it touched.
type RedactionBlock struct {
	PolicyName     string   `json:"policy_name"`
	PolicyHash     string   `json:"policy_hash"`
	FieldsRedacted []string `json:"fields_redacted"`
	RulesApplied   []string `json:"rules_applied"`
}

// BillingBlock holds optional cost/token/model accounting metadata. All
// fields are optional — pointer-typed numerics distinguish "not reported"
// from "reported as zero." PricingSource records where the cost came from
// (e.g. "reported", "estimated", "static").
type BillingBlock struct {
	CostUSD       *float64 `json:"cost_usd,omitempty"`
	TokensIn      *int     `json:"tokens_in,omitempty"`
	TokensOut     *int     `json:"tokens_out,omitempty"`
	Model         string   `json:"model,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	PricingSource string   `json:"pricing_source,omitempty"`
}

// ToolCallEvent is the canonical post-redaction envelope.
type ToolCallEvent struct {
	ID         string         `json:"id"`
	Schema     string         `json:"schema"`
	CapturedAt time.Time      `json:"captured_at"`
	SessionID  string         `json:"session_id"`
	TraceID    string         `json:"trace_id,omitempty"`
	SpanID     string         `json:"span_id,omitempty"`
	Source     SourceBlock    `json:"source"`
	Server     ServerBlock    `json:"server"`
	Tool       ToolBlock      `json:"tool"`
	Request    RequestBlock   `json:"request"`
	Response   ResponseBlock  `json:"response"`
	Timing     TimingBlock    `json:"timing"`
	Redaction  RedactionBlock `json:"redaction"`
	Billing    *BillingBlock  `json:"billing,omitempty"`
}

// Session metadata held per proxy invocation.
type SessionState struct {
	ServerName      string
	ProtocolVersion string
	Capabilities    []string
	ServerInfo      map[string]any
	Initialized     bool
	SessionID       string
	StartedAt       time.Time
}

// NewSession creates a new session state with a freshly-minted session ULID.
func NewSession(serverName string) *SessionState {
	return &SessionState{
		ServerName: serverName,
		SessionID:  NewULID(),
		StartedAt:  time.Now().UTC(),
	}
}
