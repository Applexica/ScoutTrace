package billing

import (
	"encoding/json"
	"testing"
)

func TestExtractFromCommonKeysAtTopLevel(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantCost   float64
		wantInTok  int
		wantOutTok int
		wantModel  string
		wantProv   string
	}{
		{
			name:       "snake case",
			input:      `{"cost_usd":0.012,"tokens_in":100,"tokens_out":50,"model":"claude-sonnet-4-6","provider":"anthropic"}`,
			wantCost:   0.012,
			wantInTok:  100,
			wantOutTok: 50,
			wantModel:  "claude-sonnet-4-6",
			wantProv:   "anthropic",
		},
		{
			name:      "camel case",
			input:     `{"costUsd":0.5,"tokensIn":1,"tokensOut":2,"modelName":"gpt-4o-mini","providerName":"openai"}`,
			wantCost:  0.5,
			wantInTok: 1, wantOutTok: 2,
			wantModel: "gpt-4o-mini",
			wantProv:  "openai",
		},
		{
			name:      "uppercase USD and total cost",
			input:     `{"costUSD":0.25,"input_tokens":10,"output_tokens":20,"model_id":"claude-haiku-4-5"}`,
			wantCost:  0.25,
			wantInTok: 10, wantOutTok: 20,
			wantModel: "claude-haiku-4-5",
		},
		{
			name:     "total_cost_usd alias",
			input:    `{"total_cost_usd":1.0}`,
			wantCost: 1.0,
		},
		{
			name:     "totalCostUsd alias",
			input:    `{"totalCostUsd":2.5,"model":"sonnet"}`,
			wantCost: 2.5, wantModel: "sonnet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Extract(json.RawMessage(tc.input))
			if tc.wantCost != 0 && (b.CostUSD == nil || *b.CostUSD != tc.wantCost) {
				t.Fatalf("cost = %v, want %v", b.CostUSD, tc.wantCost)
			}
			if tc.wantInTok != 0 && (b.TokensIn == nil || *b.TokensIn != tc.wantInTok) {
				t.Fatalf("tokens_in = %v, want %v", b.TokensIn, tc.wantInTok)
			}
			if tc.wantOutTok != 0 && (b.TokensOut == nil || *b.TokensOut != tc.wantOutTok) {
				t.Fatalf("tokens_out = %v, want %v", b.TokensOut, tc.wantOutTok)
			}
			if tc.wantModel != "" && b.Model != tc.wantModel {
				t.Fatalf("model = %q, want %q", b.Model, tc.wantModel)
			}
			if tc.wantProv != "" && b.Provider != tc.wantProv {
				t.Fatalf("provider = %q, want %q", b.Provider, tc.wantProv)
			}
		})
	}
}

func TestExtractFromUsageNested(t *testing.T) {
	raw := json.RawMessage(`{"model":"claude-opus-4-7","usage":{"input_tokens":250,"output_tokens":75}}`)
	b := Extract(raw)
	if b.Model != "claude-opus-4-7" {
		t.Fatalf("model = %q", b.Model)
	}
	if b.TokensIn == nil || *b.TokensIn != 250 {
		t.Fatalf("tokens_in = %v", b.TokensIn)
	}
	if b.TokensOut == nil || *b.TokensOut != 75 {
		t.Fatalf("tokens_out = %v", b.TokensOut)
	}
}

func TestExtractFromMCPContentTextPayload(t *testing.T) {
	// MCP servers wrap tool responses as {content: [{type:"text", text:"<json>"}]}
	raw := json.RawMessage(`{"content":[{"type":"text","text":"{\"cost_usd\":0.04,\"model\":\"gpt-4o\"}"}]}`)
	b := Extract(raw)
	if b.CostUSD == nil || *b.CostUSD != 0.04 {
		t.Fatalf("cost = %v", b.CostUSD)
	}
	if b.Model != "gpt-4o" {
		t.Fatalf("model = %q", b.Model)
	}
}

func TestExtractEmptyOnUnknownShape(t *testing.T) {
	raw := json.RawMessage(`{"hello":"world"}`)
	b := Extract(raw)
	if b.CostUSD != nil || b.TokensIn != nil || b.TokensOut != nil ||
		b.Model != "" || b.Provider != "" {
		t.Fatalf("expected empty billing, got %+v", b)
	}
}

func TestExtractIgnoresMalformedNestedJSON(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"not json"}]}`)
	b := Extract(raw)
	if b.CostUSD != nil || b.Model != "" {
		t.Fatalf("expected empty billing, got %+v", b)
	}
}

func TestMergePrefersExistingNonZero(t *testing.T) {
	cost := 0.10
	tokensIn := 100
	dst := Block{CostUSD: &cost, TokensIn: &tokensIn, Model: "set"}
	addTokensOut := 50
	addCost := 0.99
	src := Block{CostUSD: &addCost, TokensOut: &addTokensOut, Model: "ignored", Provider: "anthropic"}
	out := Merge(dst, src)
	if out.CostUSD == nil || *out.CostUSD != 0.10 {
		t.Fatalf("Merge clobbered existing cost: %v", out.CostUSD)
	}
	if out.TokensIn == nil || *out.TokensIn != 100 {
		t.Fatalf("Merge clobbered existing tokens_in: %v", out.TokensIn)
	}
	if out.TokensOut == nil || *out.TokensOut != 50 {
		t.Fatalf("Merge missed src tokens_out: %v", out.TokensOut)
	}
	if out.Model != "set" {
		t.Fatalf("Merge clobbered existing model: %q", out.Model)
	}
	if out.Provider != "anthropic" {
		t.Fatalf("Merge missed src provider: %q", out.Provider)
	}
}

func TestExtractTolerantOfNullsAndEmpty(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("")} {
		b := Extract(raw)
		if b.CostUSD != nil || b.Model != "" {
			t.Fatalf("expected empty for %q, got %+v", string(raw), b)
		}
	}
}
