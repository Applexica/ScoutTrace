// Package jsonrpc implements the small subset of JSON-RPC 2.0 needed by the
// MCP capture pipeline: parsing, request/response correlation, and a method
// catalog that drives capture-level granularity.
package jsonrpc

// CaptureLevel determines how much of an MCP method exchange the capture
// pipeline records. Higher levels include all behavior of lower levels.
type CaptureLevel int

const (
	// Suppressed: do not capture anything, not even a counter.
	Suppressed CaptureLevel = iota
	// Counted: bump a counter, do not emit a per-call envelope.
	Counted
	// Metadata: emit an envelope with method/latency/ok-or-err but no payload.
	Metadata
	// Full: emit a fully-redacted ToolCallEvent including args + result.
	Full
)

// Method names used by MCP.
const (
	MethodInitialize        = "initialize"
	MethodToolsList         = "tools/list"
	MethodToolsCall         = "tools/call"
	MethodResourcesList     = "resources/list"
	MethodResourcesRead     = "resources/read"
	MethodPromptsList       = "prompts/list"
	MethodPromptsGet        = "prompts/get"
	MethodPing              = "ping"
	MethodNotifications     = "notifications/" // prefix
	MethodNotificationLevel = "notifications/message"
)

// LevelFor returns the capture level for a JSON-RPC method name.
func LevelFor(method string) CaptureLevel {
	switch method {
	case MethodInitialize:
		return Metadata
	case MethodToolsList:
		return Metadata
	case MethodToolsCall:
		return Full
	case MethodResourcesList, MethodResourcesRead:
		return Metadata
	case MethodPromptsList, MethodPromptsGet:
		return Metadata
	case MethodPing:
		return Suppressed
	}
	if len(method) >= len(MethodNotifications) && method[:len(MethodNotifications)] == MethodNotifications {
		return Counted
	}
	return Counted
}
