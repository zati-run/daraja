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

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Execution kinds. A rung is chosen in review: the lowest one that qualifies.
const (
	ExecHTTPJSON     = "http.json"
	ExecHTTPFormHTML = "http.form_html"
)

// ErrSourceChanged reports that a page no longer matches what the artifact
// was reviewed against. Nothing was sent; the artifact needs re-qualifying.
var ErrSourceChanged = errors.New("the source page no longer matches the artifact")

// Kind returns the operation's execution kind.
func (o *Operation) Kind() string {
	if o.Execution == "" {
		return ExecHTTPJSON
	}
	return o.Execution
}

// FormPage names the page holding the form a write submits.
type FormPage struct {
	Path     string `json:"path"`
	Selector string `json:"selector"` // CSS selector matching exactly one <form>
}

// Extract reads one output value. It sets exactly one of CSS, LocationRegex
// and JSONPointer.
type Extract struct {
	// CSS selects elements in an HTML answer. The value is the first match's
	// Attr, or its text.
	CSS  string `json:"css,omitempty"`
	Attr string `json:"attr,omitempty"`
	// Text is a text anchor: only elements whose text contains it match. It
	// may use {{input.x}}, for example to find a marker in a list.
	Text string `json:"text,omitempty"`
	// LocationRegex has one capture group, matched against the path and query
	// of the redirect target or the page's location. It must be anchored.
	LocationRegex string `json:"location_regex,omitempty"`
	JSONPointer   string `json:"json_pointer,omitempty"`
}

// Answer is one response from the source.
type Answer struct {
	Status int
	Body   []byte
	// Location is a redirect's target, when it is known.
	Location string
}

func validateExtract(where string, e Extract, add func(string, ...any)) {
	n := 0
	for _, set := range []bool{e.CSS != "", e.LocationRegex != "", e.JSONPointer != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		add("%s: exactly one of css, location_regex, json_pointer is required", where)
	}
	if e.CSS == "" && (e.Attr != "" || e.Text != "") {
		add("%s: attr and text apply only to css", where)
	}
	if e.CSS != "" {
		if _, err := cascadia.ParseGroup(e.CSS); err != nil {
			add("%s: css %q: %v", where, e.CSS, err)
		}
	}
	checkTemplate(where+".text", e.Text, map[string]bool{"input": true}, add)
	if e.LocationRegex != "" {
		re, err := compileLocationRegex(e.LocationRegex)
		if err != nil {
			add("%s: location_regex: %v", where, err)
		} else if re.NumSubexp() != 1 {
			add("%s: location_regex needs exactly one capture group", where)
		}
	}
	if e.JSONPointer != "" && !validPointer(e.JSONPointer) {
		add("%s: json_pointer %q is not a JSON Pointer", where, e.JSONPointer)
	}
}

// compileLocationRegex requires an anchored pattern, so that a sign-in page
// whose URL merely contains a matching piece never counts as success. The anchors
// are applied to the whole pattern: in ^/a|/b$, they would each bind to only one
// alternative, and /a/login would match.
func compileLocationRegex(s string) (*regexp.Regexp, error) {
	if !strings.HasPrefix(s, "^") || !strings.HasSuffix(s, "$") {
		return nil, fmt.Errorf("%q must start with ^ and end with $", s)
	}
	return regexp.Compile("^(?:" + s[1:len(s)-1] + ")$")
}

// LocalLocation resolves a location against the origin and returns its path
// and query. ok is false when it leaves the origin.
func LocalLocation(origin, loc string) (string, bool) {
	if loc == "" {
		return "", false
	}
	base, err := url.Parse(origin + "/")
	if err != nil {
		return "", false
	}
	u, err := base.Parse(loc)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host {
		return "", false
	}
	return u.RequestURI(), true
}

// RedirectSucceeded reports whether a redirect is the declared success shape.
func (o *Operation) RedirectSucceeded(origin string, a Answer) bool {
	if o.Response.RedirectSuccess == "" {
		return false
	}
	p, ok := LocalLocation(origin, a.Location)
	if !ok {
		return false
	}
	re, err := compileLocationRegex(o.Response.RedirectSuccess)
	return err == nil && re.MatchString(p)
}

// ExtractValues reads the operation's named extracts from an answer. A value
// that can't be read is left out; the output schema decides if that's valid.
func (o *Operation) ExtractValues(origin string, a Answer, v Vars) map[string]string {
	out := map[string]string{}
	var doc *html.Node
	var jsonDoc any
	jsonRead := false
	for name, e := range o.Response.Extract {
		switch {
		case e.CSS != "":
			if doc == nil {
				parsed, err := html.Parse(bytes.NewReader(a.Body))
				if err != nil {
					continue
				}
				doc = parsed
			}
			if s, ok := cssValue(doc, e, v); ok {
				out[name] = s
			}
		case e.LocationRegex != "":
			p, ok := LocalLocation(origin, a.Location)
			if !ok {
				continue
			}
			re, err := compileLocationRegex(e.LocationRegex)
			if err != nil {
				continue
			}
			if m := re.FindStringSubmatch(p); m != nil && m[1] != "" {
				out[name] = m[1]
			}
		case e.JSONPointer != "":
			if !jsonRead {
				jsonRead = true
				_ = json.Unmarshal(a.Body, &jsonDoc)
			}
			if val, ok := Lookup(jsonDoc, e.JSONPointer); ok {
				if s, err := scalarString(val); err == nil {
					out[name] = s
				}
			}
		}
	}
	return out
}

func cssValue(doc *html.Node, e Extract, v Vars) (string, bool) {
	sel, err := cascadia.ParseGroup(e.CSS)
	if err != nil {
		return "", false
	}
	anchor := e.Text
	if anchor != "" {
		anchor, err = v.RenderString(anchor, nil)
		if err != nil || anchor == "" {
			return "", false
		}
	}
	for _, n := range cascadia.QueryAll(doc, sel) {
		text := nodeText(n)
		if anchor != "" && !strings.Contains(text, anchor) {
			continue
		}
		if e.Attr == "" {
			return text, text != ""
		}
		if val, ok := attr(n, e.Attr); ok {
			return val, true
		}
	}
	return "", false
}

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		if c.Type == html.ElementNode && (c.DataAtom == atom.Script || c.DataAtom == atom.Style) {
			return
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// TypedOutput converts extracted strings to the types the output schema
// declares, so that an integer identifier in a URL is an integer output. A
// value that doesn't convert stays a string, and schema validation refuses it.
func (o *Operation) TypedOutput(values map[string]string) map[string]any {
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(o.OutputSchema, &s)
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
		types := []string{}
		switch t := s.Properties[k].Type.(type) {
		case string:
			types = append(types, t)
		case []any:
			for _, x := range t {
				if str, ok := x.(string); ok {
					types = append(types, str)
				}
			}
		}
		for _, t := range types {
			switch t {
			case "string":
				out[k] = v
			case "integer", "number":
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					out[k] = n
				}
			case "boolean":
				if b, err := strconv.ParseBool(v); err == nil {
					out[k] = b
				}
			default:
				continue
			}
			break
		}
	}
	return out
}

// FormFields reads the reviewed form from its page and returns the values to
// submit: the form's own fields (a CSRF token, for example), with the
// artifact's declared fields set over them. It fails with ErrSourceChanged
// when the page no longer has that form, or the form no longer posts where
// and how the artifact says, or lacks a declared field.
func (o *Operation) FormFields(origin string, page []byte, target string, v Vars) (url.Values, error) {
	fp := o.Request.FormPage
	sel, err := cascadia.ParseGroup(fp.Selector)
	if err != nil {
		return nil, fmt.Errorf("form selector: %w", err)
	}
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("%w: the form page does not parse", ErrSourceChanged)
	}
	forms := cascadia.QueryAll(doc, sel)
	if len(forms) != 1 || forms[0].DataAtom != atom.Form {
		return nil, fmt.Errorf("%w: %q matches %d elements, want one form", ErrSourceChanged, fp.Selector, len(forms))
	}
	form := forms[0]
	// Errors here reach receipts, so they name no value from the page.
	method, _ := attr(form, "method")
	if method == "" {
		method = "GET"
	}
	if !strings.EqualFold(method, o.Request.Method) {
		return nil, fmt.Errorf("%w: the form's method changed", ErrSourceChanged)
	}
	if enc, ok := attr(form, "enctype"); ok && !strings.EqualFold(enc, "application/x-www-form-urlencoded") {
		return nil, fmt.Errorf("%w: the form's encoding changed", ErrSourceChanged)
	}
	pagePath, err := o.FormPagePath(v)
	if err != nil {
		return nil, err
	}
	targetPath, ok1 := LocalLocation(origin, target)
	actionPath, ok2 := formAction(origin, pagePath, doc, form)
	if !ok1 || !ok2 || targetPath != actionPath {
		return nil, fmt.Errorf("%w: the form's action changed", ErrSourceChanged)
	}
	values, names := formValues(doc, form)
	for k, t := range o.Request.Form {
		if !names[k] {
			return nil, fmt.Errorf("%w: the form has no field %q", ErrSourceChanged, k)
		}
		s, err := v.RenderString(t, nil)
		if errors.Is(err, ErrAbsent) {
			continue // the form's own default stands
		}
		if err != nil {
			return nil, err
		}
		values.Set(k, s)
	}
	return values, nil
}

// formAction returns the path and query the form posts to, resolved as a browser
// would: against the page, or its <base href>. ok is false when it leaves the
// origin.
func formAction(origin, pagePath string, doc, form *html.Node) (string, bool) {
	page, err := url.Parse(origin + pagePath)
	if err != nil {
		return "", false
	}
	base := page
	if sel, err := cascadia.Parse("base[href]"); err == nil {
		if n := cascadia.Query(doc, sel); n != nil {
			href, _ := attr(n, "href")
			if base, err = page.Parse(href); err != nil {
				return "", false
			}
		}
	}
	action, _ := attr(form, "action")
	u := page // an empty action posts to the page itself, whatever the base says
	if action != "" {
		if u, err = base.Parse(action); err != nil {
			return "", false
		}
	}
	return LocalLocation(origin, u.String())
}

// ownedBy reports whether a control belongs to form: by its form attribute if it
// has one, otherwise by being inside it.
func ownedBy(n, form *html.Node) bool {
	if id, ok := attr(n, "form"); ok {
		own, has := attr(form, "id")
		return has && own == id
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.DataAtom == atom.Form {
			return p == form
		}
	}
	return false
}

// controlDisabled reports whether a control is disabled, by its own attribute or
// by a disabled fieldset around it. Controls in a fieldset's first legend stay
// enabled.
func controlDisabled(n *html.Node) bool {
	if _, on := attr(n, "disabled"); on {
		return true
	}
	child := n
	for p := n.Parent; p != nil; child, p = p, p.Parent {
		if p.DataAtom != atom.Fieldset {
			continue
		}
		if _, on := attr(p, "disabled"); !on {
			continue
		}
		first := p.FirstChild
		for first != nil && (first.Type != html.ElementNode || first.DataAtom != atom.Legend) {
			first = first.NextSibling
		}
		if child != first {
			return true
		}
	}
	return false
}

// formValues returns what a browser would submit for a form, and the name of
// every enabled named control. Buttons are named controls but send no value unless
// the artifact declares them. A disabled control isn't named, so declaring one
// counts as drift.
func formValues(doc, form *html.Node) (url.Values, map[string]bool) {
	values, names := url.Values{}, map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.DataAtom == atom.Datalist || n.DataAtom == atom.Template {
				return // never submitted
			}
			name, named := attr(n, "name")
			named = named && name != "" && ownedBy(n, form) && !controlDisabled(n)
			switch {
			case !named:
			case n.DataAtom == atom.Input:
				names[name] = true
				t, _ := attr(n, "type")
				switch strings.ToLower(t) {
				case "submit", "button", "reset", "image", "file":
				case "checkbox", "radio":
					if _, on := attr(n, "checked"); on {
						val, ok := attr(n, "value")
						if !ok {
							val = "on"
						}
						values.Add(name, val)
					}
				default:
					val, _ := attr(n, "value")
					values.Add(name, val)
				}
			case n.DataAtom == atom.Textarea:
				names[name] = true
				values.Add(name, crlf(rawText(n)))
			case n.DataAtom == atom.Select:
				names[name] = true
				selectValue(n, name, values)
				return
			case n.DataAtom == atom.Button:
				names[name] = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return values, names
}

// crlf normalizes newlines as a browser does when it submits a textarea.
func crlf(s string) string {
	return strings.NewReplacer("\r\n", "\r\n", "\r", "\r\n", "\n", "\r\n").Replace(s)
}

func rawText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

func selectValue(n *html.Node, name string, values url.Values) {
	opts := []*html.Node{}
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode && c.DataAtom == atom.Option {
			if _, off := attr(c, "disabled"); !off && !optgroupDisabled(c) {
				opts = append(opts, c)
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	optVal := func(o *html.Node) string {
		if v, ok := attr(o, "value"); ok {
			return v
		}
		return nodeText(o)
	}
	selected := []*html.Node{}
	for _, o := range opts {
		if _, on := attr(o, "selected"); on {
			selected = append(selected, o)
		}
	}
	_, multi := attr(n, "multiple")
	switch {
	case multi:
		for _, o := range selected {
			values.Add(name, optVal(o))
		}
	case len(selected) > 0: // a single select keeps the last one marked
		values.Add(name, optVal(selected[len(selected)-1]))
	case len(opts) > 0:
		// A drop-down shows its first option; a list box shows none selected.
		if size, _ := attr(n, "size"); size == "" || size == "0" || size == "1" {
			values.Add(name, optVal(opts[0]))
		}
	}
}

func optgroupDisabled(o *html.Node) bool {
	if o.Parent != nil && o.Parent.DataAtom == atom.Optgroup {
		_, off := attr(o.Parent, "disabled")
		return off
	}
	return false
}

// Build renders the operation's request. For a form submission, fields holds
// the values FormFields returned, and replaces request.form.
func (o *Operation) Build(origin string, v Vars, fields url.Values) (*http.Request, error) {
	spec := o.Request
	if fields != nil {
		spec.Form = nil
	}
	req, err := BuildRequest(origin, spec, v)
	if err != nil {
		return nil, err
	}
	if fields != nil {
		b := fields.Encode()
		req.Body = io.NopCloser(strings.NewReader(b))
		req.ContentLength = int64(len(b))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(b)), nil }
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if o.Kind() == ExecHTTPFormHTML {
		req.Header.Set("Accept", AcceptHTML)
	}
	return req, nil
}

// AcceptHTML is the Accept header of an HTML request.
const AcceptHTML = "text/html,application/xhtml+xml"

// FormPagePath renders the path of the operation's form page.
func (o *Operation) FormPagePath(v Vars) (string, error) {
	return v.RenderString(o.Request.FormPage.Path, url.PathEscape)
}
