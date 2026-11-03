package integration

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
)

const formPage = `<!doctype html><html><body>
<form id="other" method="post" action="/elsewhere"><input name="x" value="1"></form>
<form id="f" method="POST" action="/notes">
<input type="hidden" name="csrf" value="tok">
<input name="title" value="old"><textarea name="body"> kept </textarea>
<input type="checkbox" name="on" value="yes" checked><input type="checkbox" name="off" value="no">
<input name="gone" value="x" disabled><input type="file" name="upload">
<select name="color"><option value="a">A</option><option value="b" selected>B</option></select>
<select name="size"><option>Small</option><option>Big</option></select>
<button type="submit" name="save" value="1">Save</button>
</form></body></html>`

func mutateFile(t *testing.T, path string, f func(m map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func formOp(mut func(*Operation)) *Operation {
	o := &Operation{Execution: ExecHTTPFormHTML, Request: RequestSpec{
		Method: "POST", Path: "/notes",
		FormPage: &FormPage{Path: "/notes/new", Selector: "form#f"},
		Form:     map[string]string{"title": "{{input.title}}"},
	}}
	if mut != nil {
		mut(o)
	}
	return o
}

func TestFormFieldsSubmitsWhatABrowserWould(t *testing.T) {
	o := formOp(func(o *Operation) { o.Request.Form["body"] = "{{input.absent}}" })
	got, err := o.FormFields("http://a.test", []byte(formPage), "/notes", Vars{Input: map[string]any{"title": "new"}})
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"csrf": {"tok"}, "title": {"new"}, "body": {" kept "}, "on": {"yes"}, "color": {"b"}, "size": {"Small"}}
	if got.Encode() != want.Encode() {
		t.Errorf("fields = %s\nwant     %s", got.Encode(), want.Encode())
	}
}

func TestFormFieldsRefusesDrift(t *testing.T) {
	v := Vars{Input: map[string]any{"title": "new"}}
	cases := []struct {
		name string
		page string
		mut  func(*Operation)
	}{
		{"form is gone", `<html><form id="g"></form></html>`, nil},
		{"selector matches two forms", formPage, func(o *Operation) { o.Request.FormPage.Selector = "form" }},
		{"selector matches something else", `<div id="f"></div>`, nil},
		{"method changed", strings.Replace(formPage, `method="POST" action="/notes"`, `method="get" action="/notes"`, 1), nil},
		{"action changed", strings.Replace(formPage, `action="/notes"`, `action="/v2/notes"`, 1), nil},
		{"action left the origin", strings.Replace(formPage, `action="/notes"`, `action="https://evil.test/notes"`, 1), nil},
		{"multipart", strings.Replace(formPage, `method="POST"`, `method="POST" enctype="multipart/form-data"`, 1), nil},
		{"declared field is gone", strings.Replace(formPage, `name="title"`, `name="headline"`, 1), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := formOp(tc.mut).FormFields("http://a.test", []byte(tc.page), "/notes", v)
			if !errors.Is(err, ErrSourceChanged) {
				t.Errorf("err = %v, want ErrSourceChanged", err)
			}
		})
	}
}

func TestExtractValues(t *testing.T) {
	page := []byte(`<ul><li class="n" data-id="1"><a>First</a> <small>m-1</small></li><li class="n" data-id="2"><a>Second</a> <small>m-2</small><script>no()</script></li></ul>`)
	o := &Operation{Execution: ExecHTTPFormHTML, Response: ResponseSpec{Extract: map[string]Extract{
		"first":   {CSS: "li.n a"},
		"by_ref":  {CSS: "li.n", Attr: "data-id", Text: "{{input.ref}}"},
		"missing": {CSS: "li.n", Attr: "data-id", Text: "m-9"},
		"noattr":  {CSS: "li.n", Attr: "href"},
		"loc":     {LocationRegex: "^/notes/([0-9]+)$"},
	}}}
	got := o.ExtractValues("http://a.test", Answer{Body: page, Location: "/notes/42"}, Vars{Input: map[string]any{"ref": "m-2"}})
	for k, want := range map[string]string{"first": "First", "by_ref": "2", "loc": "42"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for _, k := range []string{"missing", "noattr"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s = %q, want absent", k, got[k])
		}
	}
	off := o.ExtractValues("http://a.test", Answer{Location: "https://evil.test/notes/42"}, Vars{})
	if _, ok := off["loc"]; ok {
		t.Error("a location on another origin was read")
	}
}

func TestExtractWithEmptyAnchorMatchesNothing(t *testing.T) {
	o := &Operation{Response: ResponseSpec{Extract: map[string]Extract{"id": {CSS: "li", Attr: "data-id", Text: "{{input.ref}}"}}}}
	got := o.ExtractValues("http://a.test", Answer{Body: []byte(`<li data-id="1">x</li>`)}, Vars{Input: map[string]any{"ref": ""}})
	if len(got) != 0 {
		t.Errorf("an empty marker matched: %v", got)
	}
}

func TestRedirectSucceeded(t *testing.T) {
	o := &Operation{Response: ResponseSpec{RedirectSuccess: "^/notes/[0-9]+$"}}
	for loc, want := range map[string]bool{
		"/notes/7":                  true,
		"http://a.test/notes/7":     true,
		"/login":                    false,
		"/login?next=/notes/7":      false,
		"https://evil.test/notes/7": false,
		"//evil.test/notes/7":       false,
		"":                          false,
	} {
		if got := o.RedirectSucceeded("http://a.test", Answer{Location: loc}); got != want {
			t.Errorf("%q = %v, want %v", loc, got, want)
		}
	}
}

func TestTypedOutput(t *testing.T) {
	o := &Operation{OutputSchema: []byte(`{"properties":{"n":{"type":"integer"},"m":{"type":["integer","null"]},"s":{"type":"string"},"b":{"type":"boolean"},"bad":{"type":"integer"}}}`)}
	got := o.TypedOutput(map[string]string{"n": "7", "m": "8", "s": "9", "b": "true", "bad": "x", "free": "y"})
	if got["n"] != 7.0 || got["m"] != 8.0 || got["s"] != "9" || got["b"] != true || got["bad"] != "x" || got["free"] != "y" {
		t.Errorf("got %#v", got)
	}
}

func TestParseFormFixtureAndRefusals(t *testing.T) {
	if _, err := LoadFile("testdata/notes-form.json"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(o map[string]any)
		want string
	}{
		{"unknown execution", func(o map[string]any) { o["execution"] = "http.magic" }, "execution"},
		{"write without identifier", func(o map[string]any) { delete(o["response"].(map[string]any), "identifier") }, "identifier"},
		{"unanchored redirect", func(o map[string]any) { o["response"].(map[string]any)["redirect_success"] = "/notes/" }, "must start with ^"},
		{"two capture groups", func(o map[string]any) {
			o["response"].(map[string]any)["extract"] = map[string]any{"id": map[string]any{"location_regex": "^/(a)/(b)$"}}
		}, "one capture group"},
		{"bad css", func(o map[string]any) {
			o["response"].(map[string]any)["extract"] = map[string]any{"id": map[string]any{"css": "li["}}
		}, "css"},
		{"output map instead of extract", func(o map[string]any) { o["response"].(map[string]any)["output"] = map[string]any{"id": "/id"} }, "not response.output"},
		{"body instead of form", func(o map[string]any) { o["request"].(map[string]any)["body"] = map[string]any{"a": 1} }, "not request.body"},
		{"credential in a form page path", func(o map[string]any) {
			o["request"].(map[string]any)["form_page"] = map[string]any{"path": "/{{credential.password}}", "selector": "form"}
		}, "not allowed"},
		{"form_page without the kind", func(o map[string]any) { delete(o, "execution") }, "need http.form_html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := mutateFile(t, "testdata/notes-form.json", func(m map[string]any) { tc.edit(op(m, 2)) })
			_, err := Parse(b)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
