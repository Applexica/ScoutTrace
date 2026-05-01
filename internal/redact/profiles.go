package redact

// Built-in profiles. These are the canonical strict / standard / permissive
// rule sets shipped with ScoutTrace. They are kept in code (not embedded
// YAML) to reduce surface area in this stdlib-only build.

// Strict: redact every detectable secret, mask common PII.
func StrictProfile() *Policy {
	return &Policy{
		Name: "strict",
		Rules: []Rule{
			{
				Name:        "max_arg_bytes",
				Type:        RuleTruncate,
				Field:       "request.args",
				LimitBytes:  4096,
				Placeholder: "[truncated]",
			},
			{
				Name:        "max_result_bytes",
				Type:        RuleTruncate,
				Field:       "response.result",
				LimitBytes:  8192,
				Placeholder: "[truncated]",
			},
			{
				Name: "secrets_strict",
				Type: RuleRedactPattern,
				// AWS Access Key, GitHub PAT, Anthropic key, Bearer tokens,
				// Stripe live key, JWT-shaped tokens, generic high-entropy tokens.
				Patterns: []string{
					`AKIA[0-9A-Z]{16}`,
					`ghp_[A-Za-z0-9]{36,}`,
					`github_pat_[A-Za-z0-9_]{82,}`,
					`sk-ant-[A-Za-z0-9_-]{20,}`,
					`whs_(?:live|test)_[A-Za-z0-9]{16,}`,
					`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`,
					`sk_live_[A-Za-z0-9]{16,}`,
					`eyJ[A-Za-z0-9_=-]{8,}\.[A-Za-z0-9_=-]{8,}\.[A-Za-z0-9_.+/=-]{8,}`,
				},
				Placeholder: "[REDACTED:${pattern_name}]",
			},
			{
				Name:        "pii_email",
				Type:        RuleRedactPattern,
				Patterns:    []string{`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`},
				Placeholder: "[REDACTED:email]",
			},
			{
				Name:        "pii_credit_card",
				Type:        RuleRedactPattern,
				Patterns:    []string{`\b(?:\d[ -]*?){13,16}\b`},
				Placeholder: "[REDACTED:cc]",
			},
			{
				Name:             "path_normalize",
				Type:             RuleTransform,
				FieldsMatchRegex: `(?i)^(path|file|filename|dir|cwd)$`,
				ReplaceFrom:      `^/Users/[^/]+`,
				ReplaceTo:        "${HOME}",
			},
		},
	}
}

// Standard: like strict but skips email + path normalization. The default.
func StandardProfile() *Policy {
	p := StrictProfile()
	p.Name = "standard"
	out := p.Rules[:0]
	for _, r := range p.Rules {
		if r.Name == "pii_email" {
			continue
		}
		out = append(out, r)
	}
	p.Rules = out
	return p
}

// Permissive: only secrets, no PII, no truncation. Use only on synthetic
// or already-sanitized data.
func PermissiveProfile() *Policy {
	return &Policy{
		Name: "permissive",
		Rules: []Rule{
			{
				Name: "secrets_only",
				Type: RuleRedactPattern,
				Patterns: []string{
					`AKIA[0-9A-Z]{16}`,
					`ghp_[A-Za-z0-9]{36,}`,
					`sk-ant-[A-Za-z0-9_-]{20,}`,
					`whs_(?:live|test)_[A-Za-z0-9]{16,}`,
				},
				Placeholder: "[REDACTED:${pattern_name}]",
			},
		},
	}
}

// ByName returns a built-in profile by name. Returns nil if unknown.
func ByName(name string) *Policy {
	switch name {
	case "strict":
		return StrictProfile()
	case "standard", "":
		return StandardProfile()
	case "permissive":
		return PermissiveProfile()
	}
	return nil
}
