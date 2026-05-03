package event

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBillingBlockOmittedWhenAbsent(t *testing.T) {
	ev := ToolCallEvent{
		ID:        "evt_1",
		Schema:    SchemaVersion,
		SessionID: "sess",
	}
	b, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"billing"`) {
		t.Fatalf("expected no billing key when zero; got %s", b)
	}
}

func TestBillingBlockRoundTrip(t *testing.T) {
	costUSD := 0.0123
	tokensIn := 1500
	tokensOut := 800
	ev := ToolCallEvent{
		ID:        "evt_2",
		Schema:    SchemaVersion,
		SessionID: "sess",
		Billing: &BillingBlock{
			CostUSD:       &costUSD,
			TokensIn:      &tokensIn,
			TokensOut:     &tokensOut,
			Model:         "claude-sonnet-4-6",
			Provider:      "anthropic",
			PricingSource: "reported",
		},
	}
	b, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"cost_usd":0.0123`) ||
		!strings.Contains(string(b), `"tokens_in":1500`) ||
		!strings.Contains(string(b), `"tokens_out":800`) ||
		!strings.Contains(string(b), `"model":"claude-sonnet-4-6"`) ||
		!strings.Contains(string(b), `"provider":"anthropic"`) ||
		!strings.Contains(string(b), `"pricing_source":"reported"`) {
		t.Fatalf("unexpected billing JSON: %s", b)
	}
	var back ToolCallEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Billing == nil || back.Billing.CostUSD == nil || *back.Billing.CostUSD != 0.0123 {
		t.Fatalf("billing did not round trip: %+v", back.Billing)
	}
}

func TestBillingBlockOmitsEmptyInnerFields(t *testing.T) {
	tokensIn := 10
	bb := BillingBlock{TokensIn: &tokensIn}
	b, err := json.Marshal(&bb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"tokens_in":10`) {
		t.Fatalf("missing tokens_in: %s", s)
	}
	for _, key := range []string{"cost_usd", "tokens_out", "model", "provider", "pricing_source"} {
		if strings.Contains(s, `"`+key+`"`) {
			t.Fatalf("expected %s omitted, got %s", key, s)
		}
	}
}
