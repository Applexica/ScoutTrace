package config

import "testing"

func TestParseCostToolPrices(t *testing.T) {
	src := `{
		"destinations":[{"name":"a","type":"stdout"}],
		"cost":{
			"tool_prices":[
				{"server_glob":"playwright","tool_glob":"browser_*","cost_usd":0.001,"provider":"playwright"},
				{"server_glob":"*","tool_glob":"my_tool","cost_usd":0.01,"provider":"x","model":"y"}
			]
		}
	}`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Cost.ToolPrices) != 2 {
		t.Fatalf("got %d tool_prices", len(c.Cost.ToolPrices))
	}
	if c.Cost.ToolPrices[0].ServerGlob != "playwright" {
		t.Fatalf("server_glob = %q", c.Cost.ToolPrices[0].ServerGlob)
	}
	if c.Cost.ToolPrices[0].CostUSD != 0.001 {
		t.Fatalf("cost_usd = %v", c.Cost.ToolPrices[0].CostUSD)
	}
}

func TestStaticPriceLookupFn(t *testing.T) {
	c := &Config{
		Cost: CostConfig{
			ToolPrices: []ToolPriceEntry{
				{ServerGlob: "playwright", ToolGlob: "browser_*", CostUSD: 0.001, Provider: "p", Model: "m"},
				{ServerGlob: "claude-code", ToolGlob: "Bash", CostUSD: 0.0005},
				{ServerGlob: "*", ToolGlob: "*", CostUSD: 0.0001},
			},
		},
	}
	lookup := c.StaticPriceLookup()
	cases := []struct {
		server, tool string
		wantOK       bool
		wantCost     float64
	}{
		{"playwright", "browser_click", true, 0.001},
		{"playwright", "screenshot", true, 0.0001},
		{"claude-code", "Bash", true, 0.0005},
		{"unknown", "nope", true, 0.0001},
	}
	for _, tc := range cases {
		got, ok := lookup(tc.server, tc.tool)
		if ok != tc.wantOK {
			t.Fatalf("(%s/%s) ok=%v want %v", tc.server, tc.tool, ok, tc.wantOK)
		}
		if tc.wantOK && got.CostUSD != tc.wantCost {
			t.Fatalf("(%s/%s) cost=%v want %v", tc.server, tc.tool, got.CostUSD, tc.wantCost)
		}
	}
}

func TestStaticPriceLookupNoMatchReturnsFalse(t *testing.T) {
	c := &Config{
		Cost: CostConfig{
			ToolPrices: []ToolPriceEntry{
				{ServerGlob: "playwright", ToolGlob: "browser_*", CostUSD: 0.001},
			},
		},
	}
	lookup := c.StaticPriceLookup()
	if _, ok := lookup("not-playwright", "x"); ok {
		t.Fatalf("expected no match")
	}
}

func TestParseCostLivePricing(t *testing.T) {
	src := `{
		"destinations":[{"name":"a","type":"stdout"}],
		"cost":{
			"live_pricing":{
				"enabled":true,
				"provider":"pricepertoken",
				"url":"https://api.pricepertoken.com/mcp/mcp",
				"timeout_ms":750,
				"cache_ttl_hours":12
			}
		}
	}`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lp := c.Cost.LivePricing
	if !lp.IsEnabled() {
		t.Fatalf("live pricing should be enabled")
	}
	if lp.EffectiveProvider() != "pricepertoken" || lp.URL == "" || lp.TimeoutMS != 750 || lp.CacheTTLHours != 12 {
		t.Fatalf("live pricing config = %#v", lp)
	}
}

func TestLivePricingDefaultsEnabled(t *testing.T) {
	c, err := Parse([]byte(`{"destinations":[{"name":"a","type":"stdout"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.Cost.LivePricing.IsEnabled() {
		t.Fatalf("live pricing should default enabled")
	}
	if c.Cost.LivePricing.EffectiveProvider() != "pricepertoken" {
		t.Fatalf("provider = %q", c.Cost.LivePricing.EffectiveProvider())
	}
	lookup, source := c.LiveLookup()
	if lookup == nil || source != "pricepertoken" {
		t.Fatalf("LiveLookup = %v, %q; want non-nil, pricepertoken", lookup, source)
	}
}

func TestLivePricingCanBeDisabled(t *testing.T) {
	c, err := Parse([]byte(`{
		"destinations":[{"name":"a","type":"stdout"}],
		"cost":{"live_pricing":{"enabled":false}}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Cost.LivePricing.IsEnabled() {
		t.Fatalf("live pricing should be disabled")
	}
	lookup, source := c.LiveLookup()
	if lookup != nil || source != "" {
		t.Fatalf("LiveLookup = %v, %q; want nil, empty", lookup, source)
	}
}

func TestLivePricingRejectsUnsupportedProvider(t *testing.T) {
	_, err := Parse([]byte(`{
		"destinations":[{"name":"a","type":"stdout"}],
		"cost":{"live_pricing":{"provider":"unknown"}}
	}`))
	if err == nil {
		t.Fatalf("expected error")
	}
}
