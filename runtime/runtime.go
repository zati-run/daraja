// Package runtime executes published integrations. It is the deployable
// part of Daraja and is built to run as one process on a Nanos unikernel:
// it starts no subprocess, browser or shell, and it reaches only the origins
// that loaded integrations declare.
//
// Every call is scope-checked, validated against the operation's input
// schema, sent with the principal's own session, mapped, and validated
// against the output schema. Writes are recorded in a durable receipt
// journal before dispatch and are never re-sent automatically.
package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/session"
)

// Error codes returned to callers.
const (
	CodeScopeDenied        = "scope_denied"
	CodeUnknownOperation   = "unknown_operation"
	CodeInvalidInput       = "invalid_input"
	CodeSessionUnavailable = "session_unavailable"
	CodeSourceRefused      = "source_refused"
	CodeOutcomeUncertain   = "outcome_uncertain"
	CodeDuplicate          = "duplicate_in_progress"
	CodeInvalidOutput      = "invalid_output"
	CodeNotSent            = "not_sent"
	CodeReceiptNotFound    = "receipt_not_found"
	CodeNotReconcilable    = "not_reconcilable"
	CodeInternal           = "internal"
	CodeKeyConflict        = "idempotency_key_conflict"
	// Capability codes come from the capability package.
	CodeCapabilityUnavailable = capability.CodeUnavailable
	CodeCapabilityBusy        = capability.CodeBusy
	CodeSessionNeedsHuman     = capability.CodeNeedsHuman
)

// Dispatcher hands a job to an execution capability. capability.Broker
// implements it.
type Dispatcher interface {
	Execute(ctx context.Context, job capability.Job) capability.Outcome
}

// Error is a structured, caller-visible failure. It never holds secrets or
// source data.
type Error struct {
	Code    string           `json:"code"`
	Message string           `json:"message"`
	Receipt *journal.Receipt `json:"receipt,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Principal is a runtime caller with explicit grants.
type Principal struct {
	ID          string  `json:"id"`
	TokenSHA256 string  `json:"token_sha256"`
	Grants      []Grant `json:"grants"`
	// ReceiptScope, when set, owns this principal's receipts instead of
	// ID. OAuth principals change ID with every consent; their receipts
	// stay with the client, so re-authorizing never re-sends an uncertain
	// write. Receipts are further scoped by credential binding.
	ReceiptScope string `json:"-"`
}

// receiptOwner is who owns a write's receipts: the principal, or for a
// principal with a ReceiptScope, that scope within one credential binding.
func receiptOwner(p *Principal, g Grant) string {
	if p.ReceiptScope == "" {
		return p.ID
	}
	return p.ReceiptScope + "\x00" + g.Credential
}

// Grant allows operations of one integration with one credential binding.
type Grant struct {
	Integration string   `json:"integration"`
	Operations  []string `json:"operations"`
	Credential  string   `json:"credential"`
}

func (p *Principal) grant(integ, op string) (Grant, bool) {
	for _, g := range p.Grants {
		if g.Integration == integ && slices.Contains(g.Operations, op) {
			return g, true
		}
	}
	return Grant{}, false
}

// Config configures a Runtime.
type Config struct {
	Integrations []*integration.Compiled
	Principals   []Principal
	Credentials  map[string]session.Binding
	Journal      *journal.Journal
	// HTTPClient is the base client for source requests. Its Transport may
	// be replaced in tests to inject faults.
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	Logger         *slog.Logger
	// Capabilities brokers operations of provider-browser integrations.
	Capabilities Dispatcher
	// CapabilityTimeout bounds one brokered call, including any sign-in the
	// capability performs first. It must exceed the capability's own job
	// timeout so the capability answers first. Zero means 90 seconds.
	CapabilityTimeout time.Duration
}

// Runtime is safe for concurrent use.
type Runtime struct {
	integrations map[string]*integration.Compiled
	principals   []Principal
	creds        map[string]session.Binding
	journal      *journal.Journal
	client       *http.Client
	timeout      time.Duration
	log          *slog.Logger
	caps         Dispatcher
	capTimeout   time.Duration

	mu       sync.Mutex
	sessions map[string]*session.Session
	inflight map[string]bool
}

// New validates the configuration and returns a Runtime.
func New(cfg Config) (*Runtime, error) {
	r := &Runtime{
		integrations: map[string]*integration.Compiled{},
		principals:   cfg.Principals,
		creds:        cfg.Credentials,
		journal:      cfg.Journal,
		client:       cfg.HTTPClient,
		timeout:      cfg.RequestTimeout,
		log:          cfg.Logger,
		caps:         cfg.Capabilities,
		capTimeout:   cfg.CapabilityTimeout,
		sessions:     map[string]*session.Session{},
		inflight:     map[string]bool{},
	}
	if r.client == nil {
		r.client = &http.Client{}
	}
	if r.timeout == 0 {
		r.timeout = 30 * time.Second
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.capTimeout == 0 {
		r.capTimeout = 90 * time.Second
	}
	if r.journal == nil {
		return nil, errors.New("runtime: a receipt journal is required")
	}
	for _, in := range cfg.Integrations {
		if _, dup := r.integrations[in.ID]; dup {
			return nil, fmt.Errorf("runtime: duplicate integration %q", in.ID)
		}
		r.integrations[in.ID] = in
	}
	seen := map[string]bool{}
	for _, p := range cfg.Principals {
		if p.ID == "" || seen[p.ID] {
			return nil, fmt.Errorf("runtime: principal id %q is empty or duplicate", p.ID)
		}
		seen[p.ID] = true
		if len(p.TokenSHA256) != 64 {
			return nil, fmt.Errorf("runtime: principal %q: token_sha256 must be 64 hex characters", p.ID)
		}
		for _, g := range p.Grants {
			in, ok := r.integrations[g.Integration]
			if !ok {
				return nil, fmt.Errorf("runtime: principal %q grants unknown integration %q", p.ID, g.Integration)
			}
			for _, op := range g.Operations {
				if _, ok := in.Op(op); !ok {
					return nil, fmt.Errorf("runtime: principal %q grants unknown operation %s.%s", p.ID, g.Integration, op)
				}
			}
			if g.Credential == "" && in.Session.Provider != integration.ProviderNone {
				return nil, fmt.Errorf("runtime: principal %q: grant for %q names no credential binding", p.ID, g.Integration)
			}
			if _, ok := r.creds[g.Credential]; !ok && in.Session.Provider != integration.ProviderNone && in.Session.Provider != integration.ProviderBrowser {
				return nil, fmt.Errorf("runtime: principal %q: credential binding %q is not configured", p.ID, g.Credential)
			}
		}
	}
	for _, rec := range r.journal.Recovered {
		r.log.Warn("receipt uncertain after restart", "integration", rec.Integration, "operation", rec.Operation, "principal", rec.Principal)
	}
	return r, nil
}

// HashToken returns the hex SHA-256 of a bearer token.
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// Authenticate maps a bearer token to a principal.
func (r *Runtime) Authenticate(token string) (*Principal, bool) {
	if token == "" {
		return nil, false
	}
	h := []byte(HashToken(token))
	var found *Principal
	for i := range r.principals {
		if subtle.ConstantTimeCompare(h, []byte(r.principals[i].TokenSHA256)) == 1 {
			found = &r.principals[i]
		}
	}
	return found, found != nil
}

// Principal returns a configured principal by ID.
func (r *Runtime) Principal(id string) (*Principal, bool) {
	for i := range r.principals {
		if r.principals[i].ID == id {
			return &r.principals[i], true
		}
	}
	return nil, false
}

// Entry is one operation a principal may call.
type Entry struct {
	Integration *integration.Compiled
	Op          *integration.CompiledOp
}

// Integrations lists the loaded integrations in ID order.
func (r *Runtime) Integrations() []*integration.Compiled {
	out := make([]*integration.Compiled, 0, len(r.integrations))
	for _, in := range r.integrations {
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b *integration.Compiled) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// ValidateGrant checks a grant created at run time (for example by OAuth
// consent) against the loaded integrations and credential bindings.
func (r *Runtime) ValidateGrant(g Grant) error {
	in, ok := r.integrations[g.Integration]
	if !ok {
		return fmt.Errorf("unknown integration %q", g.Integration)
	}
	if len(g.Operations) == 0 {
		return fmt.Errorf("grant for %q names no operations", g.Integration)
	}
	for _, op := range g.Operations {
		if _, ok := in.Op(op); !ok {
			return fmt.Errorf("unknown operation %s.%s", g.Integration, op)
		}
	}
	switch in.Session.Provider {
	case integration.ProviderNone:
	case integration.ProviderBrowser:
		if g.Credential == "" {
			return fmt.Errorf("grant for %q names no credential binding", g.Integration)
		}
	default:
		if _, ok := r.creds[g.Credential]; !ok {
			return fmt.Errorf("credential binding %q is not configured", g.Credential)
		}
	}
	return nil
}

// Entries lists the operations in a principal's scope, in a stable order.
func (r *Runtime) Entries(p *Principal) []Entry {
	var out []Entry
	for _, g := range p.Grants {
		in := r.integrations[g.Integration]
		for _, name := range g.Operations {
			op, _ := in.Op(name)
			out = append(out, Entry{Integration: in, Op: op})
		}
	}
	return out
}

// Result is a successful invocation.
type Result struct {
	Output  map[string]any
	Receipt *journal.Receipt // writes only
}

func (r *Runtime) resolve(p *Principal, integ, opName string) (*integration.Compiled, *integration.CompiledOp, Grant, *Error) {
	g, ok := p.grant(integ, opName)
	if !ok {
		return nil, nil, Grant{}, errf(CodeScopeDenied, "principal %q may not call %s.%s", p.ID, integ, opName)
	}
	in, ok := r.integrations[integ]
	if !ok {
		return nil, nil, Grant{}, errf(CodeUnknownOperation, "integration %q is not loaded", integ)
	}
	op, ok := in.Op(opName)
	if !ok {
		return nil, nil, Grant{}, errf(CodeUnknownOperation, "operation %s.%s is not defined", integ, opName)
	}
	return in, op, g, nil
}

// Invoke runs one operation. For a write, idempotencyKey is required.
func (r *Runtime) Invoke(ctx context.Context, p *Principal, integ, opName string, input map[string]any, idempotencyKey string) (*Result, *Error) {
	in, op, g, e := r.resolve(p, integ, opName)
	if e != nil {
		return nil, e
	}
	if input == nil {
		input = map[string]any{}
	}
	if err := op.Input.Validate(input); err != nil {
		return nil, errf(CodeInvalidInput, "%v", err)
	}
	if op.Effect == integration.EffectRead {
		return r.read(ctx, p, in, op, g, input)
	}
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		return nil, errf(CodeInvalidInput, "a write needs an idempotency_key of 1 to 200 characters")
	}
	return r.write(ctx, p, in, op, g, input, idempotencyKey)
}

func (r *Runtime) read(ctx context.Context, p *Principal, in *integration.Compiled, op *integration.CompiledOp, g Grant, input map[string]any) (*Result, *Error) {
	for attempt := 0; attempt < 2; attempt++ {
		x := r.exchange(ctx, in, op, g, integration.Vars{Input: input}, "", attempt > 0)
		if x.err != nil {
			return nil, x.err
		}
		if x.dispatch != capability.Sent {
			code := x.code
			if code == "" {
				code = CodeSourceRefused
			}
			return nil, errf(code, "request failed: %s", x.detail)
		}
		if (x.redirect || x.status == http.StatusUnauthorized || x.status == http.StatusForbidden) && attempt == 0 {
			// Reads are safe to repeat with a fresh session.
			r.dropSession(in, g.Credential)
			continue
		}
		if x.redirect && !slices.Contains(op.Response.SuccessStatus, x.status) {
			r.dropSession(in, g.Credential)
			return nil, errf(CodeSessionUnavailable, "the source redirected the request again after the session was refreshed")
		}
		out, e := r.mapResponse(op, x.status, x.body)
		r.log.Info("read", "principal", p.ID, "integration", in.ID, "operation", op.Name, "status", x.status, "ok", e == nil)
		if e != nil {
			return nil, e
		}
		return &Result{Output: out}, nil
	}
	return nil, errf(CodeSessionUnavailable, "the source refused the session after it was refreshed")
}

// Marker derives the reconciliation marker for a receipt key.
func Marker(key string) string { return "dj-" + key[:24] }

func (r *Runtime) write(ctx context.Context, p *Principal, in *integration.Compiled, op *integration.CompiledOp, g Grant, input map[string]any, idemKey string) (*Result, *Error) {
	key := journal.Key(receiptOwner(p, g), in.ID, op.Name, idemKey)
	if !r.claim(key) {
		return nil, errf(CodeDuplicate, "a call with this idempotency_key is in progress")
	}
	defer r.release(key)
	inputHash, err := hashInput(input)
	if err != nil {
		return nil, errf(CodeInvalidInput, "%v", err)
	}
	if old, ok := r.journal.Get(key); ok {
		return r.replayIfSame(op, old, inputHash)
	}
	if in.Session.Provider != integration.ProviderBrowser {
		if _, e := r.session(ctx, in, g.Credential, false); e != nil {
			return nil, e // nothing recorded: nothing was sent
		}
	}
	marker := Marker(key)
	vars := integration.Vars{Input: input, Receipt: map[string]string{"marker": marker}}
	// Render before recording, so a render failure records nothing.
	if _, err := integration.BuildRequest(in.Source.Origin, op.Request, vars); err != nil {
		return nil, errf(CodeInvalidInput, "%v", err)
	}
	rec, err := r.journal.Begin(journal.Receipt{Key: key, Principal: p.ID, Integration: in.ID, Operation: op.Name, Marker: marker, InputSHA256: inputHash})
	if errors.Is(err, journal.ErrExists) {
		return r.replayIfSame(op, rec, inputHash)
	}
	if err != nil {
		return nil, errf(CodeInternal, "receipt journal unavailable: %v", err)
	}
	x := r.exchange(ctx, in, op, g, vars, marker, false)
	switch {
	case x.err != nil || x.dispatch == capability.NotSent:
		code, detail := CodeNotSent, "the request was not sent: "+x.detail
		if x.err != nil {
			code, detail = x.err.Code, "the request was not sent: "+x.err.Message
		} else if x.code != "" {
			code = x.code
		}
		rec.State, rec.Code, rec.Detail = journal.Failed, code, detail+"; nothing reached the source, so it is safe to retry with a new idempotency_key"
	case x.dispatch == capability.Unknown:
		rec.State, rec.Code, rec.Detail = journal.Uncertain, CodeOutcomeUncertain, "the request was sent but no response was received: "+x.detail
	default:
		rec.HTTPStatus = x.status
		switch {
		case slices.Contains(op.Response.SuccessStatus, x.status) && op.Refused(x.body) && identifier(op, x.body) == "":
			rec.State, rec.Code, rec.Detail = journal.Failed, CodeSourceRefused, "the source answered with a declared failure"
		case slices.Contains(op.Response.SuccessStatus, x.status) && op.Refused(x.body):
			rec.State, rec.Code, rec.Detail = journal.Uncertain, CodeOutcomeUncertain, "the source answered with both a declared failure and an identifier; the effect may have been applied"
		case slices.Contains(op.Response.SuccessStatus, x.status) && !wellFormed(op, x.body):
			// A success status without the declared shape is most often a
			// sign-in or error page. The effect is unknown; reconcile it.
			rec.State, rec.Code, rec.Detail = journal.Uncertain, CodeOutcomeUncertain, "the source answered "+strconv.Itoa(x.status)+" without the declared response shape; the effect may have been applied"
		case slices.Contains(op.Response.SuccessStatus, x.status):
			rec.State = journal.Succeeded
			rec.Identifier = identifier(op, x.body)
		case x.redirect:
			r.dropSession(in, g.Credential)
			rec.State, rec.Code, rec.Detail = journal.Uncertain, CodeOutcomeUncertain, "the source redirected the write, most often to sign in; the effect may have been applied"
		case x.status == http.StatusUnauthorized || x.status == http.StatusForbidden:
			r.dropSession(in, g.Credential)
			rec.State, rec.Code, rec.Detail = journal.Failed, CodeSourceRefused, "the source refused the session or the action"
		case x.status >= 400 && x.status < 500 && x.status != http.StatusRequestTimeout:
			rec.State, rec.Code, rec.Detail = journal.Failed, CodeSourceRefused, "the source refused the request"
		default:
			rec.State, rec.Code, rec.Detail = journal.Uncertain, CodeOutcomeUncertain, "unexpected status "+strconv.Itoa(x.status)+"; the effect may have been applied"
		}
	}
	rec, jerr := r.journal.Update(rec)
	if jerr != nil {
		// The outcome could not be recorded durably; the journal still says
		// dispatching, which becomes uncertain on restart.
		r.log.Error("receipt update failed", "integration", in.ID, "operation", op.Name, "error", jerr)
		return nil, &Error{Code: CodeOutcomeUncertain, Message: "the outcome could not be recorded", Receipt: &rec}
	}
	r.log.Info("write", "principal", p.ID, "integration", in.ID, "operation", op.Name, "state", rec.State, "status", rec.HTTPStatus)
	if rec.State != journal.Succeeded {
		return nil, &Error{Code: rec.Code, Message: rec.Detail, Receipt: &rec}
	}
	out, e := r.mapResponse(op, x.status, x.body)
	if e != nil {
		e.Receipt = &rec
		e.Message = "the write succeeded but its output is invalid: " + e.Message
		return nil, e
	}
	return &Result{Output: out, Receipt: &rec}, nil
}

// hashInput is a SHA-256 of the input's canonical JSON (encoding/json sorts
// map keys).
func hashInput(input map[string]any) (string, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("input cannot be encoded: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// replayIfSame replays a receipt only for the same input. A key reused with
// different input sends nothing and reveals nothing of the other receipt.
func (r *Runtime) replayIfSame(op *integration.CompiledOp, rec journal.Receipt, inputHash string) (*Result, *Error) {
	if rec.InputSHA256 != "" && rec.InputSHA256 != inputHash {
		return nil, errf(CodeKeyConflict, "this idempotency_key was already used with different input; nothing was sent. Use a new, unique key")
	}
	return r.replay(op, rec)
}

// replay answers a repeated idempotency key from its receipt without
// sending anything.
func (r *Runtime) replay(op *integration.CompiledOp, rec journal.Receipt) (*Result, *Error) {
	switch rec.State {
	case journal.Succeeded:
		out := map[string]any{}
		if rec.Identifier != "" {
			// Only the identifier is retained; source data is not stored.
			for k, ptr := range op.Response.Output {
				if ptr == op.Response.Identifier {
					out[k] = typedIdentifier(op, k, rec.Identifier)
				}
			}
		}
		return &Result{Output: out, Receipt: &rec}, nil
	case journal.Dispatching, journal.Uncertain:
		return nil, &Error{Code: CodeOutcomeUncertain, Message: "this idempotency_key has an uncertain outcome; reconcile it before retrying", Receipt: &rec}
	default:
		return nil, &Error{Code: rec.Code, Message: "this idempotency_key already has a final outcome (" + rec.State + "); use a new key to try again", Receipt: &rec}
	}
}

func typedIdentifier(op *integration.CompiledOp, field, id string) any {
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(op.OutputSchema, &s)
	if t, ok := s.Properties[field]; ok && (t.Type == "integer" || t.Type == "number") {
		if n, err := strconv.ParseFloat(id, 64); err == nil {
			return n
		}
	}
	return id
}

// Receipt returns the receipt for a write in the principal's scope.
func (r *Runtime) Receipt(p *Principal, integ, opName, idemKey string) (*journal.Receipt, *Error) {
	in, op, g, e := r.resolve(p, integ, opName)
	if e != nil {
		return nil, e
	}
	if op.Effect != integration.EffectWrite {
		return nil, errf(CodeInvalidInput, "%s.%s is not a write", integ, opName)
	}
	rec, ok := r.journal.Get(journal.Key(receiptOwner(p, g), in.ID, op.Name, idemKey))
	if !ok {
		return nil, errf(CodeReceiptNotFound, "no receipt for this idempotency_key")
	}
	return &rec, nil
}

// Reconcile resolves an uncertain receipt with the operation's declared
// reconcile read. It never re-sends the write.
func (r *Runtime) Reconcile(ctx context.Context, p *Principal, integ, opName, idemKey string) (*journal.Receipt, *Error) {
	in, op, g, e := r.resolve(p, integ, opName)
	if e != nil {
		return nil, e
	}
	if op.Effect != integration.EffectWrite {
		return nil, errf(CodeInvalidInput, "%s.%s is not a write", integ, opName)
	}
	key := journal.Key(receiptOwner(p, g), in.ID, op.Name, idemKey)
	if !r.claim(key) {
		return nil, errf(CodeDuplicate, "a call with this idempotency_key is in progress")
	}
	defer r.release(key)
	rec, ok := r.journal.Get(key)
	if !ok {
		return nil, errf(CodeReceiptNotFound, "no receipt for this idempotency_key")
	}
	if rec.Final() {
		return &rec, nil
	}
	rc := op.Reconcile
	if rc == nil || rc.Strategy != integration.ReconcileMarker {
		return &rec, &Error{Code: CodeNotReconcilable, Message: "this operation declares no reconcile read; the outcome stays uncertain", Receipt: &rec}
	}
	target, _ := in.Op(rc.Operation)
	vars := integration.Vars{Receipt: map[string]string{"marker": rec.Marker}}
	input := map[string]any{}
	for k, t := range rc.Input {
		v, err := vars.RenderString(t, nil)
		if err != nil {
			return &rec, errf(CodeInternal, "reconcile input %s: %v", k, err)
		}
		input[k] = v
	}
	if err := target.Input.Validate(input); err != nil {
		return &rec, errf(CodeInternal, "reconcile input is invalid: %v", err)
	}
	res, e := r.read(ctx, p, in, target, g, input)
	if e != nil {
		return &rec, &Error{Code: e.Code, Message: "reconcile read failed; the outcome stays uncertain: " + e.Message, Receipt: &rec}
	}
	found, _ := integration.Lookup(map[string]any(res.Output), rc.Found)
	switch {
	case found != nil:
		rec.State, rec.Code, rec.Detail = journal.Succeeded, "reconciled", "the effect was found in the source"
		rec.Identifier = fmt.Sprint(found)
		if f, ok := found.(float64); ok {
			rec.Identifier = strconv.FormatFloat(f, 'f', -1, 64)
		}
	case time.Since(rec.DispatchAt) < time.Duration(rc.SettleSeconds)*time.Second:
		return &rec, &Error{Code: CodeOutcomeUncertain, Message: "not found yet, but the settle window has not passed; try again later", Receipt: &rec}
	default:
		rec.State, rec.Code, rec.Detail = journal.NotApplied, "reconciled", "the effect was not found in the source; a new idempotency_key may be used"
	}
	rec, err := r.journal.Update(rec)
	if err != nil {
		return nil, errf(CodeInternal, "receipt journal unavailable: %v", err)
	}
	r.log.Info("reconcile", "principal", p.ID, "integration", in.ID, "operation", op.Name, "state", rec.State)
	return &rec, nil
}

func (r *Runtime) claim(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[key] {
		return false
	}
	r.inflight[key] = true
	return true
}

func (r *Runtime) release(key string) {
	r.mu.Lock()
	delete(r.inflight, key)
	r.mu.Unlock()
}

func sessionKey(in *integration.Compiled, cred string) string { return in.ID + "\x00" + cred }

func (r *Runtime) session(ctx context.Context, in *integration.Compiled, cred string, refresh bool) (*session.Session, *Error) {
	k := sessionKey(in, cred)
	r.mu.Lock()
	s := r.sessions[k]
	r.mu.Unlock()
	if s != nil && !refresh && time.Now().Before(s.Expires) {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*r.timeout)
	defer cancel()
	s, err := session.Acquire(ctx, session.OriginClient(r.client, in.Source.Origin), in, r.creds[cred])
	if err != nil {
		r.log.Warn("session unavailable", "integration", in.ID, "error", err)
		return nil, errf(CodeSessionUnavailable, "%v", err)
	}
	r.mu.Lock()
	r.sessions[k] = s
	r.mu.Unlock()
	return s, nil
}

func (r *Runtime) dropSession(in *integration.Compiled, cred string) {
	r.mu.Lock()
	delete(r.sessions, sessionKey(in, cred))
	r.mu.Unlock()
}

// exchangeResult is one request's outcome, from the runtime's own HTTP
// client or from an execution capability.
type exchangeResult struct {
	err      *Error // a failure before dispatch, with its own code
	dispatch capability.Dispatch
	status   int
	body     []byte
	code     string
	detail   string
	// redirect reports a redirect the request did not follow.
	redirect bool
}

// exchange performs one request for an operation. Provider-browser
// integrations are brokered to a capability; the runtime never holds their
// session.
func (r *Runtime) exchange(ctx context.Context, in *integration.Compiled, op *integration.CompiledOp, g Grant, vars integration.Vars, marker string, refresh bool) exchangeResult {
	if in.Session.Provider == integration.ProviderBrowser {
		if r.caps == nil {
			return exchangeResult{dispatch: capability.NotSent, code: CodeCapabilityUnavailable, detail: "this runtime has no capability broker"}
		}
		ctx, cancel := context.WithTimeout(ctx, r.capTimeout)
		defer cancel()
		out := r.caps.Execute(ctx, capability.Job{
			OpRef:   capability.OpRef{IntegrationID: in.ID, Version: in.Version, ArtifactSHA256: in.Hash, Operation: op.Name},
			Binding: g.Credential, Input: vars.Input, Marker: marker,
		})
		return exchangeResult{dispatch: out.Dispatch, status: out.Status, body: out.Body, code: out.Code, detail: out.Detail, redirect: out.Redirected}
	}
	s, e := r.session(ctx, in, g.Credential, refresh)
	if e != nil {
		return exchangeResult{err: e}
	}
	resp, body, wrote, err := r.send(ctx, in, op, s, vars)
	if err != nil {
		var ee *Error
		if errors.As(err, &ee) {
			return exchangeResult{err: ee}
		}
		if !wrote {
			return exchangeResult{dispatch: capability.NotSent, code: CodeNotSent, detail: errClass(err)}
		}
		return exchangeResult{dispatch: capability.Unknown, detail: errClass(err)}
	}
	return exchangeResult{dispatch: capability.Sent, status: resp.StatusCode, body: body, redirect: resp.StatusCode >= 300 && resp.StatusCode < 400}
}

// send renders and sends a request. wrote reports whether the request was
// fully written to the connection, which decides "not sent" versus
// "uncertain" when an error occurs.
func (r *Runtime) send(ctx context.Context, in *integration.Compiled, op *integration.CompiledOp, s *session.Session, vars integration.Vars) (*http.Response, []byte, bool, error) {
	req, err := integration.BuildRequest(in.Source.Origin, op.Request, vars)
	if err != nil {
		return nil, nil, false, errf(CodeInvalidInput, "%v", err)
	}
	if err := s.Apply(req, in.Session.RequestHeaders); err != nil {
		return nil, nil, false, errf(CodeSessionUnavailable, "%v", err)
	}
	var mu sync.Mutex
	wrote := false
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		mu.Lock()
		// A write error can still leave a partial request on the wire.
		wrote = true
		mu.Unlock()
	}}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	// An operation never follows a redirect: a followed redirect turns a
	// write sent to an expired session into a 200 from the sign-in page.
	client := *r.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	mu.Lock()
	w := wrote
	mu.Unlock()
	if err != nil {
		return nil, nil, w || !definitelyNotSent(err), err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return nil, nil, true, fmt.Errorf("reading response: %w", err)
	}
	if len(body) > 8<<20 {
		return nil, nil, true, errors.New("response exceeds 8 MiB")
	}
	return resp, body, true, nil
}

// definitelyNotSent recognises errors that happen before any request byte
// can reach the source.
func definitelyNotSent(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

func errClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if definitelyNotSent(err) {
		return "connection not established"
	}
	return "connection error"
}

func (r *Runtime) mapResponse(op *integration.CompiledOp, status int, body []byte) (map[string]any, *Error) {
	if !slices.Contains(op.Response.SuccessStatus, status) {
		return nil, errf(CodeSourceRefused, "the source answered with status %d", status)
	}
	if op.Refused(body) {
		return nil, errf(CodeSourceRefused, "the source answered with a declared failure")
	}
	var doc any
	if len(op.Response.Output) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&doc); err != nil {
			return nil, errf(CodeInvalidOutput, "the response is not JSON")
		}
	}
	out := integration.MapOutput(doc, op.Response.Output)
	if err := op.Output.Validate(out); err != nil {
		return nil, errf(CodeInvalidOutput, "output does not match the output schema: %v", err)
	}
	return out, nil
}

// wellFormed reports whether a success response has the shape the
// operation declares: an identifier when one is declared, otherwise JSON
// when output is mapped from JSON.
func wellFormed(op *integration.CompiledOp, body []byte) bool {
	if op.Response.Identifier != "" {
		return identifier(op, body) != ""
	}
	if len(op.Response.Output) > 0 {
		return json.Valid(body)
	}
	return true
}

func identifier(op *integration.CompiledOp, body []byte) string {
	if op.Response.Identifier == "" {
		return ""
	}
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	v, ok := integration.Lookup(doc, op.Response.Identifier)
	if !ok || v == nil {
		return ""
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
