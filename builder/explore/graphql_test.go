package explore

import "testing"

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := graphQLReadOnly([]byte(tc.body)); got != tc.want {
				t.Errorf("graphQLReadOnly(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
