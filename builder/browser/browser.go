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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
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
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		title, err := chromedp.Run(b.Ctx, chromedp.Title())
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
		if p := b.path(); p != "" && !strings.HasPrefix(p, prefix) {
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
	payload := map[string]any{"method": req.Method, "url": req.URL, "headers": req.Headers, "body": nil}
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
		return capability.FetchResult{Redirected: true}, nil
	}
	if a.Err != "" {
		// Sent, but the answer could not be read (for example, too large).
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
