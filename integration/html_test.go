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
	got, err := o.FormFields("http://a.test", []byte(formPage), Vars{Input: map[string]any{"title": "new"}})
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
			_, err := formOp(tc.mut).FormFields("http://a.test", []byte(tc.page), v)
			if !errors.Is(err, ErrSourceChanged) {
				t.Errorf("err = %v, want ErrSourceChanged", err)
			}
		})
	}
}

func TestFormFieldsLeakNoPageValues(t *testing.T) {
	v := Vars{Input: map[string]any{"title": "new"}}
	for _, page := range []string{
		strings.Replace(formPage, `method="POST"`, `method="POST" enctype="secret-value"`, 1),
		strings.Replace(formPage, `method="POST"`, `method="secret-value"`, 1),
	} {
		_, err := formOp(nil).FormFields("http://a.test", []byte(page), v)
		if !errors.Is(err, ErrSourceChanged) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("err = %v", err)
		}
	}
}

func TestFormActionResolvesAgainstThePage(t *testing.T) {
	v := Vars{Input: map[string]any{"title": "new", "id": "7"}}
	page := func(form string) []byte { return []byte(form + `<input name="title"></form>`) }
	cases := []struct {
		name, html, formPath, target string
		ok                           bool
	}{
		{"relative action", `<form id="f" method="post" action="create">`, "/notes/new", "/notes/create", true},
		{"relative action isn't the root", `<form id="f" method="post" action="create">`, "/notes/new", "/create", false},
		{"empty action posts to the page", `<form id="f" method="post">`, "/notes/new", "/notes/new", true},
		{"empty action on a templated page", `<form id="f" method="post">`, "/notes/{{input.id}}/edit", "/notes/7/edit", true},
		{"base changes a relative action", `<base href="/v2/"><form id="f" method="post" action="create">`, "/notes/new", "/v2/create", true},
		{"base leaving the origin is drift", `<base href="https://evil.test/"><form id="f" method="post" action="create">`, "/notes/new", "/create", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := formOp(func(o *Operation) { o.Request.FormPage.Path, o.Request.Path = tc.formPath, tc.target })
			_, err := o.FormFields("http://a.test", page(tc.html), v)
			if tc.ok && err != nil {
				t.Errorf("err = %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrSourceChanged) {
				t.Errorf("err = %v, want ErrSourceChanged", err)
			}
		})
	}
}

func TestFormFieldsFollowFormRules(t *testing.T) {
	v := Vars{Input: map[string]any{"title": "new"}}
	wrap := func(inner, after string) []byte {
		return []byte(`<form id="f" method="post" action="/notes"><input name="title">` + inner + `</form>` + after)
	}
	cases := []struct {
		name string
		page []byte
		want string
	}{
		{"disabled fieldset", wrap(`<fieldset disabled><input name="delete" value="1"></fieldset>`, ""), "title=new"},
		{"first legend of a disabled fieldset stays enabled", wrap(`<fieldset disabled><legend><input name="a" value="1"></legend><legend><input name="b" value="2"></legend></fieldset>`, ""), "a=1&title=new"},
		{"control outside, owned by id", wrap("", `<input type="hidden" name="state" value="s" form="f">`), "state=s&title=new"},
		{"control inside, owned by another form", wrap(`<input name="other" value="o" form="g">`, ""), "title=new"},
		{"datalist is never submitted", wrap(`<datalist><input name="d" value="1"></datalist>`, ""), "title=new"},
		{"disabled option is skipped", wrap(`<select name="s"><option value="a" disabled>A</option><option value="b">B</option></select>`, ""), "s=b&title=new"},
		{"selected in a disabled optgroup sends nothing", wrap(`<select name="s"><optgroup disabled><option value="a" selected>A</option></optgroup><option value="b">B</option></select>`, ""), "title=new"},
		{"selected and disabled sends nothing", wrap(`<select name="s"><option value="a" disabled selected>A</option><option value="b">B</option></select>`, ""), "title=new"},
		{"first enabled option is the default", wrap(`<select name="s"><optgroup disabled><option value="a">A</option></optgroup><option value="b">B</option></select>`, ""), "s=b&title=new"},
		{"single select keeps the last selected", wrap(`<select name="s"><option value="a" selected>A</option><option value="b" selected>B</option></select>`, ""), "s=b&title=new"},
		{"list box with nothing selected sends nothing", wrap(`<select name="s" size="3"><option value="a">A</option></select>`, ""), "title=new"},
		{"last checked radio wins", wrap(`<input type="radio" name="mode" value="delete" checked><input type="radio" name="mode" value="save" checked>`, ""), "mode=save&title=new"},
		{"last checked radio disabled sends nothing", wrap(`<input type="radio" name="mode" value="a" checked><input type="radio" name="mode" value="b" checked disabled>`, ""), "title=new"},
		{"textarea newlines become CRLF", wrap("<textarea name=\"t\">a\nb</textarea>", ""), "t=a%0D%0Ab&title=new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := formOp(nil).FormFields("http://a.test", tc.page, v)
			if err != nil {
				t.Fatal(err)
			}
			if got.Encode() != tc.want {
				t.Errorf("fields = %s, want %s", got.Encode(), tc.want)
			}
		})
	}
	// A declared field that's disabled can't be submitted by a person either.
	disabled := []byte(`<form id="f" method="post" action="/notes"><fieldset disabled><input name="title"></fieldset></form>`)
	if _, err := formOp(nil).FormFields("http://a.test", disabled, v); !errors.Is(err, ErrSourceChanged) {
		t.Errorf("declared disabled field: err = %v, want ErrSourceChanged", err)
	}
}

func TestParseRefusesFormOperationsThatCantWork(t *testing.T) {
	read := func(m map[string]any, f func(op, resp map[string]any)) {
		for _, o := range m["operations"].([]any) {
			op := o.(map[string]any)
			if op["effect"] == "read" {
				f(op, op["response"].(map[string]any))
				return
			}
		}
	}
	write := func(m map[string]any, f func(op, resp map[string]any)) {
		for _, o := range m["operations"].([]any) {
			op := o.(map[string]any)
			if op["effect"] == "write" {
				f(op, op["response"].(map[string]any))
				return
			}
		}
	}
	cases := []struct {
		name, want string
		mut        func(map[string]any)
	}{
		{"read with a location_regex", "location_regex reads a redirect", func(m map[string]any) {
			read(m, func(_, r map[string]any) {
				r["extract"] = map[string]any{"id": map[string]any{"location_regex": "^/notes/([0-9]+)$"}}
			})
		}},
		{"redirect in success_status", "not success_status", func(m map[string]any) {
			write(m, func(_, r map[string]any) { r["success_status"] = []any{200, 302} })
		}},
		{"write with no form page", "needs request.form_page", func(m map[string]any) {
			write(m, func(op, _ map[string]any) { delete(op["request"].(map[string]any), "form_page") })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(mutateFile(t, "testdata/notes-form.json", tc.mut))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFormActionComparesTheQuerySent(t *testing.T) {
	v := Vars{Input: map[string]any{"title": "new"}}
	page := func(action string) []byte {
		return []byte(`<form id="f" method="post" action="` + action + `"><input name="title"></form>`)
	}
	withQuery := func(o *Operation) { o.Request.Query = map[string]string{"mode": "delete"} }
	if _, err := formOp(withQuery).FormFields("http://a.test", page("/notes"), v); !errors.Is(err, ErrSourceChanged) {
		t.Errorf("a query the form doesn't post to: err = %v", err)
	}
	if _, err := formOp(withQuery).FormFields("http://a.test", page("/notes?mode=delete"), v); err != nil {
		t.Errorf("the query the form posts to: err = %v", err)
	}
	if _, err := formOp(nil).FormFields("http://a.test", page("/notes?mode=%ZZ"), v); !errors.Is(err, ErrSourceChanged) {
		t.Errorf("a malformed query: err = %v", err)
	}
	if _, err := formOp(nil).FormFields("http://a.test", page("/notes?mode=delete"), v); !errors.Is(err, ErrSourceChanged) {
		t.Errorf("a form action with a query the artifact doesn't send: err = %v", err)
	}
}

func TestLocationOnAnOrdinaryAnswerIsNotRead(t *testing.T) {
	o := &Operation{Response: ResponseSpec{RedirectSuccess: "^/notes/[0-9]+$", Extract: map[string]Extract{"id": {LocationRegex: "^/notes/([0-9]+)$"}}}}
	a := Answer{Status: 200, Location: "/notes/7"}
	if o.RedirectSucceeded("http://a.test", a) {
		t.Error("a 200 with a Location counted as a redirect")
	}
	if v, ok := o.ExtractValues("http://a.test", a, Vars{})["id"]; ok {
		t.Errorf("extracted %q from a 200", v)
	}
}

func TestRelativeRedirectIsNotRead(t *testing.T) {
	o := &Operation{Response: ResponseSpec{RedirectSuccess: "^/notes/[0-9]+$", Extract: map[string]Extract{"id": {LocationRegex: "^/notes/([0-9]+)$"}}}}
	for loc, want := range map[string]bool{"notes/7": false, "./notes/7": false, "/notes/7": true, "http://a.test/notes/7": true, "//evil.test/notes/7": false} {
		if got := o.RedirectSucceeded("http://a.test", Answer{Status: 303, Location: loc}); got != want {
			t.Errorf("RedirectSucceeded(%s) = %v, want %v", loc, got, want)
		}
		if _, got := o.ExtractValues("http://a.test", Answer{Status: 303, Location: loc}, Vars{})["id"]; got != want {
			t.Errorf("ExtractValues(%s) found = %v, want %v", loc, got, want)
		}
	}
}

func TestLocationRegexAnchorsTheWholePattern(t *testing.T) {
	o := &Operation{Response: ResponseSpec{RedirectSuccess: "^/notes/([0-9]+)|/done$"}}
	for loc, want := range map[string]bool{"/notes/123": true, "/done": true, "/notes/123/login": false, "/x/done": false} {
		if got := o.RedirectSucceeded("http://a.test", Answer{Status: 303, Location: loc}); got != want {
			t.Errorf("RedirectSucceeded(%s) = %v, want %v", loc, got, want)
		}
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
	got := o.ExtractValues("http://a.test", Answer{Body: page, Status: 303, Location: "/notes/42"}, Vars{Input: map[string]any{"ref": "m-2"}})
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
	off := o.ExtractValues("http://a.test", Answer{Status: 303, Location: "https://evil.test/notes/42"}, Vars{})
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
		if got := o.RedirectSucceeded("http://a.test", Answer{Status: 303, Location: loc}); got != want {
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
