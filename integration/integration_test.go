package integration

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/notes.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseFixture(t *testing.T) {
	c, err := Parse(fixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.HasPrefix(c.Hash, "sha256:") {
		t.Errorf("Hash = %q", c.Hash)
	}
	op, ok := c.Op("create_note")
	if !ok || op.Effect != EffectWrite || op.Input == nil || op.Output == nil {
		t.Fatalf("create_note not compiled: %+v", op)
	}
	if err := op.Input.Validate(map[string]any{"title": ""}); err == nil {
		t.Error("empty title passed input validation")
	}
	if err := op.Input.Validate(map[string]any{"title": "x", "extra": 1}); err == nil {
		t.Error("additional property passed input validation")
	}
}

// mutate decodes the fixture, applies f, and re-encodes it.
func mutate(t *testing.T, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ops(m map[string]any) []any { return m["operations"].([]any) }
func op(m map[string]any, i int) map[string]any {
	return ops(m)[i].(map[string]any)
}

func TestParseRefusesInvalidArtifacts(t *testing.T) {
	cases := []struct {
		name string
		f    func(m map[string]any)
		want string
	}{
		{"unknown field", func(m map[string]any) { m["surprise"] = true }, "unknown field"},
		{"wrong format", func(m map[string]any) { m["format"] = "other/v1" }, "format must be"},
		{"bad origin path", func(m map[string]any) { m["source"].(map[string]any)["origin"] = "https://a.example/x" }, "source.origin"},
		{"plain http off loopback", func(m map[string]any) { m["source"].(map[string]any)["origin"] = "http://a.example" }, "must use https"},
		{"write without reconcile", func(m map[string]any) { delete(op(m, 2), "reconcile") }, "must declare reconcile"},
		{"read with POST", func(m map[string]any) { op(m, 0)["request"].(map[string]any)["method"] = "POST" }, "needs reviewed_safe_method"},
		{"write without destructive", func(m map[string]any) { delete(op(m, 2), "destructive") }, "must declare destructive"},
		{"destructive read", func(m map[string]any) { op(m, 0)["destructive"] = true }, "cannot be destructive"},
		{"safe method on write", func(m map[string]any) { op(m, 2)["reviewed_safe_method"] = true }, "applies only to reads"},
		{"browser without signed_in", func(m map[string]any) {
			sess := m["session"].(map[string]any)
			sess["provider"] = "browser"
			delete(sess, "sign_in")
			delete(sess, "signed_in")
		}, "needs a signed_in check"},
		{"bad schema", func(m map[string]any) { op(m, 0)["input_schema"] = map[string]any{"type": 7} }, "input_schema"},
		{"input not object", func(m map[string]any) { op(m, 0)["input_schema"] = map[string]any{"type": "string"} }, `must have type "object"`},
		{"undeclared input", func(m map[string]any) {
			op(m, 0)["request"].(map[string]any)["path"] = "/api/notes/{{input.nope}}"
		}, "{{input.nope}} is not a property"},
		{"credential in op", func(m map[string]any) {
			op(m, 0)["request"].(map[string]any)["path"] = "/api/notes/{{credential.password}}"
		}, "is not allowed here"},
		{"absolute path", func(m map[string]any) { op(m, 0)["request"].(map[string]any)["path"] = "https://evil.example/x" }, "origin-relative"},
		{"cookie header", func(m map[string]any) {
			op(m, 0)["request"].(map[string]any)["headers"] = map[string]any{"Cookie": "a=b"}
		}, "managed by the runtime"},
		{"marker missing", func(m map[string]any) {
			body := op(m, 2)["request"].(map[string]any)["body"].(map[string]any)["note"].(map[string]any)
			delete(body, "ref")
		}, "needs {{receipt.marker}}"},
		{"reconcile with write", func(m map[string]any) {
			op(m, 2)["reconcile"].(map[string]any)["operation"] = "create_note"
		}, "must be a read"},
		{"reserved input", func(m map[string]any) {
			op(m, 0)["input_schema"].(map[string]any)["properties"].(map[string]any)["idempotency_key"] = map[string]any{"type": "string"}
		}, "reserved"},
		{"duplicate op", func(m map[string]any) { m["operations"] = append(ops(m), ops(m)[0]) }, "duplicate name"},
		{"unextracted session header", func(m map[string]any) {
			m["session"].(map[string]any)["request_headers"] = map[string]any{"X-T": "{{session.nope}}"}
		}, "is not extracted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(mutate(t, tc.f))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestBuildRequestRendersAndOmitsAbsent(t *testing.T) {
	c, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Op("create_note")
	v := Vars{Input: map[string]any{"title": "a/b \"q\""}, Receipt: map[string]string{"marker": "dj-1"}}
	req, err := BuildRequest("http://127.0.0.1:9", op.Request, v)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(req.Body)
	var got map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["note"]["title"] != "a/b \"q\"" || got["note"]["ref"] != "dj-1" {
		t.Errorf("body = %s", b)
	}
	if _, present := got["note"]["body"]; present {
		t.Errorf("absent input rendered: %s", b)
	}

	get, _ := c.Op("get_note")
	req, err = BuildRequest("http://127.0.0.1:9", get.Request, Vars{Input: map[string]any{"id": "1/../admin"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.EscapedPath() != "/api/notes/1%2F..%2Fadmin" {
		t.Errorf("path not escaped: %s", req.URL.EscapedPath())
	}
	_, err = BuildRequest("http://127.0.0.1:9", get.Request, Vars{Input: map[string]any{}})
	if !errors.Is(err, ErrAbsent) {
		t.Errorf("missing path input err = %v", err)
	}
}

func TestLookup(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"a":{"b/c":[1,{"d":null}]}}`), &doc)
	if v, ok := Lookup(doc, "/a/b~1c/0"); !ok || v.(float64) != 1 {
		t.Errorf("got %v %v", v, ok)
	}
	if v, ok := Lookup(doc, "/a/b~1c/1/d"); !ok || v != nil {
		t.Errorf("null lookup got %v %v", v, ok)
	}
	if _, ok := Lookup(doc, "/a/x"); ok {
		t.Error("missing path found")
	}
}

func TestParseAcceptsReviewedPostRead(t *testing.T) {
	b := mutate(t, func(m map[string]any) {
		o := op(m, 1)
		o["request"].(map[string]any)["method"] = "POST"
		delete(o["request"].(map[string]any), "query")
		o["request"].(map[string]any)["body"] = map[string]any{"ref": "{{input.ref}}"}
		o["reviewed_safe_method"] = true
	})
	if _, err := Parse(b); err != nil {
		t.Fatalf("reviewed POST read refused: %v", err)
	}
}
