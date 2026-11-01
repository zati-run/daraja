package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Template roots. A template is {{root.name}}; there are no other
// expressions.
var templateRE = regexp.MustCompile(`\{\{\s*([a-z]+)\.([A-Za-z0-9_]+)\s*\}\}`)

var knownRoots = map[string]bool{"input": true, "credential": true, "session": true, "extracted": true, "receipt": true}

type ref struct{ root, name string }

func templateRefs(s string) []ref {
	var out []ref
	for _, m := range templateRE.FindAllStringSubmatch(s, -1) {
		out = append(out, ref{m[1], m[2]})
	}
	return out
}

func checkTemplate(where, s string, allowed map[string]bool, add func(string, ...any)) {
	rest := templateRE.ReplaceAllString(s, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		add("%s: malformed template in %q", where, s)
	}
	for _, r := range templateRefs(s) {
		if !knownRoots[r.root] {
			add("%s: unknown template root %q", where, r.root)
		} else if !allowed[r.root] {
			add("%s: {{%s.%s}} is not allowed here", where, r.root, r.name)
		}
	}
}

// Vars are the values available to templates.
type Vars struct {
	Input      map[string]any
	Credential map[string]string
	Session    map[string]string
	Extracted  map[string]string
	Receipt    map[string]string
}

// ErrAbsent reports a template that names an input the caller omitted.
var ErrAbsent = errors.New("absent input")

func (v Vars) lookup(r ref) (any, error) {
	var (
		val any
		ok  bool
	)
	switch r.root {
	case "input":
		val, ok = v.Input[r.name]
	case "credential":
		val, ok = v.Credential[r.name]
	case "session":
		val, ok = v.Session[r.name]
	case "extracted":
		val, ok = v.Extracted[r.name]
	case "receipt":
		val, ok = v.Receipt[r.name]
	default:
		return nil, fmt.Errorf("unknown template root %q", r.root)
	}
	if !ok || val == nil {
		if r.root == "input" {
			return nil, fmt.Errorf("%w %q", ErrAbsent, r.name)
		}
		return nil, fmt.Errorf("value {{%s.%s}} is not available", r.root, r.name)
	}
	return val, nil
}

func scalarString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case json.Number:
		return t.String(), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	default:
		return "", fmt.Errorf("value of type %T cannot be placed in a string", v)
	}
}

// RenderString interpolates a template. escape is applied to each value.
func (v Vars) RenderString(s string, escape func(string) string) (string, error) {
	var firstErr error
	out := templateRE.ReplaceAllStringFunc(s, func(m string) string {
		sm := templateRE.FindStringSubmatch(m)
		val, err := v.lookup(ref{sm[1], sm[2]})
		if err == nil {
			var str string
			str, err = scalarString(val)
			if err == nil {
				if escape != nil {
					str = escape(str)
				}
				return str
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		return ""
	})
	return out, firstErr
}

// RenderJSON renders a JSON body template. A string leaf that is exactly
// one template takes the value's JSON type; if that value is an absent
// input, the containing key or array element is omitted.
func (v Vars) RenderJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	out, keep, err := v.renderNode(tree)
	if err != nil {
		return nil, err
	}
	if !keep {
		out = map[string]any{}
	}
	return json.Marshal(out)
}

func (v Vars) renderNode(n any) (any, bool, error) {
	switch t := n.(type) {
	case string:
		if m := templateRE.FindStringSubmatch(t); m != nil && m[0] == strings.TrimSpace(t) {
			val, err := v.lookup(ref{m[1], m[2]})
			if errors.Is(err, ErrAbsent) {
				return nil, false, nil
			}
			return val, err == nil, err
		}
		s, err := v.RenderString(t, nil)
		if errors.Is(err, ErrAbsent) {
			return nil, false, nil
		}
		return s, err == nil, err
	case map[string]any:
		m := map[string]any{}
		for k, x := range t {
			r, keep, err := v.renderNode(x)
			if err != nil {
				return nil, false, err
			}
			if keep {
				m[k] = r
			}
		}
		return m, true, nil
	case []any:
		var a []any
		for _, x := range t {
			r, keep, err := v.renderNode(x)
			if err != nil {
				return nil, false, err
			}
			if keep {
				a = append(a, r)
			}
		}
		if a == nil {
			a = []any{}
		}
		return a, true, nil
	default:
		return t, true, nil
	}
}

// BuildRequest renders an operation's request against an origin.
func BuildRequest(origin string, spec RequestSpec, v Vars) (*http.Request, error) {
	path, err := v.RenderString(spec.Path, url.PathEscape)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(origin + path)
	if err != nil {
		return nil, fmt.Errorf("rendered path: %w", err)
	}
	if spec.Query != nil {
		q := u.Query()
		for k, t := range spec.Query {
			s, err := v.RenderString(t, nil)
			if errors.Is(err, ErrAbsent) {
				continue
			}
			if err != nil {
				return nil, err
			}
			q.Set(k, s)
		}
		u.RawQuery = q.Encode()
	}
	var body io.Reader
	contentType := ""
	switch {
	case len(spec.Body) > 0:
		b, err := v.RenderJSON(spec.Body)
		if err != nil {
			return nil, err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	case len(spec.Form) > 0:
		f := url.Values{}
		for k, t := range spec.Form {
			s, err := v.RenderString(t, nil)
			if errors.Is(err, ErrAbsent) {
				continue
			}
			if err != nil {
				return nil, err
			}
			f.Set(k, s)
		}
		body, contentType = strings.NewReader(f.Encode()), "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequest(spec.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	for k, t := range spec.Headers {
		s, err := v.RenderString(t, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set(k, s)
	}
	return req, nil
}

// validPointer reports whether p is an RFC 6901 JSON Pointer.
func validPointer(p string) bool {
	if p == "" {
		return true
	}
	if !strings.HasPrefix(p, "/") {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '~' && (i+1 >= len(p) || (p[i+1] != '0' && p[i+1] != '1')) {
			return false
		}
	}
	return true
}

// Lookup resolves a JSON Pointer in a decoded JSON document. ok is false
// when the path does not exist.
func Lookup(doc any, p string) (any, bool) {
	if p == "" {
		return doc, true
	}
	cur := doc
	for _, tok := range strings.Split(p[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// MapOutput selects mapped fields from a response document. Missing
// pointers leave the field out; schema validation decides if that is valid.
func MapOutput(doc any, mapping map[string]string) map[string]any {
	out := map[string]any{}
	for k, p := range mapping {
		if v, ok := Lookup(doc, p); ok {
			out[k] = v
		}
	}
	return out
}
