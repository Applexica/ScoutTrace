package redact

import (
	"strconv"
	"strings"
)

// Path syntax: dot-separated keys. `*` matches any direct child key (or
// any array index). Examples:
//   request.args.password
//   request.args.*
//   response.result.items.*.token

// walkStrings calls fn on every string leaf reachable from *root, with the
// dotted path of the leaf. If fn returns (newValue, true), the leaf is
// replaced and the count is incremented.
func walkStrings(root *any, prefix string, fn func(path string, s string) (string, bool)) {
	walk(root, prefix, fn)
}

func walk(p *any, prefix string, fn func(path string, s string) (string, bool)) {
	switch v := (*p).(type) {
	case string:
		if newV, ok := fn(prefix, v); ok {
			*p = newV
		}
	case map[string]any:
		for k := range v {
			child := v[k]
			path := joinPath(prefix, k)
			walk(&child, path, fn)
			v[k] = child
		}
	case []any:
		for i := range v {
			path := joinPath(prefix, strconv.Itoa(i))
			child := v[i]
			walk(&child, path, fn)
			v[i] = child
		}
	}
}

func joinPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// resolvePathMutable returns the value at exact path along with parent
// references so callers can mutate it. Glob `*` is not supported here;
// see resolvePathGlob for glob-walking.
//
// On match returns:
//
//	target value, parent map (or nil), key string (for map),
//	index (for slice), ok=true
func resolvePathMutable(root any, path string) (any, map[string]any, string, int, bool) {
	segs := splitPath(path)
	cur := root
	var parentMap map[string]any
	var parentKey string
	parentIdx := -1
	for _, s := range segs {
		switch v := cur.(type) {
		case map[string]any:
			child, ok := v[s]
			if !ok {
				return nil, nil, "", -1, false
			}
			parentMap = v
			parentKey = s
			parentIdx = -1
			cur = child
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(v) {
				return nil, nil, "", -1, false
			}
			parentMap = nil
			parentKey = ""
			parentIdx = i
			cur = v[i]
		default:
			return nil, nil, "", -1, false
		}
	}
	return cur, parentMap, parentKey, parentIdx, true
}

func setMutable(parent map[string]any, key string, idx int, val any) {
	if parent != nil {
		parent[key] = val
		return
	}
	_ = idx // arrays are addressed inline by walk
}

// dropPath removes the value at path. Supports `*` glob in non-final
// position (treated as "all map keys" / "all slice indexes").
func dropPath(root *any, path string) bool {
	segs := splitPath(path)
	return dropAt(root, segs)
}

func dropAt(p *any, segs []string) bool {
	if len(segs) == 0 {
		return false
	}
	first := segs[0]
	rest := segs[1:]
	switch v := (*p).(type) {
	case map[string]any:
		if first == "*" {
			any := false
			if len(rest) == 0 {
				for k := range v {
					delete(v, k)
					any = true
				}
				return any
			}
			for k := range v {
				child := v[k]
				if dropAt(&child, rest) {
					any = true
				}
				v[k] = child
			}
			return any
		}
		child, ok := v[first]
		if !ok {
			return false
		}
		if len(rest) == 0 {
			delete(v, first)
			return true
		}
		if dropAt(&child, rest) {
			v[first] = child
			return true
		}
		v[first] = child
	case []any:
		if first == "*" {
			any := false
			for i := range v {
				child := v[i]
				if dropAt(&child, rest) {
					any = true
				}
				v[i] = child
			}
			return any
		}
		i, err := strconv.Atoi(first)
		if err != nil || i < 0 || i >= len(v) {
			return false
		}
		if len(rest) == 0 {
			// Replace element with nil (we cannot resize cleanly without parent).
			v[i] = nil
			return true
		}
		child := v[i]
		if dropAt(&child, rest) {
			v[i] = child
			return true
		}
	}
	return false
}

func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, ".")
}
