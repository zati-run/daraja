package explore

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// gqlRequest is one GraphQL operation sent over HTTP.
type gqlRequest struct {
	Name  string // operationName, or "" when the client sent none
	Query string // the document text; app code, not user data
}

// parseGraphQL decodes a GraphQL-over-HTTP body: one request object or a
// batch array. Variable values are ignored; only shapes are recorded.
func parseGraphQL(body []byte) ([]gqlRequest, bool) {
	body = bytes.TrimSpace(body)
	var raw []json.RawMessage
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
			return nil, false
		}
	} else {
		raw = []json.RawMessage{body}
	}
	out := make([]gqlRequest, 0, len(raw))
	for _, r := range raw {
		g, ok := parseGraphQLObject(r)
		if !ok {
			return nil, false
		}
		out = append(out, g)
	}
	return out, true
}

// parseGraphQLObject decodes one request object. It fails closed on any
// ambiguity a server could resolve differently from Go's decoder: a repeated
// key, or a key that differs from "query" or "operationName" only in case
// (encoding/json matches field names case-insensitively, and keeps the last
// duplicate, so {"query":"mutation…","Query":"{a}"} would otherwise be judged
// by the wrong document).
func parseGraphQLObject(r json.RawMessage) (gqlRequest, bool) {
	var g gqlRequest
	dec := json.NewDecoder(bytes.NewReader(r))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return g, false
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return g, false
		}
		key, ok := t.(string)
		if !ok || seen[key] {
			return g, false
		}
		seen[key] = true
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return g, false
		}
		var dst *string
		switch {
		case key == "query":
			dst = &g.Query
		case key == "operationName":
			dst = &g.Name
		case strings.EqualFold(key, "query"), strings.EqualFold(key, "operationName"):
			return g, false
		default:
			continue
		}
		if string(bytes.TrimSpace(val)) == "null" {
			continue
		}
		if err := json.Unmarshal(val, dst); err != nil {
			return g, false
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return g, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return g, false
	}
	return g, true
}

// graphQLReadOnly reports whether body is a GraphQL request in which every
// document defines only queries and fragments. It fails closed: a body that
// does not parse, a persisted query sent by hash only (the document cannot
// be checked), a mutation, a subscription, or any definition it does not
// recognize makes it false.
func graphQLReadOnly(body []byte) bool {
	reqs, ok := parseGraphQL(body)
	if !ok {
		return false
	}
	for _, r := range reqs {
		if !documentReadOnly(r.Query) {
			return false
		}
	}
	return true
}

// documentReadOnly checks the keyword of each top-level definition. Strings,
// block strings and comments are skipped so their text cannot hide or fake a
// keyword.
func documentReadOnly(doc string) bool {
	if strings.TrimSpace(doc) == "" {
		return false
	}
	depth, parens, defs := 0, 0, 0
	atBoundary := true
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == '#':
			for i < len(doc) && doc[i] != '\n' && doc[i] != '\r' {
				i++
			}
		case strings.HasPrefix(doc[i:], `"""`):
			end := strings.Index(doc[i+3:], `"""`)
			for end >= 0 && doc[i+3+end-1] == '\\' { // \""" is an escaped quote run
				next := strings.Index(doc[i+3+end+3:], `"""`)
				if next < 0 {
					return false
				}
				end += 3 + next
			}
			if end < 0 {
				return false
			}
			i += 3 + end + 3
		case c == '"':
			i++
			for i < len(doc) && doc[i] != '"' {
				if doc[i] == '\\' {
					i++
				}
				if i < len(doc) && (doc[i] == '\n' || doc[i] == '\r') {
					return false
				}
				i++
			}
			if i >= len(doc) {
				return false
			}
			i++
		case c == '(':
			parens++
			i++
		case c == ')':
			parens--
			if parens < 0 {
				return false
			}
			i++
		case c == '{' && parens > 0, c == '}' && parens > 0: // an object value in arguments or variable defaults
			i++
		case c == '{':
			if depth == 0 && atBoundary { // shorthand query
				defs++
				atBoundary = false
			}
			depth++
			i++
		case c == '}':
			depth--
			if depth < 0 {
				return false
			}
			if depth == 0 {
				atBoundary = true
			}
			i++
		case isNameStart(c):
			j := i + 1
			for j < len(doc) && isNameChar(doc[j]) {
				j++
			}
			if depth == 0 && atBoundary {
				switch doc[i:j] {
				case "query", "fragment":
					defs++
					atBoundary = false
				default: // mutation, subscription, schema and type definitions, anything else
					return false
				}
			}
			i = j
		default:
			i++
		}
	}
	return depth == 0 && parens == 0 && defs > 0
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// operationKey names the GraphQL operation(s) in a request for grouping
// observations, so one endpoint that multiplexes many operations is recorded
// as one observation per operation.
func operationKey(reqs []gqlRequest) string {
	names := make([]string, len(reqs))
	for i, r := range reqs {
		names[i] = r.Name
		if names[i] == "" {
			names[i] = "anonymous"
		}
	}
	return strings.Join(names, "+")
}

// redactLiterals returns doc with every string, block string and number
// literal replaced by a placeholder and comments removed, so a recorded query
// document keeps its shape (operation, fields, arguments, variables) but not
// values the app inlined, such as a search term or a token.
func redactLiterals(doc string) string {
	var b strings.Builder
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == '#':
			for i < len(doc) && doc[i] != '\n' && doc[i] != '\r' {
				i++
			}
		case strings.HasPrefix(doc[i:], `"""`):
			j := i + 3
			for j < len(doc) && !(strings.HasPrefix(doc[j:], `"""`) && doc[j-1] != '\\') {
				j++
			}
			b.WriteString(`""`)
			i = min(j+3, len(doc))
		case c == '"':
			j := i + 1
			for j < len(doc) && doc[j] != '"' {
				if doc[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteString(`""`)
			i = min(j+1, len(doc))
		case isNameStart(c):
			j := i + 1
			for j < len(doc) && isNameChar(doc[j]) {
				j++
			}
			b.WriteString(doc[i:j])
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(doc) && (isNameChar(doc[j]) || doc[j] == '.' || doc[j] == '+' || doc[j] == '-') {
				j++
			}
			b.WriteByte('0')
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
