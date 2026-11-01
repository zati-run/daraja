package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/internal/notesapp"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/mcpcheck"
	"github.com/zati-run/daraja/mcpserver"
	"github.com/zati-run/daraja/runtime"
	"github.com/zati-run/daraja/session"
)

const (
	tokenAlice = "alice-synthetic-token"
	tokenBob   = "bob-synthetic-token"
)

type env struct {
	app      *notesapp.App
	appSrv   *httptest.Server
	rtSrv    *httptest.Server
	journal  string
	in       *integration.Compiled
	faults   *faultTransport
	runtime  *runtime.Runtime
	journalH *journal.Journal
}

// faultTransport sends requests normally, but can lose the response of the
// next write after the source has received it.
type faultTransport struct {
	mu        sync.Mutex
	loseNext  bool
	blockNext chan struct{}
}

func (f *faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	isOpWrite := req.Method != http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/")
	lose := f.loseNext && isOpWrite
	var block chan struct{}
	if lose {
		f.loseNext = false
	}
	if f.blockNext != nil && isOpWrite {
		block, f.blockNext = f.blockNext, nil
	}
	f.mu.Unlock()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if lose {
		resp.Body.Close()
		return nil, errors.New("synthetic fault: connection reset after the request was sent")
	}
	if block != nil {
		<-block // simulate a process that stops before recording the outcome
		resp.Body.Close()
		return nil, errors.New("synthetic fault: stopped")
	}
	return resp, nil
}

func loadFixture(t *testing.T, origin string, edits ...string) *integration.Compiled {
	t.Helper()
	b, err := os.ReadFile("../integration/testdata/notes.json")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.Replace(string(b), "http://127.0.0.1:1", origin, 1)
	for i := 0; i+1 < len(edits); i += 2 {
		if !strings.Contains(src, edits[i]) {
			t.Fatalf("fixture has no %q", edits[i])
		}
		src = strings.Replace(src, edits[i], edits[i+1], 1)
	}
	c, err := integration.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newEnv(t *testing.T, edits ...string) *env {
	t.Helper()
	e := &env{app: notesapp.New("synthetic-user", "synthetic-pass")}
	e.appSrv = httptest.NewServer(e.app)
	t.Cleanup(e.appSrv.Close)
	e.in = loadFixture(t, e.appSrv.URL, edits...)
	e.journal = filepath.Join(t.TempDir(), "journal.jsonl")
	e.faults = &faultTransport{}
	e.start(t)
	return e
}

func (e *env) start(t *testing.T) {
	t.Helper()
	j, err := journal.Open(e.journal)
	if err != nil {
		t.Fatal(err)
	}
	e.journalH = j
	rt, err := runtime.New(runtime.Config{
		Integrations: []*integration.Compiled{e.in},
		Principals: []runtime.Principal{
			{ID: "alice", TokenSHA256: runtime.HashToken(tokenAlice), Grants: []runtime.Grant{{Integration: "synthetic-notes", Operations: []string{"get_note", "find_note_by_ref", "create_note"}, Credential: "notes-user"}}},
			{ID: "bob", TokenSHA256: runtime.HashToken(tokenBob), Grants: []runtime.Grant{{Integration: "synthetic-notes", Operations: []string{"get_note"}, Credential: "notes-user"}}},
		},
		Credentials:    map[string]session.Binding{"notes-user": {Values: map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}}},
		Journal:        j,
		HTTPClient:     &http.Client{Transport: e.faults},
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.runtime = rt
	e.rtSrv = httptest.NewServer(mcpserver.NewHandler(rt, "test"))
	t.Cleanup(e.rtSrv.Close)
}

func (e *env) client(t *testing.T, token string) *mcpcheck.Client {
	t.Helper()
	c := &mcpcheck.Client{URL: e.rtSrv.URL, Token: token}
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func call(t *testing.T, c *mcpcheck.Client, tool string, args map[string]any) (*mcpcheck.ToolResult, map[string]any) {
	t.Helper()
	r, err := c.CallTool(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var body map[string]any
	if r.IsError {
		_ = json.Unmarshal([]byte(r.Text()), &body)
	} else {
		_ = json.Unmarshal(r.StructuredContent, &body)
	}
	return r, body
}

func TestScopedToolListing(t *testing.T) {
	e := newEnv(t)
	tools, err := e.client(t, tokenAlice).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]mcpcheck.Tool{}
	for _, tl := range tools {
		names[tl.Name] = tl
	}
	for _, want := range []string{"synthetic-notes__get_note", "synthetic-notes__create_note", "daraja__reconcile", "daraja__receipt"} {
		if _, ok := names[want]; !ok {
			t.Errorf("alice missing tool %s (got %v)", want, tools)
		}
	}
	create := names["synthetic-notes__create_note"]
	if create.Annotations["readOnlyHint"] != false || !strings.Contains(string(create.InputSchema), "idempotency_key") {
		t.Errorf("write tool contract wrong: %+v %s", create.Annotations, create.InputSchema)
	}
	if names["synthetic-notes__get_note"].Annotations["readOnlyHint"] != true {
		t.Error("read tool not marked read-only")
	}

	bob, err := e.client(t, tokenBob).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bob) != 1 || bob[0].Name != "synthetic-notes__get_note" {
		t.Errorf("bob tools = %+v", bob)
	}
}

func TestUnauthenticatedRefused(t *testing.T) {
	e := newEnv(t)
	c := &mcpcheck.Client{URL: e.rtSrv.URL, Token: "wrong"}
	err := c.Initialize(context.Background())
	var he *mcpcheck.HTTPError
	if !errors.As(err, &he) || he.Status != 401 {
		t.Fatalf("err = %v", err)
	}
}

func TestReadWithHTTPFormSession(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, tokenAlice)
	r, out := call(t, c, "synthetic-notes__get_note", map[string]any{"id": 1})
	if r.IsError || out["title"] != "Seed note" || out["id"].(float64) != 1 {
		t.Fatalf("read = %v %s", out, r.Text())
	}
	// A rejected session is refreshed once for reads.
	e.app.ExpireSessions()
	r, out = call(t, c, "synthetic-notes__get_note", map[string]any{"id": 1})
	if r.IsError || out["title"] != "Seed note" {
		t.Fatalf("read after expiry = %s", r.Text())
	}
	if e.app.Logins() != 2 {
		t.Errorf("logins = %d, want 2", e.app.Logins())
	}
}

func TestScopeDenialAndInvalidInputSendNothing(t *testing.T) {
	e := newEnv(t)
	bob := e.client(t, tokenBob)
	// Bob's server has no such tool, so the protocol reports an unknown tool.
	if _, err := bob.CallTool(context.Background(), "synthetic-notes__create_note", map[string]any{"title": "x", "idempotency_key": "k"}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("bob create err = %v", err)
	}
	if _, rerr := e.runtime.Invoke(context.Background(), mustPrincipal(t, e, "bob"), "synthetic-notes", "create_note", map[string]any{"title": "x"}, "k"); rerr == nil || rerr.Code != runtime.CodeScopeDenied {
		t.Fatalf("runtime scope check = %v", rerr)
	}
	alice := e.client(t, tokenAlice)
	r, body := call(t, alice, "synthetic-notes__create_note", map[string]any{"title": "", "idempotency_key": "k1"})
	if !r.IsError || body["code"] != runtime.CodeInvalidInput {
		t.Fatalf("invalid input = %s", r.Text())
	}
	r, body = call(t, alice, "synthetic-notes__create_note", map[string]any{"title": "no key"})
	if !r.IsError || body["code"] != runtime.CodeInvalidInput {
		t.Fatalf("missing key = %s", r.Text())
	}
	if e.app.Creates() != 0 {
		t.Fatalf("creates = %d, want 0", e.app.Creates())
	}
}

func mustPrincipal(t *testing.T, e *env, id string) *runtime.Principal {
	p, ok := e.runtime.Principal(id)
	if !ok {
		t.Fatal("no principal", id)
	}
	return p
}

func TestWriteOnceAndDuplicateKey(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, tokenAlice)
	r, out := call(t, c, "synthetic-notes__create_note", map[string]any{"title": "Synthetic A", "idempotency_key": "w-1"})
	if r.IsError {
		t.Fatalf("write: %s", r.Text())
	}
	id := out["id"].(float64)
	rec := r.Meta["daraja/receipt"].(map[string]any)
	if rec["state"] != journal.Succeeded || rec["identifier"] != "2" {
		t.Fatalf("receipt = %v", rec)
	}
	r, out = call(t, c, "synthetic-notes__create_note", map[string]any{"title": "Synthetic A", "idempotency_key": "w-1"})
	if r.IsError || out["id"].(float64) != id {
		t.Fatalf("replay = %s %v", r.Text(), out)
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1 (duplicate key must not re-dispatch)", e.app.Creates())
	}
}

// A write sent with an expired session that the source redirects to its
// sign-in page must not be recorded as succeeded, even when the sign-in
// page answers with a status the operation counts as success.
func TestRedirectedWriteIsNotSucceeded(t *testing.T) {
	e := newEnv(t, `"success_status": [201]`, `"success_status": [200, 201]`)
	e.app.RedirectUnauthenticated = true
	c := e.client(t, tokenAlice)
	if r, _ := call(t, c, "synthetic-notes__get_note", map[string]any{"id": 1}); r.IsError {
		t.Fatalf("read: %s", r.Text())
	}
	e.app.ExpireSessions()
	args := map[string]any{"title": "Synthetic R", "idempotency_key": "w-redirect"}
	r, body := call(t, c, "synthetic-notes__create_note", args)
	if !r.IsError || body["code"] != runtime.CodeOutcomeUncertain {
		t.Fatalf("redirected write = %s", r.Text())
	}
	// The same key never re-sends; the receipt stays uncertain until
	// reconciled.
	r, body = call(t, c, "synthetic-notes__create_note", args)
	if !r.IsError || body["code"] != runtime.CodeOutcomeUncertain {
		t.Fatalf("retry = %s", r.Text())
	}
	if e.app.Creates() != 0 {
		t.Fatalf("creates = %d, want 0", e.app.Creates())
	}
	// The dropped session is refreshed, and a new key writes once.
	r, out := call(t, c, "synthetic-notes__create_note", map[string]any{"title": "Synthetic R2", "idempotency_key": "w-redirect-2"})
	if r.IsError || out["id"] == nil || e.app.Creates() != 1 {
		t.Fatalf("write after refresh = %s creates=%d", r.Text(), e.app.Creates())
	}
}

// Two principals with one receipt scope (one OAuth client, re-authorized)
// share receipts: the second cannot re-send the first's uncertain write.
func TestReceiptScopeSurvivesNewPrincipal(t *testing.T) {
	e := newEnv(t)
	grants := []runtime.Grant{{Integration: "synthetic-notes", Operations: []string{"get_note", "find_note_by_ref", "create_note"}, Credential: "notes-user"}}
	first := &runtime.Principal{ID: "oauth:g1", Grants: grants, ReceiptScope: "oauth-client:https://agent.example/client.json"}
	second := &runtime.Principal{ID: "oauth:g2", Grants: grants, ReceiptScope: first.ReceiptScope}
	e.faults.mu.Lock()
	e.faults.loseNext = true
	e.faults.mu.Unlock()
	in := map[string]any{"title": "Synthetic S"}
	if _, rerr := e.runtime.Invoke(context.Background(), first, "synthetic-notes", "create_note", in, "w-scope"); rerr == nil || rerr.Code != runtime.CodeOutcomeUncertain {
		t.Fatalf("first = %v", rerr)
	}
	if _, rerr := e.runtime.Invoke(context.Background(), second, "synthetic-notes", "create_note", in, "w-scope"); rerr == nil || rerr.Code != runtime.CodeOutcomeUncertain {
		t.Fatalf("second = %v", rerr)
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1", e.app.Creates())
	}
	rec, rerr := e.runtime.Reconcile(context.Background(), second, "synthetic-notes", "create_note", "w-scope")
	if rerr != nil || rec.State != journal.Succeeded {
		t.Fatalf("reconcile by the new principal = %v %v", rec, rerr)
	}
}

// A key reused with different input is a conflict: nothing is sent and
// nothing of the earlier receipt is returned. Same key, same input replays.
func TestIdempotencyKeyConflict(t *testing.T) {
	e := newEnv(t)
	grants := []runtime.Grant{{Integration: "synthetic-notes", Operations: []string{"create_note"}, Credential: "notes-user"}}
	a := &runtime.Principal{ID: "oauth:g1", Grants: grants, ReceiptScope: "oauth-client:https://agent.example/client.json"}
	b := &runtime.Principal{ID: "oauth:g2", Grants: grants, ReceiptScope: a.ReceiptScope}
	ctx := context.Background()
	if _, rerr := e.runtime.Invoke(ctx, a, "synthetic-notes", "create_note", map[string]any{"title": "From A"}, "create-note-1"); rerr != nil {
		t.Fatalf("a: %v", rerr)
	}
	res, rerr := e.runtime.Invoke(ctx, b, "synthetic-notes", "create_note", map[string]any{"title": "From B"}, "create-note-1")
	if rerr == nil || rerr.Code != runtime.CodeKeyConflict || rerr.Receipt != nil || res != nil {
		t.Fatalf("b with other input = %v %v", res, rerr)
	}
	if res, rerr := e.runtime.Invoke(ctx, b, "synthetic-notes", "create_note", map[string]any{"title": "From A"}, "create-note-1"); rerr != nil || res.Receipt == nil {
		t.Fatalf("same input replay = %v %v", res, rerr)
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1", e.app.Creates())
	}
	c := e.client(t, tokenAlice)
	call(t, c, "synthetic-notes__create_note", map[string]any{"title": "MCP", "idempotency_key": "k-mcp"})
	r, body := call(t, c, "synthetic-notes__create_note", map[string]any{"title": "MCP other", "idempotency_key": "k-mcp"})
	if !r.IsError || body["code"] != runtime.CodeKeyConflict {
		t.Fatalf("mcp conflict = %s", r.Text())
	}
	if e.app.Creates() != 2 {
		t.Fatalf("creates = %d, want 2", e.app.Creates())
	}
}

func TestLostResponseIsUncertainThenReconciled(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, tokenAlice)
	e.faults.mu.Lock()
	e.faults.loseNext = true
	e.faults.mu.Unlock()
	r, body := call(t, c, "synthetic-notes__create_note", map[string]any{"title": "Synthetic B", "idempotency_key": "w-2"})
	if !r.IsError || body["code"] != runtime.CodeOutcomeUncertain {
		t.Fatalf("lost response = %s", r.Text())
	}
	// Retrying the same key does not re-send.
	r, body = call(t, c, "synthetic-notes__create_note", map[string]any{"title": "Synthetic B", "idempotency_key": "w-2"})
	if !r.IsError || body["code"] != runtime.CodeOutcomeUncertain || e.app.Creates() != 1 {
		t.Fatalf("retry = %s creates=%d", r.Text(), e.app.Creates())
	}
	r, body = call(t, c, "daraja__reconcile", map[string]any{"tool": "synthetic-notes__create_note", "idempotency_key": "w-2"})
	if r.IsError || body["state"] != journal.Succeeded || body["identifier"] != "2" {
		t.Fatalf("reconcile = %s", r.Text())
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d after reconcile", e.app.Creates())
	}
}

func TestReconcileNotApplied(t *testing.T) {
	e := newEnv(t)
	// Point the write at a closed port after the session exists, so the
	// request is classified by the transport. Here we instead journal an
	// uncertain receipt directly, as a restart would, with no effect.
	p := mustPrincipal(t, e, "alice")
	key := journal.Key(p.ID, "synthetic-notes", "create_note", "w-3")
	if _, err := e.journalH.Begin(journal.Receipt{Key: key, Principal: p.ID, Integration: "synthetic-notes", Operation: "create_note", Marker: runtime.Marker(key)}); err != nil {
		t.Fatal(err)
	}
	c := e.client(t, tokenAlice)
	r, body := call(t, c, "daraja__reconcile", map[string]any{"tool": "synthetic-notes__create_note", "idempotency_key": "w-3"})
	if r.IsError || body["state"] != journal.NotApplied {
		t.Fatalf("reconcile = %s", r.Text())
	}
	r, body = call(t, c, "synthetic-notes__create_note", map[string]any{"title": "again", "idempotency_key": "w-3"})
	if !r.IsError || !strings.Contains(body["message"].(string), "new key") {
		t.Fatalf("final key reused = %s", r.Text())
	}
}

func TestRestartDuringDispatch(t *testing.T) {
	e := newEnv(t)
	block := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	t.Cleanup(release)
	e.faults.mu.Lock()
	e.faults.blockNext = block
	e.faults.mu.Unlock()
	c := e.client(t, tokenAlice)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.CallTool(context.Background(), "synthetic-notes__create_note", map[string]any{"title": "Synthetic C", "idempotency_key": "w-4"})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.app.Creates() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.app.Creates() != 1 {
		t.Fatal("write never reached the app")
	}
	// "Crash": stop the first runtime's journal so it can record nothing,
	// then start a new runtime on the same journal file.
	// The old server stays up (its blocked request is released below); a new
	// runtime and server take over.
	e.journalH.Close()
	e.start(t)
	release()
	<-done

	c2 := e.client(t, tokenAlice)
	r, body := call(t, c2, "daraja__receipt", map[string]any{"tool": "synthetic-notes__create_note", "idempotency_key": "w-4"})
	if r.IsError || body["state"] != journal.Uncertain || body["code"] != "restart_during_dispatch" {
		t.Fatalf("receipt after restart = %s", r.Text())
	}
	r, body = call(t, c2, "synthetic-notes__create_note", map[string]any{"title": "Synthetic C", "idempotency_key": "w-4"})
	if !r.IsError || body["code"] != runtime.CodeOutcomeUncertain || e.app.Creates() != 1 {
		t.Fatalf("retry after restart = %s creates=%d", r.Text(), e.app.Creates())
	}
	r, body = call(t, c2, "daraja__reconcile", map[string]any{"tool": "synthetic-notes__create_note", "idempotency_key": "w-4"})
	if r.IsError || body["state"] != journal.Succeeded {
		t.Fatalf("reconcile after restart = %s", r.Text())
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d", e.app.Creates())
	}
}

func TestSessionCannotCrossPrincipals(t *testing.T) {
	e := newEnv(t)
	alice := e.client(t, tokenAlice)
	_ = alice
	// Reuse alice's MCP session ID with bob's token.
	sid := sessionID(t, alice)
	req, _ := http.NewRequest(http.MethodPost, e.rtSrv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-principal session status = %d", resp.StatusCode)
	}
}

func sessionID(t *testing.T, c *mcpcheck.Client) string {
	t.Helper()
	v := c.SessionID()
	if v == "" {
		t.Fatal("no session id")
	}
	return v
}
