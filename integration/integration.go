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

	"github.com/google/jsonschema-go/jsonschema"
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
	ReviewedSafeMethod bool            `json:"reviewed_safe_method,omitempty"`
	InputSchema        json.RawMessage `json:"input_schema"`
	OutputSchema       json.RawMessage `json:"output_schema"`
	Request            RequestSpec     `json:"request"`
	Response           ResponseSpec    `json:"response"`
	Reconcile          *Reconcile      `json:"reconcile,omitempty"`
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
}

// ResponseSpec maps a response to operation output.
type ResponseSpec struct {
	SuccessStatus []int `json:"success_status"`
	// Output maps output field names to JSON Pointers into the response body.
	Output map[string]string `json:"output"`
	// Identifier is a JSON Pointer to the identifier of the record a write
	// created or changed. It is stored in the receipt.
	Identifier string `json:"identifier,omitempty"`
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
	if len(op.Response.SuccessStatus) == 0 {
		add("%s: response.success_status is required", where)
	}
	for k, ptr := range op.Response.Output {
		if !validPointer(ptr) {
			add("%s: response.output.%s %q is not a JSON Pointer", where, k, ptr)
		}
	}
	if op.Response.Identifier != "" && !validPointer(op.Response.Identifier) {
		add("%s: response.identifier is not a JSON Pointer", where)
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
