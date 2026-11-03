// Package integration defines the Daraja integration artifact
// (format daraja.integration/v1alpha1), its validation, and the
// substitution-only templates that the runtime renders into requests.
//
// An integration is data. It holds no code, no credentials and no session
// values. Loading an artifact never grants authority: the runtime still
// checks principal scope, and the source application still authorizes
// every request.
package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/andybalholm/cascadia"
	"github.com/google/jsonschema-go/jsonschema"
	"golang.org/x/net/html"
)

// Format is the only artifact format this package accepts.
const Format = "daraja.integration/v1alpha1"

// Effect classes. A read never changes state in the source application.
const (
	EffectRead  = "read"
	EffectWrite = "write"
)

// Session providers.
const (
	ProviderNone         = "none"
	ProviderHTTPForm     = "http_form"
	ProviderBrowserLease = "browser_lease"
	// ProviderBrowser keeps the session in a browser execution capability's
	// dedicated profile. The runtime never holds it: every operation of such
	// an integration runs inside the page, brokered to the capability.
	ProviderBrowser = "browser"
)

// Reconcile strategies for writes.
const (
	ReconcileNone   = "none"
	ReconcileMarker = "marker"
)

// Integration is a published or candidate integration artifact.
type Integration struct {
	Format      string      `json:"format"`
	ID          string      `json:"id"`
	Version     string      `json:"version"`
	Description string      `json:"description,omitempty"`
	Source      Source      `json:"source"`
	Session     SessionSpec `json:"session"`
	Operations  []Operation `json:"operations"`
	Provenance  Provenance  `json:"provenance"`
}

// Source identifies the application an integration connects to.
type Source struct {
	Kind   string `json:"kind"` // only "web" in v1alpha1
	Origin string `json:"origin"`
}

// SessionSpec says how a user session is acquired and applied.
type SessionSpec struct {
	Provider string `json:"provider"`
	// SignIn is the HTTP sign-in recipe for the http_form provider.
	SignIn []HTTPStep `json:"sign_in,omitempty"`
	// BrowserSignIn is the browser recipe a host-side lease broker runs for
	// the browser_lease provider. The runtime never executes it.
	BrowserSignIn []BrowserStep `json:"browser_sign_in,omitempty"`
	// Extract names session values (for example a CSRF token) read with the
	// session after it is acquired.
	Extract map[string]ValueSource `json:"extract,omitempty"`
	// SignedIn proves the session belongs to a signed-in user.
	SignedIn *Check `json:"signed_in,omitempty"`
	// RequestHeaders are added to every operation request. Values may use
	// {{session.x}} only.
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	// TTLSeconds bounds how long a session is reused before it is checked
	// again. Zero means 600.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// HTTPStep is one request of an HTTP sign-in recipe.
type HTTPStep struct {
	Method  string                 `json:"method"`
	Path    string                 `json:"path"`
	Form    map[string]string      `json:"form,omitempty"`
	Extract map[string]ValueSource `json:"extract,omitempty"`
	// ExpectStatus lists acceptable final statuses (after redirects).
	ExpectStatus []int `json:"expect_status,omitempty"`
}

// BrowserStep is one step of a browser sign-in recipe.
type BrowserStep struct {
	Navigate string `json:"navigate,omitempty"` // path on the origin
	Fill     string `json:"fill,omitempty"`     // CSS selector
	Value    string `json:"value,omitempty"`    // template, {{credential.x}} only
	Click    string `json:"click,omitempty"`    // CSS selector
	// WaitPathNot waits until the page path no longer starts with this prefix.
	WaitPathNot string `json:"wait_path_not,omitempty"`
	// Human pauses for the user to finish a step in a visible browser, such
	// as typing a one-time passcode. The text is shown to the user.
	Human string `json:"human,omitempty"`
	// When skips the step unless the current path starts with this prefix.
	When string `json:"when,omitempty"`
}

// ValueSource reads one value from a response.
type ValueSource struct {
	// Request is used by session.extract. Sign-in steps read their own response.
	Request     *SimpleRequest `json:"request,omitempty"`
	JSONPointer string         `json:"json_pointer,omitempty"`
	HTMLInput   string         `json:"html_input,omitempty"` // value of <input name=...>
	HTMLMeta    string         `json:"html_meta,omitempty"`  // content of <meta name=...>
	Header      string         `json:"header,omitempty"`
}

// SimpleRequest is a bodiless request used by checks and extraction.
type SimpleRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Check proves a condition with one request.
type Check struct {
	Request     SimpleRequest `json:"request"`
	JSONPointer string        `json:"json_pointer"`
	NotNull     bool          `json:"not_null"`
}

// Operation is one named, typed action.
type Operation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Effect      string `json:"effect"`
	// Destructive is required on writes and confirmed in review.
	Destructive *bool `json:"destructive,omitempty"`
	// ReviewedSafeMethod lets a read use a method other than GET or HEAD
	// (for example a POST search or a GraphQL query). A reviewer sets it
	// after confirming the request has no effect; the method alone never
	// decides the effect class.
	ReviewedSafeMethod bool `json:"reviewed_safe_method,omitempty"`
	// Execution is how the operation runs: http.json (the default) or
	// http.form_html.
	Execution    string          `json:"execution,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Request      RequestSpec     `json:"request"`
	Response     ResponseSpec    `json:"response"`
	Reconcile    *Reconcile      `json:"reconcile,omitempty"`
}

// RequestSpec is a request template.
type RequestSpec struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body is a JSON template. A string leaf that is exactly one
	// {{input.x}} is replaced by the typed input value, or the key is
	// omitted when the input is absent.
	Body json.RawMessage   `json:"body,omitempty"`
	Form map[string]string `json:"form,omitempty"`
	// FormPage, for http.form_html, is fetched first. The submitted values are
	// the form's own, with Form set over them.
	FormPage *FormPage `json:"form_page,omitempty"`
}

// ResponseSpec maps a response to operation output.
type ResponseSpec struct {
	SuccessStatus []int `json:"success_status"`
	// Output maps output field names to JSON Pointers into the response body.
	// http.form_html uses Extract instead.
	Output map[string]string `json:"output,omitempty"`
	// Extract names the outputs of http.form_html.
	Extract map[string]Extract `json:"extract,omitempty"`
	// Identifier names the record a write created or changed, and is stored in
	// the receipt. It is a JSON Pointer for http.json, and the name of an
	// Extract for http.form_html.
	Identifier string `json:"identifier,omitempty"`
	// RedirectSuccess, for an http.form_html write, is an anchored regular
	// expression. A redirect to a local path and query matching it is success
	// (post-redirect-get). Any other redirect stays uncertain.
	RedirectSuccess string `json:"redirect_success,omitempty"`
	// RefusedWhen lists answers that are definite failures even with a success
	// status, such as a GraphQL failure sent with HTTP 200. An answer that
	// matches one and also carries the declared identifier contradicts itself,
	// so a write answered that way stays uncertain.
	RefusedWhen []Refusal `json:"refused_when,omitempty"`
}

// Refusal matches a reviewed failure shape in a response: a JSON value, or an
// HTML element.
type Refusal struct {
	// JSONPointer needs exactly one of Equals and Contains.
	JSONPointer string `json:"json_pointer,omitempty"`
	// CSS matches when an element exists. With Equals or Contains, the
	// element's text must also match.
	CSS string `json:"css,omitempty"`
	// Equals is compared with the value as a string, so "false" matches JSON
	// false and "7" matches JSON 7. A number must match as sent: "7" doesn't
	// match 7.0.
	Equals string `json:"equals,omitempty"`
	// Contains matches a substring, for a failure code nested in an error's
	// text. Keep it specific: a broad match can class an applied write as
	// refused.
	Contains string `json:"contains,omitempty"`
}

func (r Refusal) matches(s string) bool {
	return (r.Equals != "" && s == r.Equals) || (r.Contains != "" && strings.Contains(s, r.Contains))
}

// Refused reports whether a response body matches a declared failure shape.
func (o *Operation) Refused(body []byte) bool {
	if len(o.Response.RefusedWhen) == 0 {
		return false
	}
	// JSON entries need a body the ordinary decoder accepts: anything else may
	// be malformed, and the runtime couldn't read an identifier from it either.
	// Numbers stay as sent: decoded to float64, a distinct large number could
	// round into the declared one.
	var doc any
	jsonOK := json.Unmarshal(body, new(any)) == nil
	if jsonOK {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		jsonOK = dec.Decode(&doc) == nil
	}
	var page *html.Node
	for _, r := range o.Response.RefusedWhen {
		if r.CSS != "" {
			if page == nil {
				p, err := html.Parse(bytes.NewReader(body))
				if err != nil {
					continue
				}
				page = p
			}
			sel, err := cascadia.ParseGroup(r.CSS)
			if err != nil {
				continue
			}
			for _, n := range cascadia.QueryAll(page, sel) {
				if (r.Equals == "" && r.Contains == "") || r.matches(nodeText(n)) {
					return true
				}
			}
			continue
		}
		if !jsonOK {
			continue
		}
		v, ok := Lookup(doc, r.JSONPointer)
		if !ok {
			continue
		}
		if r.Contains != "" {
			if str, ok := v.(string); ok && strings.Contains(str, r.Contains) {
				return true
			}
			continue
		}
		if s, err := scalarString(v); err == nil && s == r.Equals {
			return true
		}
	}
	return false
}

// Reconcile says how an uncertain write is resolved from the source.
type Reconcile struct {
	Strategy string `json:"strategy"`
	// Operation names a read operation that finds the effect by marker.
	Operation string `json:"operation,omitempty"`
	// Input maps the reconcile operation's input fields to templates that
	// may use {{receipt.marker}}.
	Input map[string]string `json:"input,omitempty"`
	// Found is a JSON Pointer into the reconcile operation's output. A
	// non-null value means the effect exists; it is used as the identifier.
	Found string `json:"found,omitempty"`
	// SettleSeconds is how long after dispatch a "not found" may be trusted.
	SettleSeconds int `json:"settle_seconds,omitempty"`
}

// Provenance records how the artifact was produced. It is informative and
// confers no authority.
type Provenance struct {
	BuiltBy      string `json:"built_by,omitempty"`
	Planner      string `json:"planner,omitempty"`
	Observations string `json:"observations,omitempty"` // content hash
	ReviewedBy   string `json:"reviewed_by,omitempty"`
	ReviewedAt   string `json:"reviewed_at,omitempty"`
	Notes        string `json:"notes,omitempty"`
}

// Compiled is a validated integration with resolved schemas.
type Compiled struct {
	*Integration
	Hash string // sha256 of the artifact bytes
	ops  map[string]*CompiledOp
}

// CompiledOp is a validated operation.
type CompiledOp struct {
	*Operation
	Input  *jsonschema.Resolved
	Output *jsonschema.Resolved
}

// IsDestructive reports the reviewed destructive flag (false when unset).
func (o *Operation) IsDestructive() bool { return o.Destructive != nil && *o.Destructive }

// Op returns a compiled operation by name.
func (c *Compiled) Op(name string) (*CompiledOp, bool) {
	op, ok := c.ops[name]
	return op, ok
}

var (
	idRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	opRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	valueNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	methods     = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
)

// LoadFile reads and validates an artifact file.
func LoadFile(path string) (*Compiled, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates an artifact. Unknown fields are refused.
func Parse(b []byte) (*Compiled, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var in Integration
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("decode: trailing data after artifact")
	}
	sum := sha256.Sum256(b)
	c := &Compiled{Integration: &in, Hash: "sha256:" + hex.EncodeToString(sum[:]), ops: map[string]*CompiledOp{}}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ValidationError lists every problem found in an artifact.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid integration: " + strings.Join(e.Problems, "; ")
}

func (c *Compiled) validate() error {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	in := c.Integration
	if in.Format != Format {
		add("format must be %q", Format)
	}
	if !idRE.MatchString(in.ID) || strings.Contains(in.ID, "--") {
		add("id %q must match %s without '--'", in.ID, idRE)
	}
	if in.Version == "" {
		add("version is required")
	}
	if in.Source.Kind != "web" {
		add("source.kind %q is not supported (only \"web\")", in.Source.Kind)
	}
	if u, err := url.Parse(in.Source.Origin); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		add("source.origin %q must be scheme://host[:port] with no path", in.Source.Origin)
	} else if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		add("source.origin %q must use https unless it is a loopback address", in.Source.Origin)
	}
	c.validateSession(add)
	if len(in.Operations) == 0 {
		add("at least one operation is required")
	}
	for i := range in.Operations {
		op := &in.Operations[i]
		if !opRE.MatchString(op.Name) {
			add("operation %q: name must match %s", op.Name, opRE)
			continue
		}
		if _, dup := c.ops[op.Name]; dup {
			add("operation %q: duplicate name", op.Name)
			continue
		}
		c.ops[op.Name] = &CompiledOp{Operation: op}
	}
	for i := range in.Operations {
		op := &in.Operations[i]
		cop, ok := c.ops[op.Name]
		if !ok || cop.Operation != op {
			continue
		}
		c.validateOp(cop, add)
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func (c *Compiled) validateSession(add func(string, ...any)) {
	s := &c.Session
	switch s.Provider {
	case ProviderNone:
		if len(s.SignIn) > 0 || len(s.BrowserSignIn) > 0 {
			add("session: provider none takes no sign-in recipe")
		}
	case ProviderHTTPForm:
		if len(s.SignIn) == 0 {
			add("session: provider http_form needs sign_in steps")
		}
	case ProviderBrowserLease:
		if len(s.BrowserSignIn) == 0 {
			add("session: provider browser_lease needs browser_sign_in steps")
		}
	case ProviderBrowser:
		if len(s.SignIn) > 0 {
			add("session: provider browser takes browser_sign_in, not sign_in")
		}
		if s.SignedIn == nil {
			add("session: provider browser needs a signed_in check")
		}
	default:
		add("session.provider %q is not one of none, http_form, browser_lease, browser", s.Provider)
	}
	extracted := map[string]bool{}
	for i, st := range s.SignIn {
		where := fmt.Sprintf("session.sign_in[%d]", i)
		if st.Method != http.MethodGet && st.Method != http.MethodPost {
			add("%s: method must be GET or POST", where)
		}
		checkPath(where, st.Path, add)
		for k, v := range st.Form {
			checkTemplate(where+".form."+k, v, rootsFor(extracted, "credential", "extracted"), add)
		}
		for name, vs := range st.Extract {
			if !valueNameRE.MatchString(name) {
				add("%s.extract: bad name %q", where, name)
			}
			checkValueSource(where+".extract."+name, vs, false, add)
			extracted[name] = true
		}
	}
	for i, st := range s.BrowserSignIn {
		where := fmt.Sprintf("session.browser_sign_in[%d]", i)
		n := 0
		for _, set := range []bool{st.Navigate != "", st.Fill != "", st.Click != "", st.WaitPathNot != "", st.Human != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			add("%s: exactly one of navigate, fill, click, wait_path_not, human is required", where)
		}
		if st.Navigate != "" {
			checkPath(where, st.Navigate, add)
		}
		if st.Fill != "" {
			checkTemplate(where+".value", st.Value, map[string]bool{"credential": true}, add)
		}
	}
	for name, vs := range s.Extract {
		if !valueNameRE.MatchString(name) {
			add("session.extract: bad name %q", name)
		}
		checkValueSource("session.extract."+name, vs, true, add)
	}
	if s.SignedIn != nil {
		checkSimple("session.signed_in", s.SignedIn.Request, add)
		if !validPointer(s.SignedIn.JSONPointer) {
			add("session.signed_in.json_pointer %q is not a JSON Pointer", s.SignedIn.JSONPointer)
		}
	}
	for k, v := range s.RequestHeaders {
		checkHeader("session.request_headers", k, v, map[string]bool{"session": true}, add)
		for _, ref := range templateRefs(v) {
			if ref.root == "session" {
				if _, ok := s.Extract[ref.name]; !ok {
					add("session.request_headers.%s: {{session.%s}} is not extracted", k, ref.name)
				}
			}
		}
	}
	if s.TTLSeconds < 0 {
		add("session.ttl_seconds must not be negative")
	}
}

func (c *Compiled) validateOp(cop *CompiledOp, add func(string, ...any)) {
	op := cop.Operation
	where := "operation " + op.Name
	if op.Description == "" {
		add("%s: description is required", where)
	}
	switch op.Effect {
	case EffectRead:
		if op.Request.Method != http.MethodGet && op.Request.Method != http.MethodHead && !op.ReviewedSafeMethod {
			add("%s: a read using %s needs reviewed_safe_method; reads and writes stay separate", where, op.Request.Method)
		}
		if op.Destructive != nil && *op.Destructive {
			add("%s: a read cannot be destructive", where)
		}
		if op.Reconcile != nil {
			add("%s: a read takes no reconcile", where)
		}
	case EffectWrite:
		if op.Reconcile == nil {
			add("%s: a write must declare reconcile (marker or none)", where)
		}
		if op.Destructive == nil {
			add("%s: a write must declare destructive (true or false) after review", where)
		}
		if op.ReviewedSafeMethod {
			add("%s: reviewed_safe_method applies only to reads", where)
		}
	default:
		add("%s: effect %q is not read or write", where, op.Effect)
	}
	var err error
	cop.Input, err = compileSchema(op.InputSchema, true)
	if err != nil {
		add("%s: input_schema: %v", where, err)
	}
	cop.Output, err = compileSchema(op.OutputSchema, false)
	if err != nil {
		add("%s: output_schema: %v", where, err)
	}
	if props := schemaProps(op.InputSchema); props != nil {
		if _, clash := props["idempotency_key"]; clash {
			add("%s: input field idempotency_key is reserved", where)
		}
	}
	if !slices.Contains(methods, op.Request.Method) {
		add("%s: request.method %q is not supported", where, op.Request.Method)
	}
	inputRoots := map[string]bool{"input": true}
	if op.Effect == EffectWrite {
		inputRoots["receipt"] = true
	}
	checkPath(where+".request.path", op.Request.Path, add)
	checkTemplate(where+".request.path", op.Request.Path, inputRoots, add)
	for k, v := range op.Request.Query {
		checkTemplate(where+".request.query."+k, v, inputRoots, add)
	}
	for k, v := range op.Request.Headers {
		checkHeader(where+".request.headers", k, v, inputRoots, add)
	}
	if len(op.Request.Body) > 0 && len(op.Request.Form) > 0 {
		add("%s: request takes body or form, not both", where)
	}
	if len(op.Request.Body) > 0 {
		var tree any
		if err := json.Unmarshal(op.Request.Body, &tree); err != nil {
			add("%s: request.body is not JSON: %v", where, err)
		} else {
			walkStrings(tree, func(s string) { checkTemplate(where+".request.body", s, inputRoots, add) })
		}
	}
	for k, v := range op.Request.Form {
		checkTemplate(where+".request.form."+k, v, inputRoots, add)
	}
	if (len(op.Request.Body) > 0 || len(op.Request.Form) > 0) && (op.Request.Method == http.MethodGet || op.Request.Method == http.MethodHead) {
		add("%s: GET and HEAD take no body", where)
	}
	// Every input reference must name a declared input property.
	if props := schemaProps(op.InputSchema); props != nil {
		for _, ref := range opRefs(op) {
			if ref.root == "input" {
				if _, ok := props[ref.name]; !ok {
					add("%s: {{input.%s}} is not a property of input_schema", where, ref.name)
				}
			}
		}
	}
	if len(op.Response.SuccessStatus) == 0 && op.Response.RedirectSuccess == "" {
		add("%s: response.success_status is required", where)
	}
	for k, ptr := range op.Response.Output {
		if !validPointer(ptr) {
			add("%s: response.output.%s %q is not a JSON Pointer", where, k, ptr)
		}
	}
	c.validateExecution(op, where, add)
	for i, r := range op.Response.RefusedWhen {
		w := fmt.Sprintf("%s: response.refused_when[%d]", where, i)
		both := r.Equals != "" && r.Contains != ""
		switch {
		case (r.CSS == "") == (r.JSONPointer == ""):
			add("%s needs exactly one of json_pointer and css", w)
		case r.CSS != "":
			if _, err := cascadia.ParseGroup(r.CSS); err != nil {
				add("%s: css %q: %v", w, r.CSS, err)
			}
			if both {
				add("%s takes at most one of equals and contains", w)
			}
		case !validPointer(r.JSONPointer):
			add("%s.json_pointer is not a JSON Pointer", w)
		case both || (r.Equals == "" && r.Contains == ""):
			add("%s needs exactly one of equals and contains", w)
		}
	}
	if r := op.Reconcile; r != nil && op.Effect == EffectWrite {
		switch r.Strategy {
		case ReconcileNone:
			if r.Operation != "" || len(r.Input) > 0 || r.Found != "" {
				add("%s: reconcile none takes no operation, input or found", where)
			}
		case ReconcileMarker:
			hasMarker := false
			for _, ref := range opRefs(op) {
				if ref.root == "receipt" && ref.name == "marker" {
					hasMarker = true
				}
			}
			if !hasMarker {
				add("%s: reconcile marker needs {{receipt.marker}} in the request", where)
			}
			target, ok := c.ops[r.Operation]
			if !ok {
				add("%s: reconcile.operation %q does not exist", where, r.Operation)
			} else if target.Effect != EffectRead {
				add("%s: reconcile.operation %q must be a read", where, r.Operation)
			} else if len(target.Response.Output) == 0 && len(target.Response.Extract) == 0 {
				// With nothing mapped, any answer, even a sign-in page, reads as
				// "not found", which permits a second write.
				add("%s: reconcile.operation %q must map an output", where, r.Operation)
			}
			for k, v := range r.Input {
				checkTemplate(where+".reconcile.input."+k, v, map[string]bool{"receipt": true}, add)
			}
			if !validPointer(r.Found) || r.Found == "" {
				add("%s: reconcile.found must be a non-empty JSON Pointer", where)
			}
			if r.SettleSeconds < 0 {
				add("%s: reconcile.settle_seconds must not be negative", where)
			}
		default:
			add("%s: reconcile.strategy %q is not marker or none", where, r.Strategy)
		}
	}
}

// validateExecution checks what depends on the operation's execution kind.
func (c *Compiled) validateExecution(op *Operation, where string, add func(string, ...any)) {
	resp, req := &op.Response, &op.Request
	switch op.Kind() {
	case ExecHTTPJSON:
		if len(resp.Extract) > 0 || resp.RedirectSuccess != "" || req.FormPage != nil {
			add("%s: response.extract, response.redirect_success and request.form_page need http.form_html", where)
		}
		if resp.Identifier != "" && !validPointer(resp.Identifier) {
			add("%s: response.identifier is not a JSON Pointer", where)
		}
	case ExecHTTPFormHTML:
		if len(resp.Output) > 0 {
			add("%s: http.form_html maps output with response.extract, not response.output", where)
		}
		if len(req.Body) > 0 {
			add("%s: http.form_html takes request.form, not request.body", where)
		}
		if len(resp.Extract) == 0 && op.Effect == EffectRead {
			add("%s: a read needs response.extract", where)
		}
		for _, st := range resp.SuccessStatus {
			if st >= 300 && st < 400 {
				add("%s: an http.form_html operation declares a redirect with response.redirect_success, not success_status", where)
			}
		}
		if op.Effect != EffectWrite {
			for name, e := range resp.Extract {
				if e.LocationRegex != "" {
					add("%s: response.extract.%s: location_regex reads a redirect, which only a write answers with", where, name)
				}
			}
		}
		if op.Effect == EffectWrite {
			if req.FormPage == nil {
				add("%s: an http.form_html write needs request.form_page, so the form is checked before sending", where)
			}
			// A status alone can't show a form write worked: most sign-in and
			// validation pages answer 200.
			if _, ok := resp.Extract[resp.Identifier]; resp.Identifier == "" || !ok {
				add("%s: an http.form_html write needs response.identifier naming a response.extract", where)
			}
		} else if resp.RedirectSuccess != "" {
			add("%s: response.redirect_success applies only to writes", where)
		}
		for name, e := range resp.Extract {
			if !valueNameRE.MatchString(name) {
				add("%s: response.extract: bad name %q", where, name)
			}
			validateExtract(where+".response.extract."+name, e, add)
		}
		if resp.RedirectSuccess != "" {
			if _, err := compileLocationRegex(resp.RedirectSuccess); err != nil {
				add("%s: response.redirect_success: %v", where, err)
			}
		}
		if fp := req.FormPage; fp != nil {
			checkPath(where+".request.form_page.path", fp.Path, add)
			checkTemplate(where+".request.form_page.path", fp.Path, map[string]bool{"input": true}, add)
			if _, err := cascadia.ParseGroup(fp.Selector); fp.Selector == "" || err != nil {
				add("%s: request.form_page.selector %q is not a CSS selector", where, fp.Selector)
			}
			if req.Method == http.MethodGet || req.Method == http.MethodHead {
				add("%s: request.form_page needs a method that submits a body", where)
			}
		}
	default:
		add("%s: execution %q is not http.json or http.form_html", where, op.Execution)
	}
}

func compileSchema(raw json.RawMessage, mustBeObject bool) (*jsonschema.Resolved, error) {
	if len(raw) == 0 {
		return nil, errors.New("is required")
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if mustBeObject && s.Type != "object" {
		return nil, errors.New(`must have type "object"`)
	}
	return s.Resolve(nil)
}

func schemaProps(raw json.RawMessage) map[string]json.RawMessage {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	if s.Properties == nil {
		return map[string]json.RawMessage{}
	}
	return s.Properties
}

func opRefs(op *Operation) []ref {
	var out []ref
	out = append(out, templateRefs(op.Request.Path)...)
	for _, v := range op.Request.Query {
		out = append(out, templateRefs(v)...)
	}
	for _, v := range op.Request.Headers {
		out = append(out, templateRefs(v)...)
	}
	for _, v := range op.Request.Form {
		out = append(out, templateRefs(v)...)
	}
	if fp := op.Request.FormPage; fp != nil {
		out = append(out, templateRefs(fp.Path)...)
	}
	for _, e := range op.Response.Extract {
		out = append(out, templateRefs(e.Text)...)
	}
	if len(op.Request.Body) > 0 {
		var tree any
		if json.Unmarshal(op.Request.Body, &tree) == nil {
			walkStrings(tree, func(s string) { out = append(out, templateRefs(s)...) })
		}
	}
	return out
}

func checkPath(where, p string, add func(string, ...any)) {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, "://") || strings.ContainsAny(p, "\r\n#") {
		add("%s: path %q must be origin-relative and start with a single '/'", where, p)
	}
}

func checkSimple(where string, r SimpleRequest, add func(string, ...any)) {
	if r.Method != http.MethodGet {
		add("%s: request.method must be GET", where)
	}
	checkPath(where+".request.path", r.Path, add)
	if len(templateRefs(r.Path)) > 0 {
		add("%s: request.path takes no templates", where)
	}
}

func checkValueSource(where string, vs ValueSource, needRequest bool, add func(string, ...any)) {
	n := 0
	for _, set := range []bool{vs.JSONPointer != "", vs.HTMLInput != "", vs.HTMLMeta != "", vs.Header != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		add("%s: exactly one of json_pointer, html_input, html_meta, header is required", where)
	}
	if vs.JSONPointer != "" && !validPointer(vs.JSONPointer) {
		add("%s: json_pointer %q is not a JSON Pointer", where, vs.JSONPointer)
	}
	if needRequest {
		if vs.Request == nil {
			add("%s: request is required", where)
		} else {
			checkSimple(where, *vs.Request, add)
		}
	} else if vs.Request != nil {
		add("%s: a sign-in step reads its own response; request is not allowed", where)
	}
}

func checkHeader(where, k, v string, roots map[string]bool, add func(string, ...any)) {
	lk := strings.ToLower(k)
	if lk == "cookie" || lk == "authorization" || lk == "host" || lk == "content-length" {
		add("%s: header %q is managed by the runtime", where, k)
	}
	if strings.ContainsAny(k+v, "\r\n") {
		add("%s.%s: header contains a line break", where, k)
	}
	checkTemplate(where+"."+k, v, roots, add)
}

func rootsFor(_ map[string]bool, roots ...string) map[string]bool {
	m := map[string]bool{}
	for _, r := range roots {
		m[r] = true
	}
	return m
}

func walkStrings(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case map[string]any:
		for _, x := range t {
			walkStrings(x, f)
		}
	case []any:
		for _, x := range t {
			walkStrings(x, f)
		}
	}
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
