package explore

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Limits on inferred shapes.
const (
	maxDepth = 8
	maxProps = 200
	maxItems = 3
)

// Shape infers a JSON Schema describing the structure of v. It keeps field
// names and types, never values.
func Shape(v any) map[string]any { return shape(v, 0) }

func shape(v any, depth int) map[string]any {
	switch t := v.(type) {
	case nil:
		return map[string]any{"type": "null"}
	case bool:
		return map[string]any{"type": "boolean"}
	case float64:
		if t == float64(int64(t)) {
			return map[string]any{"type": "integer"}
		}
		return map[string]any{"type": "number"}
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return map[string]any{"type": "integer"}
		}
		return map[string]any{"type": "number"}
	case string:
		return map[string]any{"type": "string"}
	case []any:
		s := map[string]any{"type": "array"}
		if depth >= maxDepth {
			return s
		}
		var items map[string]any
		for i, x := range t {
			if i >= maxItems {
				break
			}
			items = Merge(items, shape(x, depth+1))
		}
		if items != nil {
			s["items"] = items
		}
		return s
	case map[string]any:
		s := map[string]any{"type": "object"}
		if depth >= maxDepth {
			return s
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		props := map[string]any{}
		for i, k := range keys {
			if i >= maxProps {
				s["x-truncated"] = true
				break
			}
			props[k] = shape(t[k], depth+1)
		}
		s["properties"] = props
		return s
	}
	return map[string]any{}
}

// Merge combines two inferred shapes.
func Merge(a, b map[string]any) map[string]any {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	ta, tb := types(a), types(b)
	all := union(ta, tb)
	out := map[string]any{}
	if len(all) == 1 {
		out["type"] = all[0]
	} else {
		ts := make([]any, len(all))
		for i, t := range all {
			ts[i] = t
		}
		out["type"] = ts
	}
	if pa, ok := a["properties"].(map[string]any); ok {
		pb, _ := b["properties"].(map[string]any)
		props := map[string]any{}
		for k, v := range pa {
			props[k] = Merge(v.(map[string]any), asMap(pb[k]))
		}
		for k, v := range pb {
			if _, ok := props[k]; !ok {
				props[k] = v
			}
		}
		out["properties"] = props
	} else if pb, ok := b["properties"].(map[string]any); ok {
		out["properties"] = pb
	}
	if ia, ok := a["items"].(map[string]any); ok {
		out["items"] = Merge(ia, asMap(b["items"]))
	} else if ib, ok := b["items"].(map[string]any); ok {
		out["items"] = ib
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func types(s map[string]any) []string {
	switch t := s["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, x.(string))
		}
		return out
	}
	return nil
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	// integer is a subset of number.
	if seen["integer"] && seen["number"] {
		var f []string
		for _, x := range out {
			if x != "integer" {
				f = append(f, x)
			}
		}
		out = f
	}
	sort.Strings(out)
	return out
}

var (
	numericSeg = regexp.MustCompile(`^(\d+)(\.[a-z]{2,5})?$`)
	opaqueSeg  = regexp.MustCompile(`^([0-9a-fA-F]{16,}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(\.[a-z]{2,5})?$`)
)

// PathParam is a path segment that looked like an identifier.
type PathParam struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // integer or string
}

// NormalizePath replaces identifier-like segments with named parameters.
// It is a heuristic: a template may still contain identifiers, and two
// different routes may collapse together.
func NormalizePath(p string) (string, []PathParam) {
	segs := strings.Split(p, "/")
	var params []PathParam
	used := map[string]int{}
	for i, s := range segs {
		var kind, ext string
		if m := numericSeg.FindStringSubmatch(s); m != nil {
			kind, ext = "integer", m[2]
		} else if m := opaqueSeg.FindStringSubmatch(s); m != nil {
			kind, ext = "string", m[2]
		} else {
			continue
		}
		base := "id"
		if i > 0 && segs[i-1] != "" && !strings.HasPrefix(segs[i-1], "{") {
			base = Singular(strings.TrimSuffix(segs[i-1], ".json")) + "_id"
		}
		base = sanitize(base)
		used[base]++
		name := base
		if used[base] > 1 {
			name = base + "_" + string(rune('0'+used[base]))
		}
		params = append(params, PathParam{Name: name, Kind: kind})
		segs[i] = "{" + name + "}" + ext
	}
	return strings.Join(segs, "/"), params
}

var nonIdent = regexp.MustCompile(`[^a-z0-9_]+`)

func sanitize(s string) string {
	s = nonIdent.ReplaceAllString(strings.ToLower(s), "_")
	s = strings.Trim(s, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "p_" + s
	}
	return s
}

// Singular is a small English singularizer for names; it is cosmetic.
func Singular(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 3:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"), strings.HasSuffix(w, "xes"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && len(w) > 1:
		return w[:len(w)-1]
	}
	return w
}
