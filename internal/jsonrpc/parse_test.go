package jsonrpc

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseRequest(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"x"}}`)
	m, err := Parse(frame)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !m.IsRequest() {
		t.Fatalf("expected request: %+v", m)
	}
	if m.Method != "tools/call" {
		t.Fatalf("method = %q, want tools/call", m.Method)
	}
}

func TestParseNotificationVsRequest(t *testing.T) {
	cases := []struct {
		name           string
		frame          string
		isReq, isNotif bool
	}{
		{"req", `{"jsonrpc":"2.0","id":1,"method":"foo"}`, true, false},
		{"notif", `{"jsonrpc":"2.0","method":"notifications/cancelled"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(tc.frame))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := m.IsRequest(); got != tc.isReq {
				t.Errorf("IsRequest = %v, want %v", got, tc.isReq)
			}
			if got := m.IsNotification(); got != tc.isNotif {
				t.Errorf("IsNotification = %v, want %v", got, tc.isNotif)
			}
		})
	}
}

func TestCanonicalIDStringVsNumber(t *testing.T) {
	num := CanonicalID(json.RawMessage(`42`))
	str := CanonicalID(json.RawMessage(`"42"`))
	if num == str {
		t.Fatalf("expected distinct canonical ids for 42 vs \"42\"")
	}
}

func TestCorrelatorMatch(t *testing.T) {
	c := NewCorrelator(0)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return now })

	req, _ := Parse([]byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"x"}}`))
	c.AddRequest(req, "span-1")
	now = now.Add(50 * time.Millisecond)
	resp, _ := Parse([]byte(`{"jsonrpc":"2.0","id":42,"result":{"ok":true}}`))
	pair, ok := c.MatchResponse(resp)
	if !ok {
		t.Fatalf("expected match")
	}
	if pair.Method != "tools/call" {
		t.Errorf("method = %q", pair.Method)
	}
	if pair.SpanID != "span-1" {
		t.Errorf("span = %q", pair.SpanID)
	}
	if pair.EndedAt.Sub(pair.StartedAt) != 50*time.Millisecond {
		t.Errorf("latency = %v", pair.EndedAt.Sub(pair.StartedAt))
	}
}

func TestCorrelatorMissCount(t *testing.T) {
	c := NewCorrelator(time.Hour)
	resp, _ := Parse([]byte(`{"jsonrpc":"2.0","id":99,"result":{}}`))
	if _, ok := c.MatchResponse(resp); ok {
		t.Fatalf("expected miss")
	}
	misses, _, _ := c.Counters()
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
}

func TestCorrelatorSweepOrphans(t *testing.T) {
	c := NewCorrelator(time.Second)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return now })

	req, _ := Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))
	c.AddRequest(req, "")
	now = now.Add(2 * time.Second)
	orphans := c.SweepOrphans()
	if len(orphans) != 1 {
		t.Fatalf("orphans = %d, want 1", len(orphans))
	}
}
