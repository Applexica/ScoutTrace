package redact

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Result records what changed during Apply.
type Result struct {
	FieldsRedacted []string
	RulesApplied   []string
	Truncated      bool
}

// Counters are atomically-incremented metrics observed by the engine.
type Counters struct {
	Panics           uint64
	RulesDisabled    uint64
	EnvelopesDropped uint64
}

// Engine applies a Policy to JSON envelopes. Goroutine-safe.
type Engine struct {
	policy *Policy
	stats  *Counters
}

// NewEngine returns an engine bound to a validated policy.
func NewEngine(p *Policy, stats *Counters) (*Engine, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if stats == nil {
		stats = &Counters{}
	}
	return &Engine{policy: p, stats: stats}, nil
}

// Policy returns the bound policy.
func (e *Engine) Policy() *Policy { return e.policy }

// Apply mutates payload in place, returning the resulting JSON bytes.
//
// Errors: only surfaces hard JSON parse errors; rule panics are isolated,
// counted, and the offending rule is disabled for the rest of the process.
func (e *Engine) Apply(payload []byte) ([]byte, *Result, error) {
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, nil, err
	}
	res := &Result{}
	for i := range e.policy.Rules {
		if e.policy.IsRuleDisabled(i) {
			continue
		}
		r := &e.policy.Rules[i]
		if e.applyOne(&root, r, res, i) {
			res.RulesApplied = append(res.RulesApplied, r.Name)
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, nil, err
	}
	// Stable, sorted, deduped fields list.
	res.FieldsRedacted = uniqStrings(res.FieldsRedacted)
	return out, res, nil
}

func (e *Engine) applyOne(root *any, r *Rule, res *Result, idx int) (applied bool) {
	defer func() {
		if rec := recover(); rec != nil {
			atomic.AddUint64(&e.stats.Panics, 1)
			atomic.AddUint64(&e.stats.RulesDisabled, 1)
			e.policy.disableRule(idx)
			applied = false
		}
	}()
	switch r.Type {
	case RuleTruncate:
		return applyTruncate(root, r, res)
	case RuleRedactPattern:
		return applyPatterns(root, r, res, "")
	case RuleTransform:
		return applyTransform(root, r, res, "")
	case RuleDrop:
		return applyDrop(root, r, res)
	}
	return false
}

func uniqStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	sort.Strings(in)
	out := in[:0]
	last := ""
	for i, s := range in {
		if i == 0 || s != last {
			out = append(out, s)
			last = s
		}
	}
	return out
}

// ----- truncate -----

func applyTruncate(root *any, r *Rule, res *Result) bool {
	target, parent, key, idx, ok := resolvePathMutable(*root, r.Field)
	if !ok {
		return false
	}
	switch v := target.(type) {
	case string:
		if len(v) > r.LimitBytes {
			ph := r.Placeholder
			if ph == "" {
				ph = fmt.Sprintf("[truncated %d bytes]", len(v))
			}
			setMutable(parent, key, idx, ph)
			res.FieldsRedacted = append(res.FieldsRedacted, r.Field)
			res.Truncated = true
			return true
		}
	default:
		// For maps/arrays, measure marshalled size.
		b, err := json.Marshal(v)
		if err == nil && len(b) > r.LimitBytes {
			ph := r.Placeholder
			if ph == "" {
				ph = fmt.Sprintf("[truncated %d bytes]", len(b))
			}
			setMutable(parent, key, idx, ph)
			res.FieldsRedacted = append(res.FieldsRedacted, r.Field)
			res.Truncated = true
			return true
		}
	}
	return false
}

// ----- patterns -----

func applyPatterns(root *any, r *Rule, res *Result, prefix string) bool {
	any := false
	walkStrings(root, prefix, func(path string, s string) (string, bool) {
		newS := s
		hit := false
		for _, cp := range r.patternRegexps {
			ph := r.Placeholder
			if ph == "" {
				ph = "[REDACTED:" + cp.name + "]"
			} else {
				ph = strings.ReplaceAll(ph, "${pattern_name}", cp.name)
			}
			before := newS
			newS = cp.re.ReplaceAllString(newS, ph)
			if newS != before {
				res.FieldsRedacted = append(res.FieldsRedacted, path)
				hit = true
			}
		}
		if hit {
			any = true
			return newS, true
		}
		return s, false
	})
	return any
}

// ----- transform -----

func applyTransform(root *any, r *Rule, res *Result, prefix string) bool {
	any := false
	walkStrings(root, prefix, func(path string, s string) (string, bool) {
		// path is "a.b.c". Field name = last segment.
		fieldName := path
		if i := strings.LastIndex(path, "."); i >= 0 {
			fieldName = path[i+1:]
		}
		if !r.fieldsMatchCompiled.MatchString(fieldName) {
			return s, false
		}
		var newS string
		if r.replaceCompiled != nil {
			newS = r.replaceCompiled.ReplaceAllString(s, r.ReplaceTo)
		} else {
			newS = strings.ReplaceAll(s, r.ReplaceFrom, r.ReplaceTo)
		}
		if newS != s {
			res.FieldsRedacted = append(res.FieldsRedacted, path)
			any = true
			return newS, true
		}
		return s, false
	})
	return any
}

// ----- drop -----

func applyDrop(root *any, r *Rule, res *Result) bool {
	any := false
	for _, fp := range r.FieldPaths {
		if dropPath(root, fp) {
			res.FieldsRedacted = append(res.FieldsRedacted, fp)
			any = true
		}
	}
	return any
}
