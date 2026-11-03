package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/session"
)

// ErrNotSent wraps a Page.Fetch error that happened before any request
// could leave the page.
var ErrNotSent = errors.New("request not sent")

// ErrNeedsHuman reports that restoring a session needs a person.
var ErrNeedsHuman = errors.New("signing in again needs a person")

// ErrHumanActive reports that a person is completing a sign-in step in the
// binding's window. The page is not touched until the step ends.
var ErrHumanActive = errors.New("a person is completing a sign-in step in this binding's window")

// FetchRequest is one same-origin request made inside the page.
type FetchRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	// WantLocation asks for a redirect's target, which a page can't read.
	WantLocation bool
}

// FetchResult is the source's answer as the page saw it.
type FetchResult struct {
	Status  int
	Headers http.Header
	Body    []byte
	// Redirected reports a redirect the page did not follow. Status is 0
	// when the browser hides it.
	Redirected bool
	// Location is the redirect's target, when WantLocation was set and it
	// could be read.
	Location string
}

// Page is one dedicated browser profile serving one credential binding.
// Its session never leaves it.
type Page interface {
	// Fetch runs a same-origin request inside the page with the page's own
	// cookies. Errors that happen before dispatch wrap ErrNotSent.
	Fetch(ctx context.Context, in *integration.Compiled, req FetchRequest) (FetchResult, error)
	// Recover tries to restore a signed-in session without blocking on a
	// person. It returns ErrNeedsHuman when a person must act, after making
	// the step visible to them.
	Recover(ctx context.Context, in *integration.Compiled) error
}

// HostBinding is one credential binding the host serves.
type HostBinding struct {
	Name string
	Page Page
	// Grants lists the operations this binding may run, by integration ID.
	// The host enforces it itself, next to the runtime's scope check.
	Grants map[string][]string
}

// HostConfig configures a Host.
type HostConfig struct {
	ID           string
	Version      string
	Integrations []*integration.Compiled
	Bindings     []HostBinding
	// MaxQueue bounds jobs waiting per binding. Zero means 4.
	MaxQueue int
	Logger   *slog.Logger
}

// Host executes jobs against its own copy of published artifacts, through
// one Page per binding. Calls on one binding are serialized.
type Host struct {
	cfg      HostConfig
	integs   map[string]*integration.Compiled
	bindings map[string]*hostBinding
	log      *slog.Logger
}

type hostBinding struct {
	HostBinding
	slot    chan struct{}
	mu      sync.Mutex
	waiting int
	// sessions caches session values per integration, inside the page's
	// lifetime only. Values are never persisted or logged.
	sessions map[string]*pageSession
}

type pageSession struct {
	values  map[string]string
	expires time.Time
}

// NewHost validates the configuration.
func NewHost(cfg HostConfig) (*Host, error) {
	if cfg.MaxQueue == 0 {
		cfg.MaxQueue = 4
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	h := &Host{cfg: cfg, integs: map[string]*integration.Compiled{}, bindings: map[string]*hostBinding{}, log: cfg.Logger}
	for _, in := range cfg.Integrations {
		if in.Session.Provider != integration.ProviderBrowser {
			return nil, fmt.Errorf("capability host: integration %q uses provider %q; a browser host serves only provider browser", in.ID, in.Session.Provider)
		}
		h.integs[in.ID] = in
	}
	for _, b := range cfg.Bindings {
		if b.Name == "" || b.Page == nil {
			return nil, errors.New("capability host: a binding needs a name and a page")
		}
		if _, dup := h.bindings[b.Name]; dup {
			return nil, fmt.Errorf("capability host: duplicate binding %q", b.Name)
		}
		for id, ops := range b.Grants {
			in, ok := h.integs[id]
			if !ok {
				return nil, fmt.Errorf("capability host: binding %q grants unknown integration %q", b.Name, id)
			}
			for _, op := range ops {
				if _, ok := in.Op(op); !ok {
					return nil, fmt.Errorf("capability host: binding %q grants unknown operation %s.%s", b.Name, id, op)
				}
			}
		}
		h.bindings[b.Name] = &hostBinding{HostBinding: b, slot: make(chan struct{}, 1), sessions: map[string]*pageSession{}}
	}
	return h, nil
}

// Describe lists the artifacts each binding can execute.
func (h *Host) Describe() Description {
	d := Description{ID: h.cfg.ID, Version: h.cfg.Version}
	for name, b := range h.bindings {
		bd := BindingDesc{Binding: name}
		for id := range b.Grants {
			bd.Integrations = append(bd.Integrations, IntegrationDesc{ID: id, SHA256: h.integs[id].Hash})
		}
		slices.SortFunc(bd.Integrations, func(a, b IntegrationDesc) int { return compare(a.ID, b.ID) })
		d.Bindings = append(d.Bindings, bd)
	}
	slices.SortFunc(d.Bindings, func(a, b BindingDesc) int { return compare(a.Binding, b.Binding) })
	return d
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func notSent(code, format string, a ...any) Outcome {
	return Outcome{Dispatch: NotSent, Code: code, Detail: fmt.Sprintf(format, a...)}
}

// Execute runs one job. It never runs a plan supplied by the runtime: the
// request comes from the host's own artifact with the matching hash.
func (h *Host) Execute(ctx context.Context, job Job) Outcome {
	in, ok := h.integs[job.OpRef.IntegrationID]
	if !ok || in.Hash != job.OpRef.ArtifactSHA256 || in.Version != job.OpRef.Version {
		return notSent(CodeUnknownOp, "this capability has no artifact %s with that version and hash", job.OpRef.IntegrationID)
	}
	op, ok := in.Op(job.OpRef.Operation)
	if !ok {
		return notSent(CodeUnknownOp, "operation %q is not in the artifact", job.OpRef.Operation)
	}
	b, ok := h.bindings[job.Binding]
	if !ok || !slices.Contains(b.Grants[in.ID], op.Name) {
		return notSent(CodeDenied, "binding %q may not run %s.%s on this capability", job.Binding, in.ID, op.Name)
	}
	input := job.Input
	if input == nil {
		input = map[string]any{}
	}
	if err := op.Input.Validate(input); err != nil {
		return notSent(CodeInvalidInput, "%v", err)
	}
	if op.Effect == integration.EffectWrite && job.Marker == "" {
		return notSent(CodeInvalidInput, "a write job needs a receipt marker")
	}
	release, ok := b.acquire(ctx, h.cfg.MaxQueue)
	if !ok {
		return notSent(CodeBusy, "binding %q has too many queued calls", job.Binding)
	}
	defer release()

	values, out, ok := h.session(ctx, b, in)
	if !ok {
		return out
	}
	vars := integration.Vars{Input: input, Session: values}
	if op.Effect == integration.EffectWrite {
		vars.Receipt = map[string]string{"marker": job.Marker}
	}
	fields, out, ok := h.formFields(ctx, b, in, op, vars)
	if !ok {
		return out
	}
	req, err := op.Build(in.Source.Origin, vars, fields)
	if err != nil {
		return notSent(CodeInvalidInput, "%v", err)
	}
	fr := FetchRequest{Method: req.Method, URL: req.URL.String(), Headers: map[string]string{}, WantLocation: op.Response.RedirectSuccess != ""}
	for k := range req.Header {
		fr.Headers[k] = req.Header.Get(k)
	}
	for k, t := range in.Session.RequestHeaders {
		v, err := (integration.Vars{Session: values}).RenderString(t, nil)
		if err != nil {
			return notSent(CodeSession, "session header %s: %v", k, err)
		}
		fr.Headers[k] = v
	}
	if req.Body != nil {
		fr.Body, err = io.ReadAll(req.Body)
		if err != nil {
			return notSent(CodeInvalidInput, "rendering body: %v", err)
		}
	}
	res, err := b.Page.Fetch(ctx, in, fr)
	if err != nil {
		if errors.Is(err, ErrHumanActive) {
			return notSent(CodeNeedsHuman, "%v", err)
		}
		if errors.Is(err, ErrNotSent) {
			return notSent(CodeSession, "%v", err)
		}
		h.log.Warn("in-page request outcome unknown", "integration", in.ID, "operation", op.Name, "error", err)
		return Outcome{Dispatch: Unknown, Code: CodeLost, Detail: "the in-page request did not complete: " + err.Error()}
	}
	declared := res.Redirected && op.RedirectSucceeded(in.Source.Origin, integration.Answer{Location: res.Location})
	if (res.Redirected && !declared) || res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden {
		// A redirect is most often an expired session sending the request
		// to a sign-in page.
		b.dropSession(in.ID)
	}
	h.log.Info("executed", "binding", job.Binding, "integration", in.ID, "operation", op.Name, "status", res.Status, "redirected", res.Redirected)
	return Outcome{Dispatch: Sent, Status: res.Status, Body: res.Body, Redirected: res.Redirected, Location: res.Location}
}

// formFields fetches the operation's form page, if it has one, and reads the
// values to submit. Every failure here is before the submission, so nothing
// with an effect was sent.
func (h *Host) formFields(ctx context.Context, b *hostBinding, in *integration.Compiled, op *integration.CompiledOp, vars integration.Vars) (url.Values, Outcome, bool) {
	if op.Request.FormPage == nil {
		return nil, Outcome{}, true
	}
	path, err := op.FormPagePath(vars)
	if err != nil {
		return nil, notSent(CodeInvalidInput, "form page: %v", err), false
	}
	res, err := b.Page.Fetch(ctx, in, FetchRequest{Method: http.MethodGet, URL: in.Source.Origin + path, Headers: map[string]string{"Accept": integration.AcceptHTML}})
	if errors.Is(err, ErrHumanActive) {
		return nil, notSent(CodeNeedsHuman, "%v", err), false
	}
	if err != nil {
		return nil, notSent(CodeSession, "form page: %v", err), false
	}
	if res.Redirected || res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden {
		b.dropSession(in.ID)
		return nil, notSent(CodeSession, "the form page needs a new session"), false
	}
	if res.Status != http.StatusOK {
		return nil, notSent(CodeSourceChanged, "the form page answered %d", res.Status), false
	}
	target, err := vars.RenderString(op.Request.Path, url.PathEscape)
	if err != nil {
		return nil, notSent(CodeInvalidInput, "form action: %v", err), false
	}
	fields, err := op.FormFields(in.Source.Origin, res.Body, target, vars)
	if errors.Is(err, integration.ErrSourceChanged) {
		return nil, notSent(CodeSourceChanged, "%v", err), false
	}
	if err != nil {
		return nil, notSent(CodeInvalidInput, "%v", err), false
	}
	return fields, Outcome{}, true
}

func (b *hostBinding) acquire(ctx context.Context, maxQueue int) (func(), bool) {
	b.mu.Lock()
	if b.waiting >= maxQueue {
		b.mu.Unlock()
		return nil, false
	}
	b.waiting++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.waiting--
		b.mu.Unlock()
	}()
	select {
	case b.slot <- struct{}{}:
		return func() { <-b.slot }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (b *hostBinding) dropSession(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}

// session returns the integration's session values, checking the signed-in
// state and recovering it when needed. It is called with the binding slot
// held.
func (h *Host) session(ctx context.Context, b *hostBinding, in *integration.Compiled) (map[string]string, Outcome, bool) {
	b.mu.Lock()
	s := b.sessions[in.ID]
	b.mu.Unlock()
	if s != nil && time.Now().Before(s.expires) {
		return s.values, Outcome{}, true
	}
	ok, err := h.signedIn(ctx, b, in)
	if errors.Is(err, ErrHumanActive) {
		return nil, notSent(CodeNeedsHuman, "%v", err), false
	}
	if err != nil {
		return nil, notSent(CodeSession, "signed-in check failed: %v", err), false
	}
	if !ok {
		h.log.Info("session not signed in; recovering", "binding", b.Name, "integration", in.ID)
		if err := b.Page.Recover(ctx, in); err != nil {
			if errors.Is(err, ErrNeedsHuman) || errors.Is(err, ErrHumanActive) {
				return nil, notSent(CodeNeedsHuman, "%v", err), false
			}
			return nil, notSent(CodeSession, "signing in again failed: %v", err), false
		}
		if ok, err = h.signedIn(ctx, b, in); err != nil || !ok {
			return nil, notSent(CodeSession, "the session is not signed in after recovery"), false
		}
	}
	values := map[string]string{}
	for name, vs := range in.Session.Extract {
		res, err := b.Page.Fetch(ctx, in, FetchRequest{Method: http.MethodGet, URL: in.Source.Origin + vs.Request.Path, Headers: map[string]string{"Accept": "application/json, text/html"}})
		if errors.Is(err, ErrHumanActive) {
			return nil, notSent(CodeNeedsHuman, "%v", err), false
		}
		if err != nil {
			return nil, notSent(CodeSession, "session value %s: %v", name, err), false
		}
		v, err := session.ReadValue(&http.Response{StatusCode: res.Status, Header: res.Headers}, res.Body, vs)
		if err != nil {
			return nil, notSent(CodeSession, "session value %s: %v", name, err), false
		}
		values[name] = v
	}
	ttl := time.Duration(in.Session.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	b.mu.Lock()
	b.sessions[in.ID] = &pageSession{values: values, expires: time.Now().Add(ttl)}
	b.mu.Unlock()
	return values, Outcome{}, true
}

func (h *Host) signedIn(ctx context.Context, b *hostBinding, in *integration.Compiled) (bool, error) {
	chk := in.Session.SignedIn
	res, err := b.Page.Fetch(ctx, in, FetchRequest{Method: http.MethodGet, URL: in.Source.Origin + chk.Request.Path, Headers: map[string]string{"Accept": "application/json"}})
	if err != nil {
		return false, err
	}
	if res.Status != http.StatusOK {
		return false, nil
	}
	var doc any
	if json.Unmarshal(res.Body, &doc) != nil {
		return false, nil
	}
	v, ok := integration.Lookup(doc, chk.JSONPointer)
	return ok && (!chk.NotNull || v != nil), nil
}
