package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/journal"
)

const refusedArtifact = `{
  "format": "daraja.integration/v1alpha1", "id": "refusals", "version": "1",
  "source": {"kind": "web", "origin": "ORIGIN"},
  "session": {"provider": "none"},
  "operations": [
    {"name": "lookup", "description": "Read.", "effect": "read", "reviewed_safe_method": true,
     "input_schema": {"type": "object"}, "output_schema": {"type": "object"},
     "request": {"method": "POST", "path": "/gql", "body": {"query": "{ a }"}},
     "response": {"success_status": [200], "output": {"id": "/data/a/id"},
       "refused_when": [{"json_pointer": "/data/a/__typename", "equals": "Failure"}]}},
    {"name": "update", "description": "Write.", "effect": "write", "destructive": false,
     "input_schema": {"type": "object"}, "output_schema": {"type": "object"},
     "request": {"method": "POST", "path": "/gql", "body": {"query": "mutation { a }", "ref": "{{receipt.marker}}"}},
     "response": {"success_status": [200], "output": {"id": "/data/a/id"}, "identifier": "/data/a/id",
       "refused_when": [{"json_pointer": "/data/a/__typename", "equals": "Failure"}, {"json_pointer": "/data/ok", "equals": "false"}, {"json_pointer": "/errors/0/detail", "contains": "NOT_FOUND"}, {"json_pointer": "/data/code", "equals": "9007199254740992"}, {"json_pointer": "/data/flag", "contains": "false"}]},
     "reconcile": {"strategy": "marker", "operation": "lookup", "found": "/id", "settle_seconds": 3600}}
  ]}`

func refusalRuntime(t *testing.T, answer string) (*Runtime, *Principal) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	in, err := integration.Parse([]byte(strings.Replace(refusedArtifact, "ORIGIN", srv.URL, 1)))
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "j.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	p := Principal{ID: "p", TokenSHA256: HashToken("t"), Grants: []Grant{{Integration: "refusals", Operations: []string{"lookup", "update"}}}}
	r, err := New(Config{Integrations: []*integration.Compiled{in}, Principals: []Principal{p}, Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	return r, &r.principals[0]
}

func TestWriteClassifiesDeclaredFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, answer, state, code string
	}{
		{"declared failure", `{"data":{"a":{"__typename":"Failure"}}}`, journal.Failed, CodeSourceRefused},
		{"second declared failure, non-string scalar", `{"data":{"ok":false}}`, journal.Failed, CodeSourceRefused},
		{"failure code nested in error text", `{"errors":[{"detail":"{\"code\":\"NOT_FOUND\"}"}],"data":null}`, journal.Failed, CodeSourceRefused},
		{"other error text stays uncertain", `{"errors":[{"detail":"INTERNAL_SERVER_ERROR"}],"data":null}`, journal.Uncertain, CodeOutcomeUncertain},
		{"failure and identifier contradict", `{"data":{"a":{"__typename":"Failure","id":7}}}`, journal.Uncertain, CodeOutcomeUncertain},
		{"undeclared failure stays uncertain", `{"data":{"a":{"__typename":"OtherFailure"}}}`, journal.Uncertain, CodeOutcomeUncertain},
		{"declared number as sent", `{"data":{"code":9007199254740992}}`, journal.Failed, CodeSourceRefused},
		{"number that would round into the declared one stays uncertain", `{"data":{"code":9007199254740993}}`, journal.Uncertain, CodeOutcomeUncertain},
		{"contains ignores a non-string", `{"data":{"flag":false}}`, journal.Uncertain, CodeOutcomeUncertain},
		{"trailing text stays uncertain", `{"data":{"ok":false}} trailing`, journal.Uncertain, CodeOutcomeUncertain},
		{"not JSON stays uncertain", `<html>sign in</html>`, journal.Uncertain, CodeOutcomeUncertain},
		{"success", `{"data":{"a":{"__typename":"Ok","id":7}}}`, journal.Succeeded, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, p := refusalRuntime(t, tc.answer)
			res, e := r.Invoke(context.Background(), p, "refusals", "update", nil, "key")
			var rec *journal.Receipt
			if e != nil {
				rec = e.Receipt
				if e.Code != tc.code {
					t.Errorf("code = %s, want %s", e.Code, tc.code)
				}
			} else {
				rec = res.Receipt
			}
			if rec == nil || rec.State != tc.state {
				t.Fatalf("receipt = %+v, want state %s", rec, tc.state)
			}
			// A repeated key answers from the receipt, the same way.
			_, e2 := r.Invoke(context.Background(), p, "refusals", "update", nil, "key")
			if (e == nil) != (e2 == nil) {
				t.Errorf("replay error = %v, first error = %v", e2, e)
			}
		})
	}
}

func TestReadRefusesDeclaredFailure(t *testing.T) {
	t.Parallel()
	r, p := refusalRuntime(t, `{"data":{"a":{"__typename":"Failure"}}}`)
	_, e := r.Invoke(context.Background(), p, "refusals", "lookup", nil, "")
	if e == nil || e.Code != CodeSourceRefused {
		b, _ := json.Marshal(e)
		t.Fatalf("error = %s, want %s", b, CodeSourceRefused)
	}
}

func TestParseRefusesBadRefusal(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{`{"json_pointer":"","equals":"x"}`, `{"json_pointer":"no-slash","equals":"x"}`, `{"json_pointer":"/a"}`, `{"json_pointer":"/a","equals":"x","contains":"y"}`} {
		art := strings.Replace(refusedArtifact, "ORIGIN", "http://127.0.0.1:1", 1)
		art = strings.Replace(art, `{"json_pointer": "/data/a/__typename", "equals": "Failure"}`, bad, 1)
		if _, err := integration.Parse([]byte(art)); err == nil || !strings.Contains(err.Error(), "refused_when") {
			t.Errorf("Parse with %s: err = %v", bad, err)
		}
	}
}
