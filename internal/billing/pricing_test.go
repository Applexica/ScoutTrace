package billing

import "testing"

func TestEstimateReturnsZeroWhenModelUnknown(t *testing.T) {
	cost, source, ok := Estimate("totally-unknown-model", 100, 100)
	if ok {
		t.Fatalf("ok = true, want false (unknown model). cost=%v source=%q", cost, source)
	}
}

func TestEstimateReturnsZeroWhenTokensZero(t *testing.T) {
	if _, _, ok := Estimate("claude-sonnet-4-6", 0, 0); ok {
		t.Fatalf("ok = true, want false (no tokens)")
	}
}

func TestEstimateMatchesByModelSubstring(t *testing.T) {
	cases := []struct {
		model     string
		tokensIn  int
		tokensOut int
	}{
		{"claude-sonnet-4-6", 1000, 1000},
		{"anthropic/claude-haiku-4-5", 1000, 1000},
		{"claude-opus-4-7-1m", 1000, 1000},
		{"gpt-4o-mini-2024-07-18", 1000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			cost, source, ok := Estimate(tc.model, tc.tokensIn, tc.tokensOut)
			if !ok {
				t.Fatalf("unknown model %q: should match family", tc.model)
			}
			if cost <= 0 {
				t.Fatalf("cost = %v, want > 0", cost)
			}
			if source != "estimated" {
				t.Fatalf("source = %q, want estimated", source)
			}
		})
	}
}

func TestEstimateOpusCostsMoreThanHaiku(t *testing.T) {
	opusCost, _, _ := Estimate("claude-opus-4-7", 10000, 10000)
	haikuCost, _, _ := Estimate("claude-haiku-4-5", 10000, 10000)
	if !(opusCost > haikuCost) {
		t.Fatalf("opus (%v) should cost more than haiku (%v)", opusCost, haikuCost)
	}
}

func TestEstimateScalesLinearlyWithTokens(t *testing.T) {
	c1, _, _ := Estimate("claude-sonnet-4-6", 1000, 1000)
	c10, _, _ := Estimate("claude-sonnet-4-6", 10000, 10000)
	// Allow tiny float slop.
	ratio := c10 / c1
	if ratio < 9.9 || ratio > 10.1 {
		t.Fatalf("ratio = %v, want ~10 (c1=%v c10=%v)", ratio, c1, c10)
	}
}
