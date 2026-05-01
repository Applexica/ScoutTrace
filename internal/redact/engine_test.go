package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSecretsCorpus(t *testing.T) {
	pol := StrictProfile()
	eng, err := NewEngine(pol, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	corpus := map[string]string{
		"aws":    "AKIAABCDEFGHIJKLMNOP",
		"ghp":    "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"ant":    "sk-ant-api01-abcdefghijklmnop_abcdefghijk",
		"bearer": "Authorization Bearer abcdefghijklmnopqrstuvwxyz",
		"jwt":    "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		"email":  "user@example.com",
	}
	payload, _ := json.Marshal(map[string]any{"request": map[string]any{"args": corpus}})
	out, res, err := eng.Apply(payload)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, raw := range corpus {
		if strings.Contains(string(out), raw) {
			t.Errorf("output still contains %q", raw)
		}
	}
	if len(res.RulesApplied) == 0 {
		t.Errorf("no rules applied")
	}
}

func TestRedactTruncate(t *testing.T) {
	pol := &Policy{
		Name: "tr",
		Rules: []Rule{
			{Name: "trunc_args", Type: RuleTruncate, Field: "request.args", LimitBytes: 32, Placeholder: "[trunc]"},
		},
	}
	eng, err := NewEngine(pol, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	bigText := strings.Repeat("x", 200)
	payload, _ := json.Marshal(map[string]any{"request": map[string]any{"args": bigText}})
	out, res, err := eng.Apply(payload)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(string(out), "[trunc]") {
		t.Errorf("output not truncated: %s", out)
	}
	if !res.Truncated {
		t.Errorf("Result.Truncated false")
	}
}

func TestRedactDrop(t *testing.T) {
	pol := &Policy{
		Name: "d",
		Rules: []Rule{
			{Name: "drop_secret", Type: RuleDrop, FieldPaths: []string{"request.args.secret"}},
		},
	}
	eng, _ := NewEngine(pol, nil)
	payload, _ := json.Marshal(map[string]any{
		"request": map[string]any{"args": map[string]any{"secret": "xx", "ok": "y"}},
	})
	out, _, _ := eng.Apply(payload)
	if strings.Contains(string(out), "secret") {
		t.Errorf("secret not dropped: %s", out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Errorf("non-targeted field also dropped: %s", out)
	}
}

func TestRedactPanicIsolation(t *testing.T) {
	// Construct a rule that we then mutate to force a nil-deref panic
	// inside applyOne. We do this by adding a transform rule whose
	// fieldsMatchCompiled is nil after compile by sabotaging it.
	pol := &Policy{
		Name: "p",
		Rules: []Rule{
			{Name: "ok", Type: RuleRedactPattern, Patterns: []string{`secret`}, Placeholder: "X"},
			{Name: "bad", Type: RuleTransform, FieldsMatchRegex: `^x$`, ReplaceFrom: "a", ReplaceTo: "b"},
			{Name: "ok2", Type: RuleRedactPattern, Patterns: []string{`tokenA`}, Placeholder: "X"},
		},
	}
	eng, err := NewEngine(pol, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// Sabotage the bad rule's compiled regex so it panics on string match.
	pol.Rules[1].fieldsMatchCompiled = nil
	payload := []byte(`{"a":"secret tokenA","x":"y"}`)
	out, _, err := eng.Apply(payload)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("first rule did not run: %s", out)
	}
	if strings.Contains(string(out), "tokenA") {
		t.Errorf("post-panic rule did not run: %s", out)
	}
	if eng.stats.Panics == 0 {
		t.Errorf("panic counter not incremented")
	}
	if !pol.IsRuleDisabled(1) {
		t.Errorf("offending rule not disabled")
	}
}

func TestPolicyHashStable(t *testing.T) {
	a := StrictProfile()
	b := StrictProfile()
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate a: %v", err)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate b: %v", err)
	}
	if a.Hash() != b.Hash() {
		t.Errorf("hash differs across instances of strict profile")
	}
}

func TestCapturePolicyDeny(t *testing.T) {
	off := false
	cp := &CapturePolicy{Servers: []CaptureServer{
		{NameGlob: "*", CaptureArgs: &off},
	}}
	if cp.ShouldCaptureArgs("filesystem") {
		t.Errorf("expected capture_args=false for *")
	}
	cp = &CapturePolicy{}
	if !cp.ShouldCaptureArgs("filesystem") {
		t.Errorf("default should be capture_args=true")
	}
}
