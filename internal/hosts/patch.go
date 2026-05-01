package hosts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/webhookscout/scouttrace/internal/version"
)

// patchBytes wraps the listed servers under serversKey with a scouttrace
// proxy shim. Servers not listed are left alone. Already-managed servers
// are skipped (idempotent).
func patchBytes(in []byte, serversKey string, servers []string, proxyExe string) ([]byte, error) {
	tree, indent, trailingNL, err := loadJSON(in)
	if err != nil {
		return nil, err
	}
	srvAny, ok := tree[serversKey]
	if !ok {
		return nil, fmt.Errorf("hosts: %q key absent", serversKey)
	}
	srvMap, ok := srvAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hosts: %q is not an object", serversKey)
	}
	want := map[string]struct{}{}
	for _, s := range servers {
		want[s] = struct{}{}
	}
	for name, raw := range srvMap {
		if len(want) > 0 {
			if _, ok := want[name]; !ok {
				continue
			}
		}
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if alreadyManaged(entry) {
			continue
		}
		patched, err := patchEntry(entry, name, proxyExe)
		if err != nil {
			return nil, err
		}
		srvMap[name] = patched
	}
	tree[serversKey] = srvMap
	return saveJSON(tree, indent, trailingNL)
}

func unpatchBytes(in []byte, serversKey string) ([]byte, []string, error) {
	tree, indent, trailingNL, err := loadJSON(in)
	if err != nil {
		return nil, nil, err
	}
	srvAny, ok := tree[serversKey]
	if !ok {
		return in, nil, nil
	}
	srvMap, ok := srvAny.(map[string]any)
	if !ok {
		return nil, nil, errors.New("hosts: servers key not an object")
	}
	var restored []string
	for name, raw := range srvMap {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		marker, ok := entry["_scouttrace"].(map[string]any)
		if !ok {
			continue
		}
		orig, ok := marker["original"].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("E_HOST_MARKER_MISSING: server %q has _scouttrace but no original", name)
		}
		// Build a fresh map: start with the patched entry's other-than-managed
		// fields, then overlay the canonical original entries so they win.
		out := map[string]any{}
		for k, v := range entry {
			if k == "command" || k == "args" || k == "_scouttrace" {
				continue
			}
			out[k] = v
		}
		for k, v := range orig {
			out[k] = v
		}
		srvMap[name] = out
		restored = append(restored, name)
	}
	tree[serversKey] = srvMap
	out, err := saveJSON(tree, indent, trailingNL)
	if err != nil {
		return nil, nil, err
	}
	return out, restored, nil
}

func alreadyManaged(entry map[string]any) bool {
	mk, ok := entry["_scouttrace"].(map[string]any)
	if !ok {
		return false
	}
	managed, _ := mk["managed"].(bool)
	return managed
}

func patchEntry(entry map[string]any, name, proxyExe string) (map[string]any, error) {
	origCmd, _ := entry["command"].(string)
	origArgsRaw, _ := entry["args"].([]any)
	origArgs := make([]string, 0, len(origArgsRaw))
	for _, a := range origArgsRaw {
		s, ok := a.(string)
		if !ok {
			return nil, fmt.Errorf("hosts: server %q args contain non-string", name)
		}
		origArgs = append(origArgs, s)
	}

	newArgs := []any{"proxy", "--server-name", name, "--", origCmd}
	for _, a := range origArgs {
		newArgs = append(newArgs, a)
	}

	out := map[string]any{}
	for k, v := range entry {
		out[k] = v
	}
	out["command"] = proxyExe
	out["args"] = newArgs
	out["_scouttrace"] = map[string]any{
		"managed": true,
		"version": version.Version,
		"original": map[string]any{
			"command": origCmd,
			"args":    sliceToAny(origArgs),
		},
	}
	if env, ok := entry["env"]; ok {
		out["_scouttrace"].(map[string]any)["original"].(map[string]any)["env"] = env
	}
	return out, nil
}

func sliceToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// loadJSON parses raw config preserving indentation hints.
func loadJSON(b []byte) (map[string]any, string, bool, error) {
	var tree map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, "", false, err
	}
	indent := detectIndent(b)
	trailingNL := bytes.HasSuffix(b, []byte("\n"))
	return tree, indent, trailingNL, nil
}

func saveJSON(tree map[string]any, indent string, trailingNL bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", indent)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	// json.Encoder always appends a single newline. Adjust to match input.
	if !trailingNL && bytes.HasSuffix(out, []byte("\n")) {
		out = out[:len(out)-1]
	}
	return out, nil
}

// detectIndent returns the indent string used in the original document, or
// "  " (two spaces) as a default.
func detectIndent(b []byte) string {
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			j := i + 1
			n := 0
			for j < len(b) && b[j] == ' ' {
				n++
				j++
			}
			if n > 0 {
				return string(b[i+1 : i+1+n])
			}
			n = 0
			for j < len(b) && b[j] == '\t' {
				n++
				j++
			}
			if n > 0 {
				return string(b[i+1 : i+1+n])
			}
		}
	}
	return "  "
}
