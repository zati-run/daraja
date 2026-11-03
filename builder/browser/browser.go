// Package browser drives a host-side Chrome for exploration and session
// leasing. It runs only on a host that can start Chrome; the deployable
// runtime never imports it.
//
// The browser uses a dedicated, persistent profile directory per source, so
// a sign-in that needed a human step (such as an emailed passcode) is not
// repeated on every run. The profile holds the user's session and must stay
// in approved custody (Daraja keeps it under the gitignored private/ tree).
package browser

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/session"
)

// Options configure a browser.
type Options struct {
	ProfileDir string
	Headless   bool
	Logger     *slog.Logger
	// HumanTimeout bounds how long a human step may take. Zero means 10 minutes.
	HumanTimeout time.Duration
	// Notify is called when a human step starts, with the step's text.
	Notify func(string)
	// Credentials are typed by browser sign-in recipes. They are never logged.
	Credentials map[string]string
	// StepTimeout bounds one browser.steps step. Zero means 15 seconds.
	StepTimeout time.Duration
}

// Browser is one Chrome instance with one tab.
type Browser struct {
	Ctx    context.Context
	cancel func()
	opts   Options
	log    *slog.Logger

	// human is the page path prefix of a sign-in step left to a person, or
	// nil. While the page is still on it, nothing navigates or fetches.
	human atomic.Pointer[string]
}

// humanActive reports whether a person is still on a pending sign-in step.
// It only reads the page's location; when the person has moved on, it
// clears the pending step.
func (b *Browser) humanActive() bool {
	p := b.human.Load()
	if p == nil {
		return false
	}
	cur := b.path()
	if cur == "" || strings.HasPrefix(cur, *p) {
		// An unreadable location (for example, mid-navigation after the
		// person submits) keeps the guard: it fails closed.
		return true
	}
	b.human.Store(nil)
	return false
}

// Open starts Chrome with the profile.
func Open(parent context.Context, o Options) (*Browser, error) {
	if o.ProfileDir == "" {
		return nil, errors.New("browser: a profile directory is required")
	}
	if err := os.MkdirAll(o.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.HumanTimeout == 0 {
		o.HumanTimeout = 10 * time.Minute
	}
	if o.StepTimeout == 0 {
		o.StepTimeout = 15 * time.Second
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", o.Headless),
		chromedp.UserDataDir(o.ProfileDir),
	)
	actx, cancelA := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancelC := chromedp.NewContext(actx)
	if err := chromedp.Do(ctx, chromedp.Navigate("about:blank")); err != nil {
		cancelC()
		cancelA()
		return nil, fmt.Errorf("browser: start: %w", err)
	}
	return &Browser{Ctx: ctx, cancel: func() { cancelC(); cancelA() }, opts: o, log: o.Logger}, nil
}

// Close stops Chrome. The process is killed first, so that nothing paused by a
// CDP session is released when the connection drops. The profile stays on disk.
func (b *Browser) Close() {
	if c := chromedp.FromContext(b.Ctx); c != nil && c.Browser != nil {
		if p := c.Browser.Process(); p != nil {
			_ = p.Kill() // already gone on a second Close
		}
	}
	b.cancel()
}

func (b *Browser) path() string {
	loc, err := chromedp.Run(b.Ctx, chromedp.Location())
	if err != nil {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	return u.Path
}

// pathWithin is path, bounded: a page still waiting for its server's answer
// doesn't report its location, which would otherwise block without limit.
func (b *Browser) pathWithin(d time.Duration) string {
	ctx, cancel := context.WithTimeout(b.Ctx, d)
	defer cancel()
	loc, err := chromedp.Run(ctx, chromedp.Location())
	if err != nil {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	return u.Path
}

// FetchJSON runs a same-origin GET inside the page and returns status and body.
func (b *Browser) FetchJSON(ctx context.Context, path string) (int, []byte, error) {
	js := fmt.Sprintf(`fetch(%q, {credentials: "same-origin", headers: {"Accept": "application/json"}}).then(async r => JSON.stringify({s: r.status, b: await r.text()}))`, path)
	ctx, cancel := context.WithTimeout(b.Ctx, 30*time.Second)
	defer cancel()
	out, err := chromedp.Run(ctx, chromedp.Evaluate[string](js, chromedp.EvalAwaitPromise))
	if err != nil {
		return 0, nil, err
	}
	var r struct {
		S int    `json:"s"`
		B string `json:"b"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return 0, nil, err
	}
	return r.S, []byte(r.B), nil
}

// SignedIn runs the integration's signed-in check inside the page. The page
// must be on the source origin.
func (b *Browser) SignedIn(ctx context.Context, in *integration.Compiled) (bool, error) {
	chk := in.Session.SignedIn
	if chk == nil {
		return false, errors.New("the integration declares no signed_in check")
	}
	status, body, err := b.FetchJSON(ctx, chk.Request.Path)
	if err != nil {
		return false, err
	}
	if status != 200 {
		return false, nil
	}
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return false, nil
	}
	v, ok := integration.Lookup(doc, chk.JSONPointer)
	return ok && (!chk.NotNull || v != nil), nil
}

// EnsureSignedIn opens the origin and, if the profile is not signed in,
// runs the integration's browser sign-in recipe. Credential values are typed
// into the page and never logged.
func (b *Browser) EnsureSignedIn(ctx context.Context, in *integration.Compiled, creds map[string]string) error {
	return b.ensureSignedIn(ctx, in, creds, false)
}

// errHumanPending reports a human step left visible for a person.
var errHumanPending = errors.New("a sign-in step is waiting for a person")

func (b *Browser) ensureSignedIn(ctx context.Context, in *integration.Compiled, creds map[string]string, nonBlocking bool) error {
	origin := in.Source.Origin
	if err := chromedp.Do(b.Ctx, chromedp.Navigate(origin+in.Session.SignedIn.Request.Path)); err != nil {
		return fmt.Errorf("open origin: %w", err)
	}
	if err := b.waitChallenge(ctx); err != nil {
		return err
	}
	if ok, err := b.SignedIn(ctx, in); err == nil && ok {
		b.log.Info("profile already signed in", "integration", in.ID)
		return nil
	}
	vars := integration.Vars{Credential: creds}
	for i, st := range in.Session.BrowserSignIn {
		if st.When != "" && !strings.HasPrefix(b.path(), st.When) {
			continue
		}
		var err error
		switch {
		case st.Navigate != "":
			err = chromedp.Do(b.Ctx, chromedp.Navigate(origin+st.Navigate))
			if err == nil {
				err = b.waitChallenge(ctx)
			}
		case st.Fill != "":
			var v string
			v, err = vars.RenderString(st.Value, nil)
			if err == nil {
				sctx, cancel := context.WithTimeout(b.Ctx, 60*time.Second)
				err = chromedp.Do(sctx, chromedp.WaitVisible(chromedp.CSS(st.Fill)), chromedp.SendKeys(chromedp.CSS(st.Fill), v))
				cancel()
			}
		case st.Click != "":
			sctx, cancel := context.WithTimeout(b.Ctx, 60*time.Second)
			err = chromedp.Do(sctx, chromedp.Click(chromedp.CSS(st.Click)))
			cancel()
		case st.WaitPathNot != "":
			err = b.waitPathNot(ctx, st.WaitPathNot, 60*time.Second)
		case st.Human != "":
			if b.opts.Headless {
				return fmt.Errorf("sign-in step %d needs a person (%q) but the browser is headless", i, st.Human)
			}
			b.log.Warn("waiting for a person to finish a sign-in step", "integration", in.ID, "step", i)
			if b.opts.Notify != nil {
				b.opts.Notify(st.Human)
			}
			if nonBlocking {
				prefix := st.When
				if prefix == "" {
					prefix = b.path()
				}
				b.human.Store(&prefix)
				return errHumanPending
			}
			prefix := st.When
			if prefix == "" {
				prefix = b.path()
			}
			err = b.waitPathNot(ctx, prefix, b.opts.HumanTimeout)
		}
		if err != nil {
			return fmt.Errorf("sign-in step %d: %w", i, err)
		}
		b.log.Info("sign-in step done", "step", i, "path", b.path())
	}
	if err := chromedp.Do(b.Ctx, chromedp.Navigate(origin+in.Session.SignedIn.Request.Path)); err != nil {
		return err
	}
	ok, err := b.SignedIn(ctx, in)
	if err != nil {
		return fmt.Errorf("signed-in check: %w", err)
	}
	if !ok {
		return fmt.Errorf("sign-in finished but the signed-in check failed (page path %s)", b.path())
	}
	b.log.Info("signed in", "integration", in.ID)
	return nil
}

// waitChallenge waits while a recognisable interstitial browser challenge
// is shown. It does not solve challenges; a visible browser lets the page
// complete its own check.
func (b *Browser) waitChallenge(ctx context.Context) error {
	return waitChallengeOn(b.Ctx, ctx)
}

// waitChallengeOn asks the page through page, a browser context, and gives up
// when ctx ends.
func waitChallengeOn(page, ctx context.Context) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		title, err := chromedp.Run(page, chromedp.Title())
		if err != nil {
			return err
		}
		if !strings.Contains(title, "Just a moment") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("a browser challenge did not complete (a headless browser may be refused; try a visible one)")
}

func (b *Browser) waitPathNot(ctx context.Context, prefix string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p := b.pathWithin(time.Second); p != "" && !strings.HasPrefix(p, prefix) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("page stayed on %s for %s", prefix, d)
}

// Lease returns the cookies the browser holds for the origin and the
// browser's user agent.
func (b *Browser) Lease(ctx context.Context, origin string, ttl time.Duration) (*session.Lease, error) {
	res, err := chromedp.Call(b.Ctx, network.GetCookies, network.GetCookiesParams{URLs: []string{origin}})
	if err != nil {
		return nil, err
	}
	ua, err := chromedp.Run(b.Ctx, chromedp.Evaluate[string](`navigator.userAgent`))
	if err != nil {
		return nil, err
	}
	l := &session.Lease{UserAgent: ua, ExpiresAt: time.Now().Add(ttl).UTC()}
	for _, c := range res.Cookies {
		l.Cookies = append(l.Cookies, session.LeaseCookie{Name: c.Name, Value: c.Value})
	}
	return l, nil
}

// Recover restores a signed-in session for the capability without blocking
// on a person. With a sign-in recipe it signs in again using the binding's
// credentials; a human step (or a missing recipe) leaves the page visible on
// the sign-in screen, notifies, and returns capability.ErrNeedsHuman.
func (b *Browser) Recover(ctx context.Context, in *integration.Compiled) error {
	if b.humanActive() {
		return capability.ErrHumanActive
	}
	if len(in.Session.BrowserSignIn) == 0 || b.opts.Credentials == nil {
		if err := chromedp.Do(b.Ctx, chromedp.Navigate(in.Source.Origin+"/")); err != nil {
			return fmt.Errorf("open origin: %w", err)
		}
		if b.opts.Notify != nil {
			b.opts.Notify("Sign in to " + in.Source.Origin + " in the Daraja browser window.")
		}
		return fmt.Errorf("%w: no sign-in recipe or credentials for %s", capability.ErrNeedsHuman, in.ID)
	}
	err := b.ensureSignedIn(ctx, in, b.opts.Credentials, true)
	if errors.Is(err, errHumanPending) {
		return fmt.Errorf("%w: %v", capability.ErrNeedsHuman, err)
	}
	return err
}

type pageAnswer struct {
	NotSent string            `json:"ns"`
	Err     string            `json:"err"`
	Status  int               `json:"s"`
	Redir   bool              `json:"rd"`
	Headers map[string]string `json:"h"`
	Body    string            `json:"b"`
}

// fetchJS runs one request inside the page. It reports "ns" when nothing
// left the page and "err" when the request may have been sent.
const fetchJS = `(async (req) => {
  if (location.origin !== new URL(req.url).origin) return JSON.stringify({ns: "the page is not on the source origin"});
  const init = {method: req.method, headers: req.headers, credentials: "same-origin", cache: "no-store", redirect: "manual"};
  if (req.body !== null) init.body = Uint8Array.from(atob(req.body), c => c.charCodeAt(0));
  let r;
  try { r = await fetch(req.url, init); } catch (e) { return JSON.stringify({err: String(e)}); }
  if (r.type === "opaqueredirect") return JSON.stringify({s: 0, rd: true, h: {}, b: ""});
  if (new URL(r.url).origin !== location.origin) return JSON.stringify({err: "the response came from another origin"});
  const h = {};
  r.headers.forEach((v, k) => { h[k] = v; });
  let b = "";
  try {
    const buf = new Uint8Array(await r.arrayBuffer());
    if (buf.length > %d) return JSON.stringify({s: r.status, h, err: "response exceeds 8 MiB"});
    let bin = "";
    for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode.apply(null, buf.subarray(i, i + 0x8000));
    b = btoa(bin);
  } catch (e) { return JSON.stringify({s: r.status, h, err: String(e)}); }
  return JSON.stringify({s: r.status, h, b});
})(%s)`

// Fetch runs a same-origin request inside the page, with the page's own
// cookies and signing. It implements capability.Page.
func (b *Browser) Fetch(ctx context.Context, in *integration.Compiled, req capability.FetchRequest) (capability.FetchResult, error) {
	if b.humanActive() {
		return capability.FetchResult{}, fmt.Errorf("%w: %w", capability.ErrNotSent, capability.ErrHumanActive)
	}
	if err := b.onOrigin(ctx, in.Source.Origin); err != nil {
		return capability.FetchResult{}, fmt.Errorf("%w: %v", capability.ErrNotSent, err)
	}
	reqURL, fragment := req.URL, ""
	if req.WantLocation {
		// A random fragment never reaches the server, but the network events
		// carry it, so they can't be confused with another request to this URL.
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return capability.FetchResult{}, fmt.Errorf("%w: %v", capability.ErrNotSent, err)
		}
		fragment = "#" + hex.EncodeToString(nonce[:])
		reqURL += fragment
	}
	payload := map[string]any{"method": req.Method, "url": reqURL, "headers": req.Headers, "body": nil}
	if req.Body != nil {
		payload["body"] = base64.StdEncoding.EncodeToString(req.Body)
	}
	pj, err := json.Marshal(payload)
	if err != nil {
		return capability.FetchResult{}, fmt.Errorf("%w: %v", capability.ErrNotSent, err)
	}
	ectx, cancel := context.WithTimeout(b.Ctx, capability.InPageFetchTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	var redirect func() (int, string)
	if req.WantLocation {
		redirect, err = b.watchRedirect(ectx, req.Method, req.URL, fragment)
		if err != nil {
			return capability.FetchResult{}, fmt.Errorf("%w: %v", capability.ErrNotSent, err)
		}
	}
	out, err := chromedp.Run(ectx, chromedp.Evaluate[string](fmt.Sprintf(fetchJS, capability.MaxBody, pj), chromedp.EvalAwaitPromise))
	if err != nil {
		return capability.FetchResult{}, fmt.Errorf("in-page request: %w", err)
	}
	var a pageAnswer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		return capability.FetchResult{}, fmt.Errorf("in-page answer: %w", err)
	}
	if a.NotSent != "" {
		return capability.FetchResult{}, fmt.Errorf("%w: %s", capability.ErrNotSent, a.NotSent)
	}
	if a.Redir {
		// Operations never follow redirects; the runtime decides what a
		// redirect means for a write.
		res := capability.FetchResult{Redirected: true}
		if redirect != nil {
			res.Status, res.Location = redirect()
		}
		return res, nil
	}
	if a.Err == capability.ErrBodyTooLarge.Error() {
		return capability.FetchResult{}, capability.ErrBodyTooLarge
	}
	if a.Err != "" {
		// Sent, but the answer could not be read.
		return capability.FetchResult{}, errors.New(a.Err)
	}
	body, err := base64.StdEncoding.DecodeString(a.Body)
	if err != nil {
		return capability.FetchResult{}, fmt.Errorf("in-page body: %w", err)
	}
	h := http.Header{}
	for k, v := range a.Headers {
		h.Set(k, v)
	}
	return capability.FetchResult{Status: a.Status, Headers: h, Body: body}, nil
}

// watchRedirect reports the status and Location of the redirect answering the
// request with this method, URL and fragment. A page's fetch can't read them, so
// they come from the browser's own network events. The result is 0 and "" when
// they didn't arrive in time. The caller then treats the redirect as undeclared.
func (b *Browser) watchRedirect(ctx context.Context, method, target, fragment string) (func() (int, string), error) {
	sent := chromedp.Events(ctx, network.RequestWillBeSent)
	extra := chromedp.Events(ctx, network.ResponseReceivedExtraInfo)
	if _, err := chromedp.Call(ctx, network.Enable, network.EnableParams{}); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	ids := map[network.RequestID]bool{}
	answers := map[network.RequestID]*network.EventResponseReceivedExtraInfo{}
	go func() {
		for ev, err := range sent {
			if err != nil {
				return
			}
			if ev.Request != nil && ev.Request.Method == method && ev.Request.URL == target && ev.Request.URLFragment == fragment {
				mu.Lock()
				ids[ev.RequestID] = true
				mu.Unlock()
			}
		}
	}()
	go func() {
		for ev, err := range extra {
			if err != nil {
				return
			}
			mu.Lock()
			answers[ev.RequestID] = &ev
			mu.Unlock()
		}
	}()
	read := func() (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(ids) != 1 {
			return 0, "", false
		}
		for id := range ids {
			if a := answers[id]; a != nil {
				for k, v := range a.Headers {
					if strings.EqualFold(k, "Location") {
						return int(a.StatusCode), fmt.Sprint(v), true
					}
				}
				return int(a.StatusCode), "", true
			}
		}
		return 0, "", false
	}
	return func() (int, string) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if status, loc, ok := read(); ok || time.Now().After(deadline) {
				return status, loc
			}
			select {
			case <-ctx.Done():
				return 0, ""
			case <-time.After(20 * time.Millisecond):
			}
		}
	}, nil
}

// errDrift marks a page that no longer matches a browser.steps plan.
var errDrift = errors.New("the page no longer matches the plan")

// errPlan marks a failure whose message Daraja wrote, so it may be reported.
var errPlan = errors.New("the plan failed")

// stepError keeps page text out of what a failed step reports. A script error
// can carry anything the page chooses, and the message reaches logs and
// receipts. Drift messages name only what the artifact declares.
func stepError(err error) error {
	switch {
	case errors.Is(err, errDrift), errors.Is(err, errPlan):
		return err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return errors.New("the step timed out")
	}
	return errors.New("the browser call failed")
}

// RunSteps implements capability.Page. It stops at the first failure. It uses
// real input events, so the application sees what a person's input produces.
func (b *Browser) RunSteps(ctx context.Context, in *integration.Compiled, steps []integration.Step) capability.StepsResult {
	var res capability.StepsResult
	fail := func(i int, err error) capability.StepsResult {
		res.Err = fmt.Errorf("step %d: %w", i, stepError(err))
		res.Drift = errors.Is(err, errDrift)
		return res
	}
	if b.humanActive() {
		res.Err = capability.ErrHumanActive
		return res
	}
	for i, st := range steps {
		sctx, cancel := context.WithTimeout(b.Ctx, b.opts.StepTimeout)
		stop := context.AfterFunc(ctx, cancel)
		err := func() error {
			if err := b.checkPage(sctx, in.Source.Origin, st); err != nil {
				return err
			}
			if st.Commit {
				// From here on, the effect may exist.
				res.Committed = true
			}
			return b.runStep(sctx, in.Source.Origin, st)
		}()
		stop()
		cancel()
		if err != nil {
			return fail(i, err)
		}
	}
	rctx, cancel := context.WithTimeout(b.Ctx, b.opts.StepTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	// One evaluation, in one document: the URL and the page can't disagree.
	final, err := evalIsolated(rctx, `location.href + "\n" + document.documentElement.outerHTML`)
	if err != nil {
		return fail(len(steps), err)
	}
	loc, html, ok := strings.Cut(final, "\n")
	if !ok {
		return fail(len(steps), fmt.Errorf("%w: the final page could not be read", errPlan))
	}
	if !sameOrigin(loc, in.Source.Origin) {
		return fail(len(steps), fmt.Errorf("%w: the plan ended on another origin", errDrift))
	}
	if len(html) > capability.MaxBody {
		return fail(len(steps), fmt.Errorf("%w: the final page exceeds 8 MiB", errPlan))
	}
	res.Body, res.Location = []byte(html), loc
	return res
}

func sameOrigin(loc, origin string) bool {
	u, err := url.Parse(loc)
	if err != nil {
		return false
	}
	return u.Scheme+"://"+u.Host == origin
}

// evalIsolated runs a script in a fresh isolated world of the current document.
// The world shares the page's DOM but none of its scripts, so a page that
// replaces a built-in can't change what the script sees or returns. The world
// ends with its document, so a navigation makes the call fail rather than run
// on another page.
func evalIsolated(ctx context.Context, script string) (string, error) {
	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return "", err
	}
	world, err := chromedp.Call(ctx, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{FrameID: tree.FrameTree.Frame.ID, WorldName: "daraja"})
	if err != nil {
		return "", err
	}
	byValue := true
	res, err := chromedp.Call(ctx, cdpruntime.Evaluate, cdpruntime.EvaluateParams{Expression: script, ContextID: world.ExecutionContextID, ReturnByValue: &byValue})
	if err != nil {
		return "", err
	}
	if res.ExceptionDetails != nil {
		return "", errors.New("the script failed")
	}
	var out string
	if err := json.Unmarshal(res.Result.Value, &out); err != nil {
		return "", err
	}
	return out, nil
}

// pageCheck is the part of every step's script that reads the origin and the
// assertions. It answers with a fixed code, so no page text can come back.
const pageCheck = `if (e.origin && location.origin !== e.origin) return "origin";
  const bad = (e.expect || []).findIndex(x => ![...document.querySelectorAll(x.css)].some(n => !x.text || n.textContent.trim() === x.text));
  if (bad >= 0) return "expect:" + bad;`

// describe turns a script's code into what a failure reports, using only what
// the artifact declares.
func describe(code string, st integration.Step, css string) string {
	switch code {
	case "origin":
		return "the page is on another origin"
	case "missing":
		return css + " did not appear"
	case "field":
		return css + " is not a field that can be filled"
	case "value":
		return css + " did not take the value"
	case "option":
		return css + " has no such option"
	case "covered":
		return css + " is covered"
	}
	if rest, ok := strings.CutPrefix(code, "expect:"); ok {
		for i, e := range st.Expect {
			if rest == fmt.Sprint(i) {
				return e.CSS + " is not as expected"
			}
		}
	}
	return "the page did not answer as expected"
}

// checkPage runs before a step acts, also after the commit step: the page is on
// the source origin, and the step's assertions hold. It retries until the step's
// timeout, because a page may still be rendering.
func (b *Browser) checkPage(ctx context.Context, origin string, st integration.Step) error {
	if st.Navigate != "" && len(st.Expect) == 0 {
		return nil
	}
	want := origin
	if st.Navigate != "" {
		want = "" // A navigate leaves the current page, wherever it is.
	}
	arg, err := json.Marshal(map[string]any{"origin": want, "expect": st.Expect})
	if err != nil {
		return err
	}
	script := fmt.Sprintf("((e) => {\n  %s\n  return \"ok\";\n})(%s)", pageCheck, arg)
	last := "the page did not answer"
	for {
		code, err := evalIsolated(ctx, script)
		if err == nil { // Otherwise a navigation is in progress; look again.
			if code == "ok" {
				return nil
			}
			last = describe(code, st, "")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s", errDrift, last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// actScript checks the origin and the step's assertions, finds the first shown
// element, and acts on it, all synchronously in one evaluation, so nothing on
// the page can change between the check and the action. It targets the element
// itself, so a moved focus or a scroll can't send the action elsewhere. A click
// needs the element's center to land on it, so a covered element isn't clicked.
// With e.act empty it only looks, and acts on nothing.
const actScript = `((e) => {
  ` + pageCheck + `
  const shown = (n) => {
    const r = n.getBoundingClientRect(), s = getComputedStyle(n);
    return r.width > 0 && r.height > 0 && s.visibility === "visible" && !n.disabled;
  };
  const n = [...document.querySelectorAll(e.css)].find(n => shown(n) && (!e.text || n.textContent.includes(e.text)));
  if (!n) return "missing";
  const fire = () => {
    n.dispatchEvent(new Event("input", {bubbles: true}));
    n.dispatchEvent(new Event("change", {bubbles: true}));
  };
  if (e.act === "fill") {
    const nope = ["checkbox", "radio", "file", "hidden", "submit", "button", "reset", "image", "range", "color"];
    const field = n instanceof HTMLTextAreaElement || (n instanceof HTMLInputElement && !nope.includes(n.type));
    if (!field || n.readOnly) return "field";
    const norm = (s) => s.replace(/\r\n?/g, "\n");
    n.focus();
    n.value = e.value;
    if (norm(n.value) !== norm(e.value)) return "value";
    fire();
  } else if (e.act === "select") {
    if (!n.options || ![...n.options].some(o => o.value === e.value)) return "option";
    n.value = e.value;
    fire();
  } else if (e.act === "click") {
    n.scrollIntoView({block: "center", behavior: "instant"});
    const r = n.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    if (!hit || !n.contains(hit)) return "covered";
    n.click();
  }
  return "ok";
})(%s)`

// actOnPage waits until the step's target is ready, then acts once. Only a look
// is retried. If the action's evaluation fails, it may have run, so it is never
// repeated; the failure stands, and after a commit step that's uncertain.
func actOnPage(sctx context.Context, origin, act string, st integration.Step, css string) error {
	script := func(act string) (string, error) {
		arg, err := json.Marshal(map[string]any{"origin": origin, "act": act, "css": css, "text": st.Text, "value": st.Value, "expect": st.Expect})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(actScript, arg), nil
	}
	look, err := script("")
	if err != nil {
		return err
	}
	do, err := script(act)
	if err != nil {
		return err
	}
	why := css + " did not appear"
	for {
		code, err := evalIsolated(sctx, look)
		if err == nil {
			if code == "ok" {
				if act == "" {
					return nil
				}
				if code, err = evalIsolated(sctx, do); err != nil {
					return err
				}
				if code == "ok" {
					return nil
				}
				if code == "value" {
					return fmt.Errorf("%w: %s", errDrift, describe(code, st, css))
				}
			}
			why = describe(code, st, css)
		}
		select {
		case <-sctx.Done():
			return fmt.Errorf("%w: %s", errDrift, why)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// runStep performs one action. A selector that never appears is drift.
func (b *Browser) runStep(sctx context.Context, origin string, st integration.Step) error {
	switch {
	case st.Navigate != "":
		if err := chromedp.Do(sctx, chromedp.Navigate(origin+st.Navigate)); err != nil {
			return err
		}
		return waitChallengeOn(sctx, sctx)
	case st.Fill != "":
		return actOnPage(sctx, origin, "fill", st, st.Fill)
	case st.Select != "":
		return actOnPage(sctx, origin, "select", st, st.Select)
	case st.Click != "":
		return actOnPage(sctx, origin, "click", st, st.Click)
	case st.WaitFor != "":
		return actOnPage(sctx, origin, "", st, st.WaitFor)
	case st.WaitPathNot != "":
		d := b.opts.StepTimeout
		if err := b.waitPathNot(sctx, st.WaitPathNot, d); err != nil {
			return fmt.Errorf("%w: %v", errDrift, err)
		}
		return nil
	}
	return fmt.Errorf("%w: a step has no action", errPlan)
}

// onOrigin navigates to the origin when the page is elsewhere.
func (b *Browser) onOrigin(ctx context.Context, origin string) error {
	cur, err := chromedp.Run(b.Ctx, chromedp.Evaluate[string](`location.origin`))
	if err == nil && cur == origin {
		return nil
	}
	nctx, cancel := context.WithTimeout(b.Ctx, 45*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if err := chromedp.Do(nctx, chromedp.Navigate(origin+"/")); err != nil {
		return fmt.Errorf("open origin: %w", err)
	}
	if err := b.waitChallenge(ctx); err != nil {
		return err
	}
	cur, err = chromedp.Run(b.Ctx, chromedp.Evaluate[string](`location.origin`))
	if err != nil {
		return err
	}
	if cur != origin {
		return fmt.Errorf("the page went to %s instead of the source origin", cur)
	}
	return nil
}
