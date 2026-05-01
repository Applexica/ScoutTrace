// Package redact implements a small declarative redaction engine. Rules
// run in declaration order over the JSON tree of a captured envelope.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// RuleType describes the kind of transformation a rule performs.
type RuleType string

const (
	RuleTruncate      RuleType = "truncate"
	RuleRedactPattern RuleType = "redact_pattern"
	RuleTransform     RuleType = "transform"
	RuleDrop          RuleType = "drop"
)

// Rule is a single declarative redaction step.
type Rule struct {
	Name             string   `json:"name"`
	Type             RuleType `json:"type"`
	Field            string   `json:"field,omitempty"`
	FieldPaths       []string `json:"field_paths,omitempty"`
	LimitBytes       int      `json:"limit_bytes,omitempty"`
	Placeholder      string   `json:"placeholder,omitempty"`
	Patterns         []string `json:"patterns,omitempty"`
	FieldsMatchRegex string   `json:"fields_match_regex,omitempty"`
	ReplaceFrom      string   `json:"replace_from,omitempty"`
	ReplaceTo        string   `json:"replace_to,omitempty"`

	// Compiled state (set by compile()).
	patternRegexps      []*compiledPattern
	fieldsMatchCompiled *regexp.Regexp
	replaceCompiled     *regexp.Regexp
}

type compiledPattern struct {
	name string
	re   *regexp.Regexp
}

// Policy is an ordered set of rules plus a friendly name.
type Policy struct {
	Name  string `json:"name"`
	Rules []Rule `json:"rules"`

	hash     string
	disabled map[int]struct{}
}

// CapturePolicy controls capture-level decisions made before the envelope
// is even constructed (the AC-R2 backstop).
type CapturePolicy struct {
	// Servers maps glob pattern → server-level controls. Glob is a simple
	// `*` fnmatch.
	Servers []CaptureServer `json:"servers"`
}

// CaptureServer applies to servers whose name matches NameGlob.
type CaptureServer struct {
	NameGlob      string `json:"name_glob"`
	CaptureArgs   *bool  `json:"capture_args,omitempty"`
	CaptureResult *bool  `json:"capture_result,omitempty"`
}

// ShouldCaptureArgs returns whether args should be captured for server.
// Default is true.
func (c *CapturePolicy) ShouldCaptureArgs(server string) bool {
	for _, s := range c.Servers {
		if globMatch(s.NameGlob, server) && s.CaptureArgs != nil {
			return *s.CaptureArgs
		}
	}
	return true
}

// ShouldCaptureResult returns whether result should be captured for server.
// Default is true.
func (c *CapturePolicy) ShouldCaptureResult(server string) bool {
	for _, s := range c.Servers {
		if globMatch(s.NameGlob, server) && s.CaptureResult != nil {
			return *s.CaptureResult
		}
	}
	return true
}

// globMatch implements a minimal `*`-only glob.
func globMatch(pat, s string) bool {
	if pat == "" || pat == "*" {
		return true
	}
	if pat == s {
		return true
	}
	// Substring/prefix/suffix patterns: handle leading/trailing *.
	hasPrefix := false
	hasSuffix := false
	core := pat
	if len(core) > 0 && core[0] == '*' {
		hasPrefix = true
		core = core[1:]
	}
	if len(core) > 0 && core[len(core)-1] == '*' {
		hasSuffix = true
		core = core[:len(core)-1]
	}
	switch {
	case hasPrefix && hasSuffix:
		return contains(s, core)
	case hasPrefix:
		return hasSuffixOnly(s, core)
	case hasSuffix:
		return hasPrefixOnly(s, core)
	}
	return false
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasPrefixOnly(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func hasSuffixOnly(s, p string) bool { return len(s) >= len(p) && s[len(s)-len(p):] == p }

// Validate compiles all regexes and returns the first error encountered.
// On success the Policy is ready to apply.
func (p *Policy) Validate() error {
	if p == nil {
		return errors.New("redact: nil policy")
	}
	if p.disabled == nil {
		p.disabled = map[int]struct{}{}
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Name == "" {
			return fmt.Errorf("rule %d: name required", i)
		}
		switch r.Type {
		case RuleTruncate:
			if r.Field == "" {
				return fmt.Errorf("rule %s: field required", r.Name)
			}
			if r.LimitBytes <= 0 {
				return fmt.Errorf("rule %s: limit_bytes must be positive", r.Name)
			}
		case RuleRedactPattern:
			if len(r.Patterns) == 0 {
				return fmt.Errorf("rule %s: patterns required", r.Name)
			}
			for _, pat := range r.Patterns {
				re, err := regexp.Compile(pat)
				if err != nil {
					return fmt.Errorf("rule %s: pattern %q: %w", r.Name, pat, err)
				}
				r.patternRegexps = append(r.patternRegexps, &compiledPattern{name: r.Name, re: re})
			}
		case RuleTransform:
			if r.FieldsMatchRegex == "" {
				return fmt.Errorf("rule %s: fields_match_regex required", r.Name)
			}
			fre, err := regexp.Compile(r.FieldsMatchRegex)
			if err != nil {
				return fmt.Errorf("rule %s: fields_match_regex: %w", r.Name, err)
			}
			r.fieldsMatchCompiled = fre
			if r.ReplaceFrom != "" {
				vre, err := regexp.Compile(r.ReplaceFrom)
				if err != nil {
					return fmt.Errorf("rule %s: replace_from: %w", r.Name, err)
				}
				r.replaceCompiled = vre
			}
		case RuleDrop:
			if len(r.FieldPaths) == 0 {
				return fmt.Errorf("rule %s: field_paths required", r.Name)
			}
		default:
			return fmt.Errorf("rule %s: unknown type %q", r.Name, r.Type)
		}
	}
	if p.hash == "" {
		p.recomputeHash()
	}
	return nil
}

func (p *Policy) recomputeHash() {
	b, _ := json.Marshal(p.Rules)
	sum := sha256.Sum256(b)
	p.hash = "sha256:" + hex.EncodeToString(sum[:])
}

// Hash returns a deterministic content hash of the policy. Used to detect
// drift across a fleet (PRD §13.5).
func (p *Policy) Hash() string {
	if p.hash == "" {
		p.recomputeHash()
	}
	return p.hash
}

// IsRuleDisabled reports whether the i-th rule has been disabled by the
// runtime panic harness.
func (p *Policy) IsRuleDisabled(i int) bool {
	_, ok := p.disabled[i]
	return ok
}

// disableRule marks rule i as poison-pill, skipping it forever after.
func (p *Policy) disableRule(i int) { p.disabled[i] = struct{}{} }
