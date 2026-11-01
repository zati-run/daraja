package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/internal/notesapp"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/mcpserver"
	"github.com/zati-run/daraja/oauth"
	"github.com/zati-run/daraja/runtime"
	"github.com/zati-run/daraja/session"
)

const passphrase = "correct horse battery staple"

type env struct {
	rtURL     string
	clientID  string
	redirect  string
	srv       *oauth.Server
	statePath string
	cimd      *httptest.Server
}

// start runs the synthetic app, a runtime with no static principals, the
// embedded authorization server, and a TLS server hosting the client's
// metadata document.
func start(t *testing.T) *env {
	t.Helper()
	app := notesapp.New("synthetic-user", "synthetic-pass")
	appSrv := httptest.NewServer(app)
	t.Cleanup(appSrv.Close)
	b, err := os.ReadFile("../integration/testdata/notes.json")
	if err != nil {
		t.Fatal(err)
	}
	in, err := integration.Parse([]byte(strings.Replace(string(b), "http://127.0.0.1:1", appSrv.URL, 1)))
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "j.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	rt, err := runtime.New(runtime.Config{
		Integrations: []*integration.Compiled{in}, Journal: j,
		Credentials: map[string]session.Binding{"notes": {Values: map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	e := &env{redirect: "http://127.0.0.1:53682/callback", statePath: filepath.Join(t.TempDir(), "oauth.json")}
	e.cimd = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"client_id": e.clientID, "client_name": "Synthetic Agent", "redirect_uris": []string{e.redirect},
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
			"token_endpoint_auth_method": "none",
		}
		if r.URL.Path == "/liar.json" {
			doc["client_id"] = "https://someone-else.example/client.json"
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(e.cimd.Close)
	e.clientID = e.cimd.URL + "/client.json"

	mux := http.NewServeMux()
	rtSrv := httptest.NewServer(mux)
	t.Cleanup(rtSrv.Close)
	e.rtURL = rtSrv.URL
	hash, err := oauth.HashPassphrase(passphrase, 1000)
	if err != nil {
		t.Fatal(err)
	}
	e.srv, err = oauth.New(oauth.Config{
		Issuer: rtSrv.URL, OwnerPassphraseHash: hash, StatePath: e.statePath,
		Catalog: rt, Bindings: []string{"notes"}, MetadataClient: e.cimd.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Routes(mux)
	mux.Handle("/mcp", mcpserver.NewHandler(rt, "test", mcpserver.WithAuthenticator(e.srv.Verify), mcpserver.WithChallenge(e.srv.Challenge())))
	return e
}

var requestRE = regexp.MustCompile(`name=request value="([^"]+)"`)

// browserless plays the person at the consent page: it reads the form and
// submits it with the given passphrase and operations.
func consent(t *testing.T, authURL, pass string, ops ...string) *http.Response {
	t.Helper()
	nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := nc.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := requestRE.FindSubmatch(page)
	if m == nil {
		t.Fatalf("no consent form (status %d): %s", resp.StatusCode, page)
	}
	form := url.Values{"request": {string(m[1])}, "passphrase": {pass}, "action": {"approve"}, "binding_synthetic-notes": {"notes"}}
	for _, op := range ops {
		form.Add("op", "synthetic-notes/"+op)
	}
	u, _ := url.Parse(authURL)
	resp, err = nc.PostForm(u.Scheme+"://"+u.Host+oauth.AuthorizePath, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCIMDClientAuthorizesAndSeesOnlyGrantedTools(t *testing.T) {
	e := start(t)
	fetcher := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		resp := consent(t, args.URL, passphrase, "get_note")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("consent status %d: %s", resp.StatusCode, body)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: e.clientID},
		RedirectURL:                    e.redirect,
		AuthorizationCodeFetcher:       fetcher,
		RequestRefreshToken:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "sdk-oauth-test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: e.rtURL + "/mcp", OAuthHandler: h}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if !slices.Contains(names, "synthetic-notes__get_note") || slices.Contains(names, "synthetic-notes__create_note") {
		t.Fatalf("tools = %v, want the granted read and not the write", names)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "synthetic-notes__get_note", Arguments: map[string]any{"id": 1}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}

	// The grant is persisted, and token values never are.
	st, err := os.ReadFile(e.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(e.statePath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %v, want 0600", fi.Mode().Perm())
	}
	tok, _ := h.TokenSource(context.Background())
	tk, err := tok.Token()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(st), tk.AccessToken) || strings.Contains(string(st), tk.RefreshToken) {
		t.Fatal("a raw token was persisted")
	}

	// Revoking the grant ends access.
	g := e.srv.Grants()
	if len(g) != 1 {
		t.Fatalf("grants = %d", len(g))
	}
	if err := e.srv.Revoke(g[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.srv.Verify(tk.AccessToken); ok {
		t.Fatal("token still valid after revocation")
	}
}

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 50)
	s := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(s[:])
}

func authURL(e *env, clientID, redirect, challenge string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"st"}}
	if challenge != "" {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	return e.rtURL + oauth.AuthorizePath + "?" + q.Encode()
}

func TestAuthorizationRefusals(t *testing.T) {
	e := start(t)
	nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(u string) *http.Response {
		resp, err := nc.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	_, ch := pkce()

	if r := get(authURL(e, e.clientID, "http://127.0.0.1:9/other", ch)); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown redirect_uri: status %d, want 400 without redirect", r.StatusCode)
	}
	if r := get(authURL(e, e.cimd.URL+"/liar.json", e.redirect, ch)); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched client_id: status %d", r.StatusCode)
	}
	if r := get(authURL(e, "https://example.com/", e.redirect, ch)); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("root client_id: status %d", r.StatusCode)
	}
	r := get(authURL(e, e.clientID, e.redirect, ""))
	if r.StatusCode != http.StatusFound || !strings.Contains(r.Header.Get("Location"), "error=invalid_request") {
		t.Fatalf("missing PKCE: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	// A wrong passphrase grants nothing.
	resp := consent(t, authURL(e, e.clientID, e.redirect, ch), "wrong passphrase!", "get_note")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "not correct") || len(e.srv.Grants()) != 0 {
		t.Fatalf("wrong passphrase: %d grants=%d", resp.StatusCode, len(e.srv.Grants()))
	}

	// A wrong verifier is refused; the code then cannot be replayed.
	resp = consent(t, authURL(e, e.clientID, e.redirect, ch), passphrase, "get_note")
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" || loc.Query().Get("iss") != e.rtURL {
		t.Fatalf("no code or iss: %s", loc)
	}
	tokenReq := func(verifier string) map[string]string {
		resp, err := http.PostForm(e.rtURL+oauth.TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {code},
			"client_id": {e.clientID}, "redirect_uri": {e.redirect}, "code_verifier": {verifier}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if out := tokenReq(strings.Repeat("w", 50)); out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %v", out)
	}
	v, _ := pkce()
	if out := tokenReq(v); out["error"] != "invalid_grant" {
		t.Fatalf("replayed code: %v", out)
	}

	// The default metadata client refuses non-public addresses.
	_, err := oauth.PublicOnlyClient().Get(e.cimd.URL + "/client.json")
	if err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("public-only client reached loopback: %v", err)
	}
}

func TestUnauthenticatedMCPPointsToResourceMetadata(t *testing.T) {
	e := start(t)
	resp, err := http.Post(e.rtURL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), e.srv.ResourceMetadataURL()) {
		t.Fatalf("status %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// grantTokens consents for ops and exchanges the code, returning the token
// response.
func grantTokens(t *testing.T, e *env, ops ...string) map[string]any {
	t.Helper()
	v, ch := pkce()
	resp := consent(t, authURL(e, e.clientID, e.redirect, ch), passphrase, ops...)
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return tokenPost(t, e, url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {e.clientID}, "redirect_uri": {e.redirect}, "code_verifier": {v}})
}

func tokenPost(t *testing.T, e *env, f url.Values) map[string]any {
	t.Helper()
	resp, err := http.PostForm(e.rtURL+oauth.TokenPath, f)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// Each consent is a new grant, but receipts belong to the client, so a
// re-authorized client still sees, and cannot re-send, its earlier writes.
func TestReconsentKeepsReceiptScope(t *testing.T) {
	e := start(t)
	a := grantTokens(t, e, "get_note")
	b := grantTokens(t, e, "get_note", "create_note")
	pa, ok1 := e.srv.Verify(a["access_token"].(string))
	pb, ok2 := e.srv.Verify(b["access_token"].(string))
	if !ok1 || !ok2 {
		t.Fatalf("tokens not valid: %v %v", a, b)
	}
	if pa.ID == pb.ID {
		t.Fatal("two consents share a grant")
	}
	if pa.ReceiptScope == "" || pa.ReceiptScope != pb.ReceiptScope {
		t.Fatalf("receipt scopes %q and %q, want one non-empty scope", pa.ReceiptScope, pb.ReceiptScope)
	}
}

// A rotated-out refresh token presented again revokes the grant.
func TestRefreshTokenReuseRevokesGrant(t *testing.T) {
	e := start(t)
	first := grantTokens(t, e, "get_note")
	old := first["refresh_token"].(string)
	refresh := func(tok string) map[string]any {
		return tokenPost(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {e.clientID}})
	}
	second := refresh(old)
	if second["access_token"] == nil {
		t.Fatalf("refresh: %v", second)
	}
	if out := refresh(old); out["error"] != "invalid_grant" {
		t.Fatalf("reuse: %v", out)
	}
	if _, ok := e.srv.Verify(second["access_token"].(string)); ok {
		t.Fatal("tokens issued after rotation still valid after reuse")
	}
	if out := refresh(second["refresh_token"].(string)); out["error"] != "invalid_grant" {
		t.Fatalf("current refresh token after reuse: %v", out)
	}
}

// Wrong passphrases are limited across requests: fresh pending requests
// do not reset the budget, and once locked, even the right passphrase
// waits.
func TestConsentLockoutAcrossRequests(t *testing.T) {
	e := start(t)
	_, ch := pkce()
	for i := 0; i < 10; i++ {
		resp := consent(t, authURL(e, e.clientID, e.redirect, ch), "wrong passphrase!", "get_note")
		resp.Body.Close()
	}
	resp := consent(t, authURL(e, e.clientID, e.redirect, ch), passphrase, "get_note")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Try again in") || len(e.srv.Grants()) != 0 {
		t.Fatalf("after budget: status %d grants=%d", resp.StatusCode, len(e.srv.Grants()))
	}
}
