package event

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// NewTraceID returns a 16-byte hex-encoded trace id.
func NewTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("scouttrace/event: rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// NewSpanID returns an 8-byte hex-encoded span id.
func NewSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("scouttrace/event: rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// New returns a freshly-minted ToolCallEvent with id/timestamps populated.
// Call sites then fill in source/server/tool/request/response and pass it
// to the redactor.
func New(session *SessionState, scoutVersion string, host string, hostVersion string) *ToolCallEvent {
	now := time.Now().UTC()
	return &ToolCallEvent{
		ID:         NewULIDAt(now),
		Schema:     SchemaVersion,
		CapturedAt: now,
		SessionID:  session.SessionID,
		TraceID:    NewTraceID(),
		SpanID:     NewSpanID(),
		Source: SourceBlock{
			Kind:              "mcp_stdio",
			Host:              host,
			HostVersion:       hostVersion,
			ScoutTraceVersion: scoutVersion,
		},
	}
}
