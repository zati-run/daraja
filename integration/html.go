package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	// Text is a text anchor: only elements whose text contains it match. For a
	// reconcile marker, select the element holding the marker alone: anchoring on a
	// whole row would also match a marker that a person typed into a title. It
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

func (a Answer) isRedirect() bool { return a.Status >= 300 && a.Status < 400 }

// redirectPath is the local path and query of a Location header. A location
// relative to the request's own path can't be resolved from the origin alone, so
// it isn't read: /auth/submit redirecting to notes/7 goes to /auth/notes/7, not
// /notes/7.
func redirectPath(origin, loc string) (string, bool) {
	if u, err := url.Parse(loc); err != nil || (!u.IsAbs() && !strings.HasPrefix(loc, "/")) {
		return "", false
	}
	return LocalLocation(origin, loc)
}

// RedirectSucceeded reports whether a redirect is the declared success shape.
func (o *Operation) RedirectSucceeded(origin string, a Answer) bool {
	if o.Response.RedirectSuccess == "" || !a.isRedirect() {
		return false
	}
	p, ok := redirectPath(origin, a.Location)
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
			// A Location header on any other answer, such as a 200 sign-in
			// page, shows nothing.
			if !a.isRedirect() {
				continue
			}
			p, ok := redirectPath(origin, a.Location)
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
func (o *Operation) FormFields(origin string, page []byte, v Vars) (url.Values, error) {
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
	if len(forms) != 1 || forms[0].DataAtom != atom.Form || forms[0].Namespace != "" {
		return nil, fmt.Errorf("%w: %q matches %d elements, want one form", ErrSourceChanged, fp.Selector, len(forms))
	}
	form := forms[0]
	if id, ok := attr(form, "id"); ok && countID(doc, id) != 1 {
		// Controls join a form by the first element with its ID.
		return nil, fmt.Errorf("%w: the form's ID isn't unique", ErrSourceChanged)
	}
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
	if !utf8Page(doc, form) {
		return nil, fmt.Errorf("%w: the form's encoding changed", ErrSourceChanged)
	}
	pagePath, err := o.FormPagePath(v)
	if err != nil {
		return nil, err
	}
	// The target is what will really be sent, with its query.
	req, err := o.Build(origin, v, url.Values{})
	if err != nil {
		return nil, err
	}
	actionPath, ok := formAction(origin, pagePath, doc, form)
	if !ok || !sameTarget(req.URL.RequestURI(), actionPath) {
		return nil, fmt.Errorf("%w: the form's action changed", ErrSourceChanged)
	}
	values, names, computed := formValues(doc, form)
	set := map[string]bool{}
	for k, t := range o.Request.Form {
		control, ok := names[k]
		if !ok {
			return nil, fmt.Errorf("%w: the form has no field %q", ErrSourceChanged, k)
		}
		s, err := v.RenderString(t, nil)
		if errors.Is(err, ErrAbsent) {
			continue // the form's own default stands
		}
		if err != nil {
			return nil, err
		}
		if control == atom.Textarea {
			s = crlf(s)
		}
		values.Set(k, s)
		set[k] = true
	}
	for name := range computed {
		if !set[name] {
			// Errors reach receipts, so this doesn't name the field.
			return nil, fmt.Errorf("%w: the form has a control whose default a browser may send differently", ErrSourceChanged)
		}
	}
	return values, nil
}

// countID counts the elements with an ID.
func countID(n *html.Node, id string) int {
	count := 0
	if v, ok := attr(n, "id"); ok && n.Type == html.ElementNode && v == id {
		count = 1
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		count += countID(c, id)
	}
	return count
}

// sameTarget compares two paths with their queries, in any query order.
func sameTarget(a, b string) bool {
	ua, err1 := url.ParseRequestURI(a)
	ub, err2 := url.ParseRequestURI(b)
	if err1 != nil || err2 != nil || ua.EscapedPath() != ub.EscapedPath() {
		return false
	}
	// URL.Query would silently drop an entry it can't parse.
	qa, err1 := url.ParseQuery(ua.RawQuery)
	qb, err2 := url.ParseQuery(ub.RawQuery)
	return err1 == nil && err2 == nil && qa.Encode() == qb.Encode()
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
		return has && id != "" && own == id
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.DataAtom == atom.Form && p.Namespace == "" {
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
func formValues(doc, form *html.Node) (url.Values, map[string]atom.Atom, map[string]bool) {
	values, names := url.Values{}, map[string]atom.Atom{}
	computed, overridden := map[string]bool{}, map[string]bool{}
	// A later checked radio unchecks an earlier one in its group.
	radios := map[string]*html.Node{}
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.DataAtom == atom.Template {
				return
			}
			// A radio in a datalist still unchecks its group, though it's never sent.
			if group, ok := checkedRadio(n); ok && ownedBy(n, form) {
				radios[group] = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.DataAtom == atom.Datalist || n.DataAtom == atom.Template {
				return // never submitted
			}
			name, named := attr(n, "name")
			// An element in SVG or MathML with a control's name isn't a control.
			named = named && name != "" && n.Namespace == "" && ownedBy(n, form) && !controlDisabled(n)
			if named && unreproducible(n, name) {
				computed[name] = true
			}
			switch {
			case !named:
			case n.DataAtom == atom.Input:
				t, _ := attr(n, "type")
				t = strings.ToLower(t)
				if submitOverride(n, t) {
					overridden[name] = true
				}
				names[name] = atom.Input
				switch t {
				case "file":
					computed[name] = true // a browser sends it empty, by a rule not followed here
				case "submit", "button", "reset", "image":
				case "radio":
					if radios[name] == n {
						val, ok := attr(n, "value")
						if !ok {
							val = "on"
						}
						values.Add(name, val)
					}
				case "checkbox":
					if _, on := attr(n, "checked"); on {
						val, ok := attr(n, "value")
						if !ok {
							val = "on"
						}
						values.Add(name, val)
					}
				default:
					val, _ := attr(n, "value")
					if !exactDefault(t, val) {
						computed[name] = true
					}
					values.Add(name, val)
				}
			case n.DataAtom == atom.Textarea:
				names[name] = atom.Textarea
				values.Add(name, rawText(n))
			case n.DataAtom == atom.Select:
				names[name] = atom.Select
				if _, sized := attr(n, "size"); sized {
					computed[name] = true // a list box may have no default
				}
				selectValue(n, name, values)
				return
			case n.DataAtom == atom.Button:
				t, ok := attr(n, "type")
				if !ok {
					t = "submit"
				}
				if submitOverride(n, strings.ToLower(t)) {
					overridden[name] = true
				}
				names[name] = atom.Button
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	// Even if another control has the name, declaring it could mean the button.
	for name := range overridden {
		delete(names, name)
	}
	// A browser converts newlines in every value it submits.
	for _, vs := range values {
		for i := range vs {
			vs[i] = crlf(vs[i])
		}
	}
	return values, names, computed
}

// unreproducible reports a control whose default a browser changes or adds to at
// submit, in ways not reimplemented here.
func unreproducible(n *html.Node, name string) bool {
	if strings.ContainsAny(name, "\r\n") || strings.EqualFold(name, "_charset_") {
		return true
	}
	if _, ok := attr(n, "dirname"); ok {
		return true
	}
	if wrap, _ := attr(n, "wrap"); n.DataAtom == atom.Textarea && strings.EqualFold(wrap, "hard") {
		return true
	}
	return false
}

// UTF8ContentType reports whether a Content-Type header leaves a form page
// encoded as UTF-8: a browser encodes the form in the page's encoding, and
// nothing here encodes any other. A header that doesn't parse says nothing, so it
// fails closed.
func UTF8ContentType(ct string) bool {
	if ct == "" {
		return true
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	cs, ok := params["charset"]
	return !ok || strings.EqualFold(cs, "utf-8") || strings.EqualFold(cs, "utf8")
}

var metaCharset = regexp.MustCompile(`(?i)charset\s*=\s*["']?([^\s;"']+)`)

// utf8Page reports whether a browser would encode this form as UTF-8. A page
// that declares no encoding is taken as UTF-8.
func utf8Page(doc, form *html.Node) bool {
	isUTF8 := func(s string) bool {
		return s == "" || strings.EqualFold(strings.TrimSpace(s), "utf-8") || strings.EqualFold(strings.TrimSpace(s), "utf8")
	}
	if cs, ok := attr(form, "accept-charset"); ok && !isUTF8(cs) {
		return false
	}
	for _, m := range cascadia.QueryAll(doc, cascadia.MustCompile("meta")) {
		if cs, ok := attr(m, "charset"); ok && !isUTF8(cs) {
			return false
		}
		if h, _ := attr(m, "http-equiv"); strings.EqualFold(h, "content-type") {
			c, _ := attr(m, "content")
			if strings.Contains(strings.ToLower(c), "charset") {
				// A browser allows spaces and quotes here. Anything unreadable fails closed.
				got := metaCharset.FindStringSubmatch(c)
				if got == nil || !isUTF8(got[1]) {
					return false
				}
			}
		}
	}
	return true
}

// asciiCollapse strips and collapses ASCII whitespace only, as a browser does for
// an option's text: strings.Fields would also collapse a no-break space.
func asciiCollapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r'
	}), " ")
}

// exactDefault reports whether a browser sends an input's default exactly as the
// page has it. Most types are sanitized, clamped or rewritten, and this doesn't
// reimplement that: anything else must come from the artifact.
func exactDefault(typ, val string) bool {
	switch typ {
	case "hidden":
		return true
	case "", "text", "search", "tel", "password":
		return !strings.ContainsAny(val, "\r\n")
	}
	return false
}

// submitOverride reports a submit button that sends the form somewhere else or
// some other way. The artifact can't declare it: its target is checked against
// the form, not the button.
func submitOverride(n *html.Node, typ string) bool {
	if n.DataAtom == atom.Button && (typ == "button" || typ == "reset") {
		return false
	}
	if n.DataAtom == atom.Input && typ != "submit" && typ != "image" {
		return false
	}
	for _, a := range []string{"formaction", "formmethod", "formenctype", "formtarget"} {
		if _, ok := attr(n, a); ok {
			return true
		}
	}
	return false
}

// checkedRadio returns the group of a radio button marked checked.
func checkedRadio(n *html.Node) (string, bool) {
	if n.DataAtom != atom.Input || n.Namespace != "" {
		return "", false
	}
	t, _ := attr(n, "type")
	name, _ := attr(n, "name")
	_, on := attr(n, "checked")
	return name, on && name != "" && strings.EqualFold(t, "radio")
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

// selectValue adds what a browser submits for a select. A selected option that's
// disabled stays selected but isn't submitted, so it sends nothing, and the first
// enabled option doesn't replace it.
func selectValue(n *html.Node, name string, values url.Values) {
	opts := []*html.Node{}
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode && c.DataAtom == atom.Option {
			opts = append(opts, c)
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	add := func(o *html.Node) {
		if _, off := attr(o, "disabled"); off || optgroupDisabled(o) {
			return
		}
		if v, ok := attr(o, "value"); ok {
			values.Add(name, v)
			return
		}
		values.Add(name, asciiCollapse(rawText(o)))
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
			add(o)
		}
	case len(selected) > 0: // a single select keeps the last one marked
		add(selected[len(selected)-1])
	default:
		// A drop-down shows its first enabled option.
		for _, o := range opts {
			_, off := attr(o, "disabled")
			if !off && !optgroupDisabled(o) {
				add(o)
				return
			}
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
