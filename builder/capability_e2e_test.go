package builder_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zati-run/daraja/builder/browser"
	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/internal/notesapp"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/mcpcheck"
	"github.com/zati-run/daraja/mcpserver"
	"github.com/zati-run/daraja/runtime"
)

const (
	capToken   = "capability-synthetic-token"
	agentToken = "agent-synthetic-token"
)

type capEnv struct {
	app    *notesapp.App
	rtURL  string
	broker *capability.Broker
	stop   func() // stops the capability agent
}

func browserArtifact(t *testing.T, origin string, edits ...string) *integration.Compiled {
	t.Helper()
	b, err := os.ReadFile("../integration/testdata/notes-browser.json")
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "http://127.0.0.1:1", origin, 1))
	for i := 0; i+1 < len(edits); i += 2 {
		if !strings.Contains(string(b), edits[i]) {
			t.Fatalf("fixture has no %q", edits[i])
		}
		b = []byte(strings.Replace(string(b), edits[i], edits[i+1], 1))
	}
	in, err := integration.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// startCapEnv runs the synthetic app, a runtime that brokers to
// capabilities over MCP Streamable HTTP, and a capability host on real
// Chrome that dials out to the runtime. Binding "alice" signs in with a
// recipe; binding "nobody" has no credentials.
func startCapEnv(t *testing.T, capTimeout time.Duration, edits ...string) *capEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	app := notesapp.New("synthetic-user", "synthetic-pass")
	app.RedirectUnauthenticated = len(edits) > 0
	appSrv := httptest.NewServer(app)
	t.Cleanup(appSrv.Close)
	in := browserArtifact(t, appSrv.URL, edits...)

	broker, err := capability.NewBroker(capability.BrokerConfig{
		Registrations: []capability.Registration{{ID: "host", TokenSHA256: capability.HashToken(capToken), Bindings: []string{"alice", "nobody"}}},
		PollWait:      time.Second, LiveFor: 2 * time.Second, PickupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	all := []string{"get_note", "find_note_by_ref", "create_note"}
	rt, err := runtime.New(runtime.Config{
		Integrations: []*integration.Compiled{in},
		Principals: []runtime.Principal{
			{ID: "agent", TokenSHA256: runtime.HashToken(agentToken), Grants: []runtime.Grant{{Integration: in.ID, Operations: all, Credential: "alice"}}},
			{ID: "stranger", TokenSHA256: runtime.HashToken("stranger-token"), Grants: []runtime.Grant{{Integration: in.ID, Operations: []string{"get_note"}, Credential: "nobody"}}},
		},
		Journal: j, Capabilities: broker, CapabilityTimeout: capTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.NewHandler(rt, "test"))
	mux.Handle("/capability/", broker.Handler())
	rtSrv := httptest.NewServer(mux)
	t.Cleanup(rtSrv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	open := func(name string, creds map[string]string) *browser.Browser {
		b, err := browser.Open(ctx, browser.Options{ProfileDir: filepath.Join(t.TempDir(), name), Headless: true, Credentials: creds})
		if err != nil {
			cancel()
			t.Skipf("Chrome is not available: %v", err)
		}
		return b
	}
	alice := open("alice", map[string]string{"username": "synthetic-user", "password": "synthetic-pass"})
	nobody := open("nobody", nil)
	host, err := capability.NewHost(capability.HostConfig{
		ID: "host", Version: "test", Integrations: []*integration.Compiled{in},
		Bindings: []capability.HostBinding{
			{Name: "alice", Page: alice, Grants: map[string][]string{in.ID: all}},
			{Name: "nobody", Page: nobody, Grants: map[string][]string{in.ID: {"get_note"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := &capability.Agent{URL: rtSrv.URL, Token: capToken, Exec: host, JobTimeout: 30 * time.Second}
	done := make(chan struct{})
	go func() { _ = agent.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		<-done
		alice.Close()
		nobody.Close()
	}
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			stop()
		}
	})
	e := &capEnv{app: app, rtURL: rtSrv.URL, broker: broker, stop: stop}
	ref := capability.OpRef{IntegrationID: in.ID, Version: in.Version, ArtifactSHA256: in.Hash, Operation: "get_note"}
	waitFor(t, 10*time.Second, func() bool { return broker.Live("alice", ref) })
	return e
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mcpClient(t *testing.T, url, token string) *mcpcheck.Client {
	t.Helper()
	c := &mcpcheck.Client{URL: url + "/mcp", Token: token}
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func call(t *testing.T, c *mcpcheck.Client, tool string, args map[string]any) (map[string]any, *mcpcheck.ToolResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text()), &out)
	return out, res
}

func TestCapabilityInPageReadWriteAndRecovery(t *testing.T) {
	e := startCapEnv(t, 60*time.Second)
	c := mcpClient(t, e.rtURL, agentToken)
	tool := func(op string) string { return mcpserver.ToolName("synthetic-notes-browser", op) }

	// A5: a read runs inside the page against the internal API.
	out, res := call(t, c, tool("get_note"), map[string]any{"id": 1})
	if res.IsError || out["title"] != "Seed note" {
		t.Fatalf("read: error=%v %s", res.IsError, res.Text())
	}
	if e.app.Logins() != 1 {
		t.Fatalf("logins = %d, want 1 (the capability signs in once)", e.app.Logins())
	}

	// A8, A9: one write, and a repeated key does not dispatch again.
	args := map[string]any{"title": "Capability note", "idempotency_key": "cap-w1"}
	out, res = call(t, c, tool("create_note"), args)
	if res.IsError || out["id"] == nil {
		t.Fatalf("write: %s", res.Text())
	}
	_, res = call(t, c, tool("create_note"), args)
	if res.IsError {
		t.Fatalf("duplicate: %s", res.Text())
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1", e.app.Creates())
	}

	// Expired source session: the capability signs in again by itself.
	e.app.ExpireSessions()
	_, res = call(t, c, tool("get_note"), map[string]any{"id": 1})
	if res.IsError {
		t.Fatalf("read after expiry: %s", res.Text())
	}
	if e.app.Logins() != 2 {
		t.Fatalf("logins = %d, want 2", e.app.Logins())
	}

	// A16: a binding that needs a person answers at once without blocking.
	s := mcpClient(t, e.rtURL, "stranger-token")
	start := time.Now()
	_, res = call(t, s, tool("get_note"), map[string]any{"id": 1})
	if !res.IsError || !strings.Contains(res.Text(), capability.CodeNeedsHuman) {
		t.Fatalf("want %s, got %s", capability.CodeNeedsHuman, res.Text())
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("session_needs_human took %s; it must not block", time.Since(start))
	}

	// No registered capability: nothing is sent and the write is not_sent.
	e.stop()
	time.Sleep(2500 * time.Millisecond) // LiveFor is 2 s in this environment
	_, res = call(t, c, tool("create_note"), map[string]any{"title": "Offline", "idempotency_key": "cap-w2"})
	if !res.IsError || !strings.Contains(res.Text(), capability.CodeUnavailable) || !strings.Contains(res.Text(), `"failed"`) {
		t.Fatalf("want failed %s, got %s", capability.CodeUnavailable, res.Text())
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1", e.app.Creates())
	}
}

func TestCapabilityLostAnswerIsUncertainThenReconciled(t *testing.T) {
	e := startCapEnv(t, 3*time.Second)
	e.app.SlowTitle, e.app.SlowFor = "Slow capability note", 6*time.Second
	c := mcpClient(t, e.rtURL, agentToken)
	tool := func(op string) string { return mcpserver.ToolName("synthetic-notes-browser", op) }

	// Warm the session so the slow write is the only long step.
	if _, res := call(t, c, tool("get_note"), map[string]any{"id": 1}); res.IsError {
		t.Fatalf("read: %s", res.Text())
	}
	args := map[string]any{"title": "Slow capability note", "idempotency_key": "cap-slow"}
	_, res := call(t, c, tool("create_note"), args)
	if !res.IsError || !strings.Contains(res.Text(), "uncertain") {
		t.Fatalf("want uncertain, got %s", res.Text())
	}
	// A retry with the same key never re-dispatches.
	_, res = call(t, c, tool("create_note"), args)
	if !res.IsError || !strings.Contains(res.Text(), "reconcile") {
		t.Fatalf("retry: %s", res.Text())
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1", e.app.Creates())
	}
	// While the slow write still holds the binding, a reconcile read may not
	// get an answer either; the receipt then stays uncertain. Once the page
	// is free, the reconcile read finds the effect by its marker.
	var out map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, res = call(t, c, "daraja__reconcile", map[string]any{"tool": tool("create_note"), "idempotency_key": "cap-slow"})
		if !res.IsError && out["state"] == journal.Succeeded {
			break
		}
		if !strings.Contains(res.Text(), "uncertain") || time.Now().After(deadline) {
			t.Fatalf("reconcile: %s", res.Text())
		}
		time.Sleep(time.Second)
	}
	if e.app.Creates() != 1 {
		t.Fatalf("creates after reconcile = %d, want 1", e.app.Creates())
	}
}

// In the page, an operation never follows a redirect: a write sent with an
// expired session that the source redirects to sign in is uncertain, not
// succeeded, even if the sign-in page's status counts as success.
func TestCapabilityRedirectedWriteIsUncertain(t *testing.T) {
	e := startCapEnv(t, 60*time.Second, "\"success_status\": [\n          201\n        ]", "\"success_status\": [200, 201]")
	c := mcpClient(t, e.rtURL, agentToken)
	tool := func(op string) string { return mcpserver.ToolName("synthetic-notes-browser", op) }
	if _, res := call(t, c, tool("get_note"), map[string]any{"id": 1}); res.IsError {
		t.Fatalf("read: %s", res.Text())
	}
	e.app.ExpireSessions()
	args := map[string]any{"title": "Redirected note", "idempotency_key": "cap-redirect"}
	_, res := call(t, c, tool("create_note"), args)
	if !res.IsError || !strings.Contains(res.Text(), "uncertain") {
		t.Fatalf("want uncertain, got %s", res.Text())
	}
	if e.app.Creates() != 0 {
		t.Fatalf("creates = %d, want 0", e.app.Creates())
	}
	// The host dropped the session, so the next write signs in again.
	out, res := call(t, c, tool("create_note"), map[string]any{"title": "After redirect", "idempotency_key": "cap-redirect-2"})
	if res.IsError || out["id"] == nil || e.app.Creates() != 1 {
		t.Fatalf("write after redirect: %s creates=%d", res.Text(), e.app.Creates())
	}
}
