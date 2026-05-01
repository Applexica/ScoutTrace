package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// yamlToJSON parses a small YAML subset into JSON bytes. The subset covers:
//   - mappings (`key: value`)
//   - sequences (`- item`)
//   - scalars (string, number, bool, null/~)
//   - quoted strings (single and double)
//   - comments (`# …`) at line end
//   - 2-space indentation
//
// We keep this simple: it is enough for the documented config schema.
// Anything else (anchors, multi-doc, flow style) falls through to a parse
// error so users get a clear message.
func yamlToJSON(b []byte) ([]byte, error) {
	lines := preprocessYAML(b)
	root, _, err := parseBlock(lines, 0, 0)
	if err != nil {
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type yamlLine struct {
	indent int
	text   string // trimmed; comments stripped
	num    int    // 1-based for diagnostics
}

func preprocessYAML(b []byte) []yamlLine {
	raw := strings.Split(string(b), "\n")
	out := make([]yamlLine, 0, len(raw))
	for i, ln := range raw {
		// Strip trailing CR.
		ln = strings.TrimRight(ln, "\r")
		// Compute indent (spaces only; treat tabs as one space).
		indent := 0
		for _, r := range ln {
			if r == ' ' {
				indent++
				continue
			}
			break
		}
		body := ln[indent:]
		// Strip comments — naive: not inside quoted strings.
		body = stripYAMLComment(body)
		body = strings.TrimRight(body, " \t")
		if body == "" {
			continue
		}
		out = append(out, yamlLine{indent: indent, text: body, num: i + 1})
	}
	return out
}

func stripYAMLComment(s string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if inDouble && i+1 < len(s) {
				i++
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return s[:i]
			}
		}
	}
	return s
}

// parseBlock parses lines starting at idx with indent >= baseIndent.
// Returns the constructed node and the index of the first line that no
// longer belongs to this block.
func parseBlock(lines []yamlLine, idx, baseIndent int) (any, int, error) {
	if idx >= len(lines) {
		return nil, idx, nil
	}
	first := lines[idx]
	if first.indent < baseIndent {
		return nil, idx, nil
	}
	if strings.HasPrefix(first.text, "- ") || first.text == "-" {
		return parseSequence(lines, idx, first.indent)
	}
	return parseMapping(lines, idx, first.indent)
}

func parseMapping(lines []yamlLine, idx, indent int) (any, int, error) {
	out := map[string]any{}
	for idx < len(lines) {
		ln := lines[idx]
		if ln.indent < indent {
			return out, idx, nil
		}
		if ln.indent > indent {
			return nil, idx, fmt.Errorf("yaml: line %d: unexpected indent inside mapping", ln.num)
		}
		key, rest, err := splitMapKey(ln.text, ln.num)
		if err != nil {
			return nil, idx, err
		}
		idx++
		rest = strings.TrimSpace(rest)
		if rest == "" {
			// Nested block — peek next line.
			if idx >= len(lines) || lines[idx].indent <= indent {
				out[key] = nil
				continue
			}
			child, next, err := parseBlock(lines, idx, lines[idx].indent)
			if err != nil {
				return nil, idx, err
			}
			out[key] = child
			idx = next
			continue
		}
		// Inline scalar or flow value.
		val, err := parseScalarOrFlow(rest, ln.num)
		if err != nil {
			return nil, idx, err
		}
		out[key] = val
	}
	return out, idx, nil
}

func parseSequence(lines []yamlLine, idx, indent int) (any, int, error) {
	out := []any{}
	for idx < len(lines) {
		ln := lines[idx]
		if ln.indent < indent {
			return out, idx, nil
		}
		if ln.indent > indent {
			return nil, idx, fmt.Errorf("yaml: line %d: bad indent inside sequence", ln.num)
		}
		if !strings.HasPrefix(ln.text, "-") {
			return out, idx, nil
		}
		body := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
		idx++
		if body == "" {
			// Item is a nested block on subsequent more-indented lines.
			if idx >= len(lines) || lines[idx].indent <= indent {
				out = append(out, nil)
				continue
			}
			child, next, err := parseBlock(lines, idx, lines[idx].indent)
			if err != nil {
				return nil, idx, err
			}
			out = append(out, child)
			idx = next
			continue
		}
		// Either a scalar item or a one-line mapping like `- key: value`.
		if i := indexUnquoted(body, ':'); i >= 0 {
			// Mapping item; inflate as a mapping with indent=indent+2.
			synthetic := []yamlLine{{indent: indent + 2, text: body, num: ln.num}}
			child, _, err := parseMapping(synthetic, 0, indent+2)
			if err != nil {
				return nil, idx, err
			}
			// Continue absorbing more lines whose indent == indent+2 into the
			// same mapping item.
			tail := []yamlLine{}
			for idx < len(lines) && lines[idx].indent > indent {
				tail = append(tail, lines[idx])
				idx++
			}
			if len(tail) > 0 {
				combined := append([]yamlLine{{indent: indent + 2, text: body, num: ln.num}}, tail...)
				child, _, err = parseMapping(combined, 0, indent+2)
				if err != nil {
					return nil, idx, err
				}
			}
			out = append(out, child)
			continue
		}
		val, err := parseScalarOrFlow(body, ln.num)
		if err != nil {
			return nil, idx, err
		}
		out = append(out, val)
	}
	return out, idx, nil
}

func splitMapKey(line string, lineNum int) (string, string, error) {
	i := indexUnquoted(line, ':')
	if i < 0 {
		return "", "", fmt.Errorf("yaml: line %d: missing ':' in mapping", lineNum)
	}
	key := strings.TrimSpace(line[:i])
	if (strings.HasPrefix(key, `"`) && strings.HasSuffix(key, `"`)) ||
		(strings.HasPrefix(key, `'`) && strings.HasSuffix(key, `'`)) {
		key = key[1 : len(key)-1]
	}
	return key, line[i+1:], nil
}

func parseScalarOrFlow(s string, lineNum int) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// Flow sequence/mapping: defer to encoding/json which is JSON-shaped.
	if (strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) ||
		(strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("yaml: line %d: %w", lineNum, err)
		}
		return v, nil
	}
	return parseYAMLScalar(s), nil
}

func parseYAMLScalar(s string) any {
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			var v string
			if err := json.Unmarshal([]byte(s), &v); err == nil {
				return v
			}
			return s[1 : len(s)-1]
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		}
	}
	switch s {
	case "null", "~":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// indexUnquoted returns the index of the first occurrence of ch in s that
// is outside any quoted string.
func indexUnquoted(s string, ch byte) int {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if inDouble && i+1 < len(s) {
				i++
				continue
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		}
		if !inSingle && !inDouble && c == ch {
			return i
		}
	}
	return -1
}
