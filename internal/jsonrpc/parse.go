package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Message is a minimal view of a JSON-RPC 2.0 frame. ID is held as a
// RawMessage because the spec allows both strings and numbers and we
// must preserve the original bytes for correlation.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// ErrEmpty is returned when an empty/whitespace-only frame is parsed.
var ErrEmpty = errors.New("jsonrpc: empty frame")

// Parse decodes one frame. Trailing whitespace is tolerated.
func Parse(b []byte) (*Message, error) {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return nil, ErrEmpty
	}
	var m Message
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// IsRequest reports whether the message is a request: it has a method and
// an id. (Notifications have a method but no id.)
func (m *Message) IsRequest() bool {
	return m.Method != "" && len(m.ID) != 0
}

// IsNotification reports whether the message is a notification.
func (m *Message) IsNotification() bool {
	return m.Method != "" && len(m.ID) == 0
}

// IsResponse reports whether the message is a response (has an id and
// either result or error).
func (m *Message) IsResponse() bool {
	return len(m.ID) != 0 && m.Method == "" && (len(m.Result) != 0 || len(m.Error) != 0)
}

// CanonicalID returns a stable string form of the JSON-RPC id, preserving
// the type distinction between numeric and string ids (per JSON-RPC §4.1,
// "42" and 42 are different ids).
func CanonicalID(id json.RawMessage) string {
	t := bytes.TrimSpace(id)
	if len(t) == 0 {
		return ""
	}
	if t[0] == '"' {
		// String id: prefix to disambiguate from "42".
		return "s:" + string(t)
	}
	return "n:" + string(t)
}
