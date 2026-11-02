package explore

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

func TestGraphQLReadOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"named query", `{"operationName":"Q","query":"query Q($id: ID!) { ticket(id: $id) { id } }","variables":{"id":"1"}}`, true},
		{"shorthand query", `{"query":"{ me { id } }"}`, true},
		{"query with fragment", `{"query":"query Q { me { ...F } } fragment F on User { id }"}`, true},
		{"object default in variables", `{"query":"query Q($f: Filter = {a: 1, b: {c: 2}}) { list(filter: $f) { id } }"}`, true},
		{"keyword inside a string", `{"query":"query Q { search(q: \"mutation { x }\") { id } }"}`, true},
		{"keyword inside a comment", "{\"query\":\"# mutation M { x }\\nquery Q { me { id } }\"}", true},
		{"batch of queries", `[{"query":"query A { a }"},{"query":"{ b }"}]`, true},
		{"mutation", `{"query":"mutation M { createTicket { id } }"}`, false},
		{"subscription", `{"query":"subscription S { events { id } }"}`, false},
		{"query then mutation", `{"query":"query Q { a } mutation M { b }"}`, false},
		{"batch with a mutation", `[{"query":"query A { a }"},{"query":"mutation B { b }"}]`, false},
		{"leading comment hides nothing", "{\"query\":\"# query\\nmutation M { b }\"}", false},
		{"block string then mutation", `{"query":"query Q { a(d: \"\"\"x\"\"\") } mutation M { b }"}`, false},
		{"persisted query by hash only", `{"operationName":"Q","extensions":{"persistedQuery":{"version":1,"sha256Hash":"ab"}}}`, false},
		{"empty query", `{"query":""}`, false},
		{"unterminated string", `{"query":"query Q { a(s: \"x) }"}`, false},
		{"unbalanced braces", `{"query":"query Q { a "}`, false},
		{"schema definition", `{"query":"type T { a: Int }"}`, false},
		{"not json", `query=query%20Q`, false},
		{"empty batch", `[]`, false},
		{"case-variant key hides a mutation", `{"query":"mutation M { del }","Query":"{ a }"}`, false},
		{"case-variant key alone", `{"Query":"{ a }"}`, false},
		{"duplicate query key", `{"query":"{ a }","query":"mutation M { del }"}`, false},
		{"duplicate operationName", `{"operationName":"A","operationName":"B","query":"query A { a }"}`, false},
		{"trailing data", `{"query":"{ a }"} {"query":"mutation M { b }"}`, false},
		{"null operationName", `{"operationName":null,"query":"{ a }"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := graphQLReadOnly([]byte(tc.body)); got != tc.want {
				t.Errorf("graphQLReadOnly(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestRedactLiterals(t *testing.T) {
	cases := []struct{ in, want string }{
		{`query Q($id: ID!) { ticket(id: $id) { id } }`, `query Q($id: ID!) { ticket(id: $id) { id } }`},
		{`query Q { customer(token: "SECRET", n: 42, f: -1.5e3) { id } }`, `query Q { customer(token: "", n: 0, f: 0) { id } }`},
		{`query Q { a(d: """multi\nline SECRET""") }`, `query Q { a(d: "") }`},
		{`query Q { a(s: "esc \" SECRET") }`, `query Q { a(s: "") }`},
		{"# SECRET comment\nquery Q { field2 }", "\nquery Q { field2 }"},
	}
	for _, tc := range cases {
		if got := redactLiterals(tc.in); got != tc.want {
			t.Errorf("redactLiterals(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadSafeGraphQLGet(t *testing.T) {
	task := &Task{GraphQLReadPaths: []string{"/gql"}}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://a.test/gql?query=%7Ba%7D", true},
		{"https://a.test/gql?query=%7Ba%7D&query=mutation%20M%7Bb%7D", false},
		{"https://a.test/gql?query=%7Ba%7D&Query=mutation%20M%7Bb%7D", false},
		{"https://a.test/gql?query=mutation%20M%7Bb%7D", false},
		{"https://a.test/gql", false},
	}
	for _, tc := range cases {
		r := &network.Request{Method: "GET", URL: tc.url}
		if got := readSafe(task, r); got != tc.want {
			t.Errorf("readSafe(GET %s) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
