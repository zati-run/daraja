package explore

import (
	"bytes"
	"encoding/json"
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
		var v struct {
			OperationName *string `json:"operationName"`
			Query         *string `json:"query"`
		}
		if err := json.Unmarshal(r, &v); err != nil {
			return nil, false
		}
		g := gqlRequest{}
		if v.OperationName != nil {
			g.Name = *v.OperationName
		}
		if v.Query != nil {
			g.Query = *v.Query
		}
		out = append(out, g)
	}
	return out, true
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
