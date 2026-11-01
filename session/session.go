// Package session acquires and checks user sessions for an integration's
// source application. A session is the user's own login, held in memory by
// the process that uses it. Daraja never derives authority from a session:
// the source application decides what the session may do.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zati-run/daraja/integration"
)

// Session is an acquired user session.
type Session struct {
	Cookies   []*http.Cookie
	UserAgent string
	Values    map[string]string // extracted session values, such as a CSRF token
	Acquired  time.Time
	Expires   time.Time
}

// Apply adds the session's cookies, user agent and headers to a request.
func (s *Session) Apply(req *http.Request, headers map[string]string) error {
	for _, c := range s.Cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	v := integration.Vars{Session: s.Values}
	for k, t := range headers {
		val, err := v.RenderString(t, nil)
		if err != nil {
			return fmt.Errorf("session header %s: %w", k, err)
		}
		req.Header.Set(k, val)
	}
	return nil
}

// Binding is the credential material for one principal and integration.
// Values are never logged.
type Binding struct {
	Values map[string]string `json:"values,omitempty"`
	Lease  *LeaseConfig      `json:"lease,omitempty"`
	// Synthetic marks test credentials for a synthetic app. Only synthetic
	// bindings may live where file custody cannot be verified, such as
	// inside a unikernel image.
	Synthetic bool `json:"synthetic,omitempty"`
}

// LeaseConfig locates a host-side lease broker.
type LeaseConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ErrUnavailable reports that no valid session could be acquired.
var ErrUnavailable = errors.New("session unavailable")

// Acquire obtains a session using the integration's provider and checks it.
func Acquire(ctx context.Context, client *http.Client, in *integration.Compiled, b Binding) (*Session, error) {
	var (
		s   *Session
		err error
	)
	switch in.Session.Provider {
	case integration.ProviderNone:
		s = &Session{}
	case integration.ProviderHTTPForm:
		s, err = signInHTTP(ctx, client, in, b)
	case integration.ProviderBrowserLease:
		s, err = fetchLease(ctx, client, in, b)
	default:
		err = fmt.Errorf("provider %q not supported", in.Session.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if s.Values == nil {
		s.Values = map[string]string{}
	}
	if s.Acquired.IsZero() {
		s.Acquired = time.Now()
	}
	ttl := time.Duration(in.Session.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if s.Expires.IsZero() || s.Acquired.Add(ttl).Before(s.Expires) {
		s.Expires = s.Acquired.Add(ttl)
	}
	if err := finish(ctx, client, in, s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return s, nil
}

// finish runs session extraction and the signed-in check.
func finish(ctx context.Context, client *http.Client, in *integration.Compiled, s *Session) error {
	for name, vs := range in.Session.Extract {
		resp, body, err := simpleGet(ctx, client, in.Source.Origin, vs.Request.Path, s)
		if err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		val, err := ReadValue(resp, body, vs)
		if err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		s.Values[name] = val
	}
	if chk := in.Session.SignedIn; chk != nil {
		resp, body, err := simpleGet(ctx, client, in.Source.Origin, chk.Request.Path, s)
		if err != nil {
			return fmt.Errorf("signed-in check: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("signed-in check: status %d", resp.StatusCode)
		}
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return fmt.Errorf("signed-in check: response is not JSON")
		}
		v, ok := integration.Lookup(doc, chk.JSONPointer)
		if !ok || (chk.NotNull && v == nil) {
			return errors.New("signed-in check: the session is not signed in")
		}
	}
	return nil
}

func simpleGet(ctx context.Context, client *http.Client, origin, path string, s *Session) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if err := s.Apply(req, nil); err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp, body, err
}

var inputTagRE = regexp.MustCompile(`(?is)<input\b[^>]*>`)
var attrRE = regexp.MustCompile(`(?is)\b(name|value)\s*=\s*("([^"]*)"|'([^']*)')`)
var metaTagRE = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
var metaAttrRE = regexp.MustCompile(`(?is)\b(name|content)\s*=\s*("([^"]*)"|'([^']*)')`)

// ReadValue reads one value from a response as a ValueSource describes.
func ReadValue(resp *http.Response, body []byte, vs integration.ValueSource) (string, error) {
	switch {
	case vs.Header != "":
		v := resp.Header.Get(vs.Header)
		if v == "" {
			return "", fmt.Errorf("header %s absent", vs.Header)
		}
		return v, nil
	case vs.JSONPointer != "":
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return "", errors.New("response is not JSON")
		}
		v, ok := integration.Lookup(doc, vs.JSONPointer)
		s, isStr := v.(string)
		if !ok || !isStr || s == "" {
			return "", fmt.Errorf("no string at %s", vs.JSONPointer)
		}
		return s, nil
	case vs.HTMLInput != "":
		for _, tag := range inputTagRE.FindAll(body, -1) {
			var name, value string
			for _, m := range attrRE.FindAllSubmatch(tag, -1) {
				val := string(m[3]) + string(m[4])
				if strings.EqualFold(string(m[1]), "name") {
					name = val
				} else {
					value = val
				}
			}
			if name == vs.HTMLInput {
				return value, nil
			}
		}
		return "", fmt.Errorf("input %q not found", vs.HTMLInput)
	case vs.HTMLMeta != "":
		if v, ok := HTMLMeta(body, vs.HTMLMeta); ok {
			return v, nil
		}
		return "", fmt.Errorf("meta %q not found", vs.HTMLMeta)
	}
	return "", errors.New("empty value source")
}

// HTMLMeta returns the content attribute of the first <meta name=...> tag.
func HTMLMeta(body []byte, name string) (string, bool) {
	for _, tag := range metaTagRE.FindAll(body, -1) {
		var n, content string
		for _, m := range metaAttrRE.FindAllSubmatch(tag, -1) {
			val := string(m[3]) + string(m[4])
			switch strings.ToLower(string(m[1])) {
			case "name":
				n = val
			case "content":
				content = val
			}
		}
		if n == name {
			return html.UnescapeString(content), true
		}
	}
	return "", false
}

// OriginClient returns a copy of client that refuses redirects off origin.
func OriginClient(client *http.Client, origin string) *http.Client {
	c := *client
	o, _ := url.Parse(origin)
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != o.Scheme || req.URL.Host != o.Host {
			return fmt.Errorf("redirect off origin to %s://%s refused", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &c
}

func signInHTTP(ctx context.Context, base *http.Client, in *integration.Compiled, b Binding) (*Session, error) {
	jar, _ := cookiejar.New(nil)
	client := OriginClient(base, in.Source.Origin)
	client.Jar = jar
	extracted := map[string]string{}
	for i, st := range in.Session.SignIn {
		vars := integration.Vars{Credential: b.Values, Extracted: extracted}
		var body io.Reader
		if len(st.Form) > 0 {
			f := url.Values{}
			for k, t := range st.Form {
				v, err := vars.RenderString(t, nil)
				if err != nil {
					return nil, fmt.Errorf("sign-in step %d: %w", i, err)
				}
				f.Set(k, v)
			}
			body = strings.NewReader(f.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, st.Method, in.Source.Origin+st.Path, body)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("sign-in step %d: %w", i, err)
		}
		rb, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("sign-in step %d: %w", i, err)
		}
		if len(st.ExpectStatus) > 0 && !contains(st.ExpectStatus, resp.StatusCode) {
			return nil, fmt.Errorf("sign-in step %d: status %d%s", i, resp.StatusCode, challengeHint(resp, rb))
		}
		for name, vs := range st.Extract {
			v, err := ReadValue(resp, rb, vs)
			if err != nil {
				return nil, fmt.Errorf("sign-in step %d extract %s: %w%s", i, name, err, challengeHint(resp, rb))
			}
			extracted[name] = v
		}
	}
	u, _ := url.Parse(in.Source.Origin)
	return &Session{Cookies: jar.Cookies(u)}, nil
}

// challengeHint names a recognisable bot challenge so the failure is not
// mistaken for bad credentials.
func challengeHint(resp *http.Response, body []byte) string {
	if resp.StatusCode == http.StatusForbidden && (bytes.Contains(body, []byte("_cf_chl_opt")) || bytes.Contains(body, []byte("cf-turnstile"))) {
		return " (a browser challenge blocked HTTP sign-in; this source needs the browser_lease provider)"
	}
	return ""
}

func contains(a []int, v int) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

// Lease is what a host-side broker returns.
type Lease struct {
	Cookies   []LeaseCookie `json:"cookies"`
	UserAgent string        `json:"user_agent,omitempty"`
	ExpiresAt time.Time     `json:"expires_at"`
}

// LeaseCookie is a cookie in a lease.
type LeaseCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func fetchLease(ctx context.Context, client *http.Client, in *integration.Compiled, b Binding) (*Session, error) {
	if b.Lease == nil || b.Lease.URL == "" {
		return nil, errors.New("no lease broker is configured for this credential binding")
	}
	u := strings.TrimRight(b.Lease.URL, "/") + "/v1/leases/" + url.PathEscape(in.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.Lease.Token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lease broker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("lease broker: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var l Lease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&l); err != nil {
		return nil, fmt.Errorf("lease broker: %w", err)
	}
	s := &Session{UserAgent: l.UserAgent, Expires: l.ExpiresAt, Acquired: time.Now()}
	for _, c := range l.Cookies {
		s.Cookies = append(s.Cookies, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	if len(s.Cookies) == 0 {
		return nil, errors.New("lease broker returned no cookies")
	}
	return s, nil
}
