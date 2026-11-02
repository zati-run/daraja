// Package explore records redacted observations of the JSON traffic a web
// application makes while a bounded task is performed in a host browser.
//
// Observations keep the method, a normalized path, query parameter names,
// status, content type and inferred JSON shapes. They never keep header,
// cookie, query or body values. Writes are blocked in the browser unless the
// task allows them. Exploration is partial: the report states what was not
// observed.
package explore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/zati-run/daraja/builder/browser"
	"github.com/zati-run/daraja/integration"
)

// Task describes a bounded exploration.
type Task struct {
	Goal          string   `json:"goal"`
	IntegrationID string   `json:"integration_id"`
	Origin        string   `json:"origin"`
	AllowEffects  []string `json:"allow_effects"` // "read" and optionally "write"
	// Session is the session block the candidate integration will use. The
	// browser sign-in recipe and signed-in check are used during exploration.
	Session integration.SessionSpec `json:"session"`
	// PathPrefixes limits which same-origin paths are observed.
	PathPrefixes []string `json:"path_prefixes"`
	// GraphQLReadPaths are same-origin paths that serve GraphQL over POST.
	// In a read-only task a POST to one of them is let through only when
	// every operation in its body is a query (see graphQLReadOnly); anything
	// else, including a persisted query sent by hash only, stays blocked.
	// Observations on these paths are grouped by operation name.
	GraphQLReadPaths []string `json:"graphql_read_paths,omitempty"`
	Walk             []Step   `json:"walk"`
}

// Step is one exploration action.
type Step struct {
	Navigate string `json:"navigate,omitempty"`
	Click    string `json:"click,omitempty"`
	Fill     string `json:"fill,omitempty"`
	Value    string `json:"value,omitempty"`
	WaitMS   int    `json:"wait_ms,omitempty"`
	// Fetch runs a same-origin request from the page, for example to read a
	// route the UI exposes. Writes still need allow_effects.
	Fetch *FetchStep `json:"fetch,omitempty"`
	// Demonstrate hands the visible browser to a person, who performs this
	// part of the task while traffic is recorded. The text tells them what
	// to do; the step ends when they signal that they are done.
	Demonstrate string `json:"demonstrate,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Options configure an exploration.
type Options struct {
	// Demonstrate is called for a demonstrate step and returns when the
	// person signals that the demonstration is finished.
	Demonstrate func(ctx context.Context, instructions string) error
	// Quiet is how long the network must be idle before a step counts as
	// settled. Zero means 750 milliseconds.
	Quiet time.Duration
	// SettleMax bounds the wait for the network to settle. Zero means 15 s.
	SettleMax time.Duration
}

// FetchStep is a page-originated request.
type FetchStep struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
	// CSRFFrom names a session.extract value to send as the header CSRFHeader.
	CSRFFrom   string `json:"csrf_from,omitempty"`
	CSRFHeader string `json:"csrf_header,omitempty"`
}

// Observation is a redacted record of one route.
type Observation struct {
	Method        string         `json:"method"`
	PathTemplate  string         `json:"path_template"`
	PathParams    []PathParam    `json:"path_params,omitempty"`
	QueryNames    []string       `json:"query_names,omitempty"`
	Statuses      []int          `json:"statuses"`
	ContentType   string         `json:"content_type,omitempty"`
	ResourceType  string         `json:"resource_type"`
	RequestShape  map[string]any `json:"request_shape,omitempty"`
	ResponseShape map[string]any `json:"response_shape,omitempty"`
	Count         int            `json:"count"`
	Blocked       bool           `json:"blocked,omitempty"` // a write the explorer refused to send
	Steps         []int          `json:"steps"`
	// GraphQLOperation and GraphQLQuery are set for a GraphQL read path:
	// the operation name and its document with every inline literal replaced
	// by a placeholder (variable values are never recorded, only their shape).
	GraphQLOperation string `json:"graphql_operation,omitempty"`
	GraphQLQuery     string `json:"graphql_query,omitempty"`
}

// Coverage summarizes what was and was not observed.
type Coverage struct {
	Requests        int      `json:"requests"`
	JSONResponses   int      `json:"json_responses"`
	NonJSON         int      `json:"non_json"`
	BlockedWrites   int      `json:"blocked_writes"`
	OutsidePrefixes int      `json:"outside_prefixes"`
	BodyUnavailable int      `json:"body_unavailable"`
	Gaps            []string `json:"gaps"`
}

// Report is the result of an exploration.
type Report struct {
	Format       string        `json:"format"`
	Goal         string        `json:"goal"`
	Integration  string        `json:"integration_id"`
	Origin       string        `json:"origin"`
	AllowEffects []string      `json:"allow_effects"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Observations []Observation `json:"observations"`
	Coverage     Coverage      `json:"coverage"`
}

// ReportFormat identifies observation reports.
const ReportFormat = "daraja.observations/v1alpha1"

// Hash returns a content hash of the report's observations.
func (r *Report) Hash() string {
	b, _ := json.Marshal(r.Observations)
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (t *Task) graphQLPath(path string) bool {
	for _, p := range t.GraphQLReadPaths {
		if p == path {
			return true
		}
	}
	return false
}

func (t *Task) allows(effect string) bool {
	for _, e := range t.AllowEffects {
		if e == effect {
			return true
		}
	}
	return false
}

// pending merges one request's events. Each event type arrives on its own
// subscription and is handled by its own goroutine, so they can be seen in
// any order; a request is recorded once it was sent (in scope), answered
// and finished.
type pending struct {
	method, path, query string
	resType             string
	step                int
	status              int
	mime                string
	reqShape            map[string]any
	gql                 []gqlRequest

	sent, answered, finished bool
}

// inFlight reports whether an in-scope request has not completed yet.
func (p *pending) inFlight() bool { return p.sent && !(p.answered && p.finished) }

// entry returns the merge record for id, creating it. Called with mu held.
func (r *recorder) entry(id network.RequestID) *pending {
	p := r.pend[id]
	if p == nil {
		p = &pending{}
		r.pend[id] = p
	}
	return p
}

// complete removes and returns a request that has all its events, or nil.
// Called with mu held.
func (r *recorder) complete(id network.RequestID) *pending {
	p := r.pend[id]
	if p == nil || !p.sent || !p.answered || !p.finished {
		return nil
	}
	delete(r.pend, id)
	return p
}

type recorder struct {
	mu      sync.Mutex
	task    *Task
	origin  *url.URL
	step    int
	pend    map[network.RequestID]*pending
	obs     map[string]*Observation
	cov     Coverage
	pending sync.WaitGroup
	// sockets counts WebSockets the page opened or tried to open.
	sockets int
	// lastEvent is when the last network event was seen, for settling.
	lastEvent time.Time
}

func (r *recorder) touch() {
	r.mu.Lock()
	r.lastEvent = time.Now()
	r.mu.Unlock()
}

// settle waits until no in-scope request is in flight and the network has
// been quiet for o.Quiet, bounded by o.SettleMax. It reports whether the
// network settled, so a timeout is never mistaken for "not exercised".
func (r *recorder) settle(ctx context.Context, o Options) bool {
	deadline := time.Now().Add(o.SettleMax)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		busy := false
		for _, p := range r.pend {
			if p.inFlight() {
				busy = true
				break
			}
		}
		idle := !busy && time.Since(r.lastEvent) >= o.Quiet
		r.mu.Unlock()
		if idle {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

func (r *recorder) inScope(u *url.URL) bool {
	if u.Scheme != r.origin.Scheme || u.Host != r.origin.Host {
		return false
	}
	if len(r.task.PathPrefixes) == 0 {
		return true
	}
	for _, p := range r.task.PathPrefixes {
		if strings.HasPrefix(u.Path, p) {
			return true
		}
	}
	return false
}

func (r *recorder) add(method, path string, query url.Values, resType string, step int, status int, mime string, reqShape, respShape map[string]any, blocked bool, gql []gqlRequest) {
	tmpl, params := NormalizePath(path)
	key := method + " " + tmpl
	op := ""
	if len(gql) > 0 {
		tmpl, params = path, nil // a GraphQL path is fixed; never templated
		op = operationKey(gql)
		key = method + " " + tmpl + " #" + op
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.obs[key]
	if !ok {
		o = &Observation{Method: method, PathTemplate: tmpl, PathParams: params, ResourceType: resType, GraphQLOperation: op}
		if len(gql) == 1 {
			o.GraphQLQuery = redactLiterals(gql[0].Query)
		}
		r.obs[key] = o
	}
	o.Count++
	if status != 0 && !containsInt(o.Statuses, status) {
		o.Statuses = append(o.Statuses, status)
	}
	if !containsInt(o.Steps, step) {
		o.Steps = append(o.Steps, step)
	}
	for name := range query {
		if !containsStr(o.QueryNames, name) {
			o.QueryNames = append(o.QueryNames, name)
		}
	}
	sort.Strings(o.QueryNames)
	if mime != "" {
		o.ContentType = mime
	}
	o.RequestShape = Merge(o.RequestShape, reqShape)
	o.ResponseShape = Merge(o.ResponseShape, respShape)
	o.Blocked = o.Blocked || blocked
}

// Explore runs a task's walk in a signed-in browser and records
// observations. creds are used only for the sign-in recipe.
func Explore(ctx context.Context, b *browser.Browser, task *Task, signIn *integration.Compiled, creds map[string]string) (*Report, error) {
	return ExploreWith(ctx, b, task, signIn, creds, Options{})
}

// ExploreWith is Explore with options, for example to record a person's
// demonstration in a visible browser.
func ExploreWith(ctx context.Context, b *browser.Browser, task *Task, signIn *integration.Compiled, creds map[string]string, o Options) (*Report, error) {
	if o.Quiet == 0 {
		o.Quiet = 750 * time.Millisecond
	}
	if o.SettleMax == 0 {
		o.SettleMax = 15 * time.Second
	}
	origin, err := url.Parse(task.Origin)
	if err != nil || origin.Host == "" {
		return nil, fmt.Errorf("task origin %q is invalid", task.Origin)
	}
	rep := &Report{Format: ReportFormat, Goal: task.Goal, Integration: task.IntegrationID, Origin: task.Origin, AllowEffects: task.AllowEffects, StartedAt: time.Now().UTC()}
	if err := b.EnsureSignedIn(ctx, signIn, creds); err != nil {
		return nil, fmt.Errorf("sign-in: %w", err)
	}
	rec := &recorder{task: task, origin: origin, pend: map[network.RequestID]*pending{}, obs: map[string]*Observation{}}

	lctx, cancel := context.WithCancel(b.Ctx)
	defer cancel()
	var wg sync.WaitGroup
	readOnly := !task.allows(integration.EffectWrite)
	var sg *socketGuard
	if readOnly {
		sg = newSocketGuard(b.Ctx, lctx, &wg)
	}
	// listen records one tab's traffic. Each tab has its own CDP session,
	// so request IDs are keyed by tab. Writes are guarded browser-wide
	// (below), not per tab.
	listen := func(tctx context.Context, tab string) error {
		key := func(id network.RequestID) network.RequestID { return network.RequestID(tab + "/" + string(id)) }
		ectx, ecancel := context.WithCancel(tctx)
		context.AfterFunc(lctx, ecancel)
		sent := chromedp.Events(ectx, network.RequestWillBeSent)
		received := chromedp.Events(ectx, network.ResponseReceived)
		finished := chromedp.Events(ectx, network.LoadingFinished)
		failed := chromedp.Events(ectx, network.LoadingFailed)
		sockets := chromedp.Events(ectx, network.WebSocketCreated)
		maxBuf := int64(64 << 20)
		if _, err := chromedp.Call(tctx, network.Enable, network.EnableParams{MaxTotalBufferSize: &maxBuf}); err != nil {
			return err
		}
		if !task.allows(integration.EffectWrite) {
			// Request interception does not see WebSocket frames, and
			// Network.setBlockedURLs does not stop a WebSocket handshake in
			// this Chrome (tested). So socket constructors are disabled
			// before any page script runs in each attached tab, and in each
			// worker it starts (socketGuard); cross-site frames are held
			// paused. A document that loaded before its tab was attached
			// keeps them; the report names sockets as a gap.
			if _, err := chromedp.Call(tctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{Source: noSocketsJS}); err != nil {
				return fmt.Errorf("disabling WebSockets: %w", err)
			}
			if _, err := chromedp.Call(tctx, network.SetBypassServiceWorker, network.SetBypassServiceWorkerParams{Bypass: true}); err != nil {
				return fmt.Errorf("bypassing service workers: %w", err)
			}
			if err := sg.armTab(tctx); err != nil {
				return fmt.Errorf("disabling WebSockets in workers: %w", err)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, err := range sockets {
				if err != nil {
					return
				}
				rec.mu.Lock()
				rec.sockets++
				rec.mu.Unlock()
			}
		}()

		// record shapes and stores one completed request off the event loops.
		record := func(id network.RequestID, p *pending) {
			rec.pending.Add(1)
			go func() {
				defer rec.pending.Done()
				q, _ := url.ParseQuery(p.query)
				if !strings.Contains(p.mime, "json") {
					rec.mu.Lock()
					rec.cov.NonJSON++
					rec.mu.Unlock()
					rec.add(p.method, p.path, q, p.resType, p.step, p.status, p.mime, p.reqShape, nil, false, p.gql)
					return
				}
				body, err := chromedp.Call(tctx, network.GetResponseBody, network.GetResponseBodyParams{RequestID: id})
				var shape map[string]any
				if err == nil {
					shape = shapeOf(body.Body, body.Base64encoded)
				}
				rec.mu.Lock()
				if shape == nil {
					rec.cov.BodyUnavailable++
				} else {
					rec.cov.JSONResponses++
				}
				rec.mu.Unlock()
				rec.add(p.method, p.path, q, p.resType, p.step, p.status, p.mime, p.reqShape, shape, false, p.gql)
			}()
		}

		wg.Add(4)
		go func() { // a failed or blocked request is no longer in flight
			defer wg.Done()
			for ev, err := range failed {
				if err != nil {
					return
				}
				rec.mu.Lock()
				delete(rec.pend, key(ev.RequestID))
				rec.lastEvent = time.Now()
				rec.mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			for ev, err := range sent {
				if err != nil {
					return
				}
				u, err := url.Parse(ev.Request.URL)
				if err != nil {
					continue
				}
				rec.mu.Lock()
				if !rec.inScope(u) {
					if u.Host == origin.Host {
						rec.cov.OutsidePrefixes++
					}
					rec.mu.Unlock()
					continue
				}
				rec.cov.Requests++
				rec.lastEvent = time.Now()
				p := rec.entry(key(ev.RequestID))
				p.sent, p.method, p.path, p.query, p.resType, p.step, p.reqShape = true, strings.ToUpper(ev.Request.Method), u.Path, u.RawQuery, string(ev.Type), rec.step, postShape(ev.Request)
				p.gql = graphQLOps(task, p.method, u.Path, ev.Request)
				done := rec.complete(key(ev.RequestID))
				rec.mu.Unlock()
				if done != nil {
					record(ev.RequestID, done)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for ev, err := range received {
				if err != nil {
					return
				}
				rec.mu.Lock()
				p := rec.entry(key(ev.RequestID))
				p.answered, p.status, p.mime = true, int(ev.Response.Status), ev.Response.MimeType
				done := rec.complete(key(ev.RequestID))
				rec.mu.Unlock()
				if done != nil {
					record(ev.RequestID, done)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for ev, err := range finished {
				if err != nil {
					return
				}
				rec.mu.Lock()
				rec.entry(key(ev.RequestID)).finished = true
				rec.lastEvent = time.Now()
				done := rec.complete(key(ev.RequestID))
				rec.mu.Unlock()
				if done != nil {
					record(ev.RequestID, done)
				}
			}
		}()
		return nil
	}
	if !task.allows(integration.EffectWrite) {
		// The write guard runs on the browser target, so it pauses every
		// request from every target (tabs, popups, a form posted to a new
		// window, workers, service workers, iframes) before it is sent, and
		// of every resource type (including beacons). A tab attached late
		// can miss being recorded, never being guarded.
		bs := chromedp.FromContext(b.Ctx).Browser
		paused := chromedp.BrowserEvents(lctx, fetch.RequestPaused)
		if _, err := cdp.Call(b.Ctx, bs, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}}); err != nil {
			return nil, fmt.Errorf("browser-wide write guard: %w", err)
		}
		defer cdp.Call(b.Ctx, bs, fetch.Disable, cdp.Empty{}) //nolint:errcheck
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev, err := range paused {
				if err != nil {
					return
				}
				if readSafe(task, ev.Request) {
					_, _ = cdp.Call(b.Ctx, bs, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: ev.RequestID})
					continue
				}
				_, _ = cdp.Call(b.Ctx, bs, fetch.FailRequest, fetch.FailRequestParams{RequestID: ev.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
				m := strings.ToUpper(ev.Request.Method)
				if u, err := url.Parse(ev.Request.URL); err == nil && rec.inScope(u) {
					rec.mu.Lock()
					rec.cov.BlockedWrites++
					step := rec.step
					rec.mu.Unlock()
					rec.add(m, u.Path, u.Query(), string(ev.ResourceType), step, 0, "", postShape(ev.Request), nil, true, graphQLOps(task, m, u.Path, ev.Request))
				}
			}
		}()
	}
	if err := listen(b.Ctx, "main"); err != nil {
		return nil, err
	}
	if readOnly {
		if err := sg.stopServiceWorkers(b.Ctx); err != nil {
			return nil, fmt.Errorf("stopping running service workers: %w", err)
		}
	}
	unarmedTabs := 0

	// Follow tabs the page or the person opens (a demonstration often does),
	// so their traffic is recorded and, in a read-only task, their writes are
	// blocked too. Requests a tab sends before it is attached are neither
	// guarded nor recorded; the report says so when that can have happened.
	mainID := chromedp.FromContext(b.Ctx).Target.TargetID
	var tabsMu sync.Mutex
	tabs := map[target.ID]bool{mainID: true}
	extraTabs := 0
	created := chromedp.BrowserEvents(lctx, target.TargetCreated)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e, err := range created {
			if err != nil {
				return
			}
			if e.TargetInfo == nil || e.TargetInfo.Type != "page" {
				continue
			}
			id := e.TargetInfo.TargetID
			tabsMu.Lock()
			seen := tabs[id]
			tabs[id] = true
			if !seen {
				extraTabs++
			}
			tabsMu.Unlock()
			if seen {
				continue
			}
			// Detach on cancel: cancelling must not close the person's tab.
			tctx, tcancel := chromedp.NewContext(b.Ctx, chromedp.WithTargetID(id), chromedp.WithDetachOnCancel())
			context.AfterFunc(lctx, tcancel)
			if err := chromedp.Do(tctx); err != nil {
				continue
			}
			if err := listen(tctx, string(id)); err != nil {
				tabsMu.Lock()
				unarmedTabs++
				tabsMu.Unlock()
			}
		}
	}()

	walkErr := runWalk(ctx, b, task, rec, signIn, o)
	unsettled := !rec.settle(ctx, o)
	rec.pending.Wait()
	cancel()
	wg.Wait()
	if readOnly {
		sg.disarm(b.Ctx)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	keys := make([]string, 0, len(rec.obs))
	for k := range rec.obs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rep.Observations = append(rep.Observations, *rec.obs[k])
	}
	rep.Coverage = rec.cov
	rep.Coverage.Gaps = []string{
		"only routes exercised by this walk were observed",
		"WebSocket, server-sent events, service-worker and streamed traffic is not observed, and WebSocket frames are not checked by the write guard",
		"non-JSON responses are counted but not shaped",
		"path templates are heuristic and can merge or split routes",
		"shapes come from sampled responses and are not a server contract",
	}
	if rec.sockets > 0 {
		gap := fmt.Sprintf("the page opened or tried to open %d WebSocket(s); socket frames are not observed or checked by the write guard", rec.sockets)
		if !task.allows(integration.EffectWrite) {
			gap += " (socket constructors were disabled in attached tabs and their workers before their scripts ran; a document loaded before its tab was attached keeps them)"
		}
		rep.Coverage.Gaps = append(rep.Coverage.Gaps, gap)
	}
	tabsMu.Lock()
	if extraTabs > 0 {
		gap := fmt.Sprintf("%d other tab(s) opened; requests a tab sent before Daraja attached to it were not recorded", extraTabs)
		if !task.allows(integration.EffectWrite) {
		}
		rep.Coverage.Gaps = append(rep.Coverage.Gaps, gap)
	}
	tabsMu.Unlock()
	if readOnly {
		sg.mu.Lock()
		if n := len(sg.held); n > 0 {
			rep.Coverage.Gaps = append(rep.Coverage.Gaps, fmt.Sprintf("%d cross-site frame(s) or worker(s) were held paused so they could not open sockets; they did not load and their traffic was not recorded", n))
		}
		if sg.running > 0 {
			rep.Coverage.Gaps = append(rep.Coverage.Gaps, fmt.Sprintf("%d frame(s) or worker(s) were already running when Daraja attached; they keep socket constructors, and socket frames are not checked by the write guard", sg.running))
		}
		sg.mu.Unlock()
		if unarmedTabs > 0 {
			rep.Coverage.Gaps = append(rep.Coverage.Gaps, fmt.Sprintf("%d tab(s) could not be fully attached; their sockets and workers were not disabled", unarmedTabs))
		}
	}
	if unsettled {
		rep.Coverage.Gaps = append(rep.Coverage.Gaps, "the network did not settle before the recording ended; late requests may be missing")
	}
	if !task.allows(integration.EffectWrite) {
		rep.Coverage.Gaps = append(rep.Coverage.Gaps, "writes were blocked in the browser; blocked routes show request shapes only")
	}
	rep.FinishedAt = time.Now().UTC()
	if walkErr != nil {
		return rep, fmt.Errorf("walk stopped early (partial report kept): %w", walkErr)
	}
	return rep, nil
}

func runWalk(ctx context.Context, b *browser.Browser, task *Task, rec *recorder, in *integration.Compiled, o Options) error {
	values := map[string]string{}
	for i, st := range task.Walk {
		rec.mu.Lock()
		rec.step = i
		rec.mu.Unlock()
		sctx, cancel := context.WithTimeout(b.Ctx, 60*time.Second)
		var err error
		switch {
		case st.Navigate != "":
			err = chromedp.Do(sctx, chromedp.Navigate(task.Origin+st.Navigate))
		case st.Click != "":
			err = chromedp.Do(sctx, chromedp.WaitVisible(chromedp.CSS(st.Click)), chromedp.Click(chromedp.CSS(st.Click)))
		case st.Fill != "":
			err = chromedp.Do(sctx, chromedp.WaitVisible(chromedp.CSS(st.Fill)), chromedp.SendKeys(chromedp.CSS(st.Fill), st.Value))
		case st.Fetch != nil:
			err = pageFetch(sctx, b, task, st.Fetch, in, values)
		case st.Demonstrate != "":
			if o.Demonstrate == nil {
				err = errors.New("a demonstrate step needs a person and a visible browser")
			} else {
				err = o.Demonstrate(ctx, st.Demonstrate)
			}
		}
		cancel()
		if err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		rec.touch()
		rec.settle(ctx, o)
		if st.WaitMS > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(st.WaitMS) * time.Millisecond):
			}
		}
	}
	return nil
}

func pageFetch(ctx context.Context, b *browser.Browser, task *Task, f *FetchStep, in *integration.Compiled, values map[string]string) error {
	m := strings.ToUpper(f.Method)
	if m != "GET" && m != "HEAD" && !task.allows(integration.EffectWrite) {
		return fmt.Errorf("fetch %s %s is a write and the task does not allow writes", m, f.Path)
	}
	headers := map[string]string{"Accept": "application/json"}
	if f.CSRFFrom != "" {
		v, ok := values[f.CSRFFrom]
		if !ok {
			vs, ok := in.Session.Extract[f.CSRFFrom]
			if !ok {
				return fmt.Errorf("csrf_from %q is not a session.extract value", f.CSRFFrom)
			}
			status, body, err := b.FetchJSON(ctx, vs.Request.Path)
			if err != nil || status != 200 {
				return fmt.Errorf("read %s: status %d %v", f.CSRFFrom, status, err)
			}
			var doc any
			_ = json.Unmarshal(body, &doc)
			s, _ := integration.Lookup(doc, vs.JSONPointer)
			v, _ = s.(string)
			if v == "" {
				return fmt.Errorf("no value for %s", f.CSRFFrom)
			}
			values[f.CSRFFrom] = v
		}
		headers[f.CSRFHeader] = v
	}
	opts := map[string]any{"method": m, "credentials": "same-origin", "headers": headers}
	if len(f.Body) > 0 {
		headers["Content-Type"] = "application/json"
		opts["body"] = string(f.Body)
	}
	ob, _ := json.Marshal(opts)
	js := fmt.Sprintf(`fetch(%q, %s).then(r => r.status)`, f.Path, ob)
	_, err := chromedp.Run(ctx, chromedp.Evaluate[int](js, chromedp.EvalAwaitPromise))
	return err
}

// noSocketsJS disables socket constructors (and refuses shared workers) in a
// read-only walk. Socket frames would be a write channel the request guard
// cannot check.
const noSocketsJS = `(() => {
  const refuse = function () { throw new DOMException("sockets are disabled in a read-only Daraja walk", "SecurityError"); };
  // A shared worker is not attached to any one tab, so it cannot be armed
  // like a dedicated worker; it is refused instead.
  for (const name of ["WebSocket", "WebTransport", "SharedWorker"]) {
    try { Object.defineProperty(globalThis, name, {value: refuse, writable: false, configurable: false}); } catch (e) {}
  }
})();`

// readSafe reports whether a read-only walk may send the request. GET and
// HEAD pass, except on a GraphQL read path, where they pass only with a
// query parameter that is read-only: some servers accept mutations over GET,
// and a persisted query sent by hash only cannot be checked. OPTIONS (a
// preflight, which carries no operation) passes. A POST to a GraphQL read
// path passes only when every operation is a query. Everything else is
// blocked, to any origin.
func readSafe(task *Task, r *network.Request) bool {
	m := strings.ToUpper(r.Method)
	u, err := url.Parse(r.URL)
	if err != nil {
		return false
	}
	if methodOverridden(r, u) {
		return false // a server honoring the override would see another method
	}
	gql := task.graphQLPath(u.Path)
	switch m {
	case "OPTIONS":
		return true
	case "GET", "HEAD":
		if !gql {
			return true
		}
		params := u.Query()
		for k := range params {
			if k != "query" && strings.EqualFold(k, "query") {
				return false // a server could read this spelling instead
			}
		}
		if len(params["query"]) != 1 {
			return false // absent, or repeated so a server could pick another copy
		}
		q, _ := json.Marshal(map[string]string{"query": params.Get("query")})
		return graphQLReadOnly(q)
	case "POST":
		// A URL query string is refused outright: some servers read query,
		// operationName, extensions or a persisted-query id from the URL
		// before the body, so the body alone cannot show what will run.
		return gql && u.RawQuery == "" && graphQLReadOnly(postBody(r))
	}
	return false
}

// methodOverridden reports whether a request asks the server to treat it as a
// different method, through a header or a _method parameter that frameworks
// such as Rails, Laravel and Express method-override honor.
func methodOverridden(r *network.Request, u *url.URL) bool {
	for k := range r.Headers {
		switch strings.ToLower(k) {
		case "x-http-method-override", "x-http-method", "x-method-override":
			return true
		}
	}
	for k := range u.Query() {
		if strings.EqualFold(k, "_method") {
			return true
		}
	}
	return false
}

func postShape(r *network.Request) map[string]any {
	b := postBody(r)
	if b == nil {
		return nil
	}
	return shapeOf(b, false)
}

// postBody is the request body Chrome reported, or nil when it has none or
// did not include it (large bodies can be left out).
func postBody(r *network.Request) []byte {
	if r == nil || !r.HasPostData || len(r.PostDataEntries) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, e := range r.PostDataEntries {
		buf.Write(e.Bytes)
	}
	return buf.Bytes()
}

// graphQLOps returns the operations of a request to a GraphQL read path, or
// nil for any other request or a body that does not parse.
func graphQLOps(task *Task, method, path string, r *network.Request) []gqlRequest {
	if method != "POST" || !task.graphQLPath(path) {
		return nil
	}
	ops, ok := parseGraphQL(postBody(r))
	if !ok {
		return nil
	}
	return ops
}

func shapeOf(b []byte, b64 bool) map[string]any {
	if b64 {
		d, err := base64.StdEncoding.DecodeString(string(b))
		if err == nil {
			b = d
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return Shape(v)
}

func containsInt(a []int, v int) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

func containsStr(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

// ErrNoObservations reports an exploration that saw nothing usable.
var ErrNoObservations = errors.New("no observations")
