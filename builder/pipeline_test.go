package builder_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zati-run/daraja/builder/browser"
	"github.com/zati-run/daraja/builder/explore"
	"github.com/zati-run/daraja/builder/lease"
	"github.com/zati-run/daraja/builder/plan"
	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/internal/notesapp"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/mcpcheck"
	"github.com/zati-run/daraja/mcpserver"
	"github.com/zati-run/daraja/runtime"
	"github.com/zati-run/daraja/session"
)

func taskFor(origin string) *explore.Task {
	return &explore.Task{
		Goal:          "read a note and list notes",
		IntegrationID: "synthetic-notes",
		Origin:        origin,
		AllowEffects:  []string{"read"},
		PathPrefixes:  []string{"/api/"},
		Session: integration.SessionSpec{
			Provider: integration.ProviderBrowserLease,
			BrowserSignIn: []integration.BrowserStep{
				{Navigate: "/login"},
				{Fill: `input[name=user]`, Value: "{{credential.username}}"},
				{Fill: `input[name=pass]`, Value: "{{credential.password}}"},
				{Click: `button[type=submit]`},
				{WaitPathNot: "/login"},
			},
			Extract:        map[string]integration.ValueSource{"csrf": {Request: &integration.SimpleRequest{Method: "GET", Path: "/api/me"}, JSONPointer: "/csrf"}},
			SignedIn:       &integration.Check{Request: integration.SimpleRequest{Method: "GET", Path: "/api/me"}, JSONPointer: "/user/id", NotNull: true},
			RequestHeaders: map[string]string{"X-CSRF-Token": "{{session.csrf}}"},
		},
		Walk: []explore.Step{
			{Navigate: "/app", WaitMS: 800},
			{Click: "#create", WaitMS: 800, Note: "a write; must be blocked because the task allows only reads"},
		},
	}
}

func TestExplorePlanServeThroughBrowserLease(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	app := notesapp.New("synthetic-user", "synthetic-pass")
	srv := httptest.NewServer(app)
	defer srv.Close()
	creds := map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := browser.Open(ctx, browser.Options{ProfileDir: filepath.Join(t.TempDir(), "profile"), Headless: true})
	if err != nil {
		t.Skipf("Chrome is not available: %v", err)
	}
	defer b.Close()

	task := taskFor(srv.URL)
	si := mustArtifact(t, task, []integration.Operation{readOp("signed_in_check", "/api/me")})
	rep, err := explore.Explore(ctx, b, task, si, creds)
	if err != nil {
		t.Fatalf("explore: %v", err)
	}
	routes := map[string]explore.Observation{}
	for _, o := range rep.Observations {
		routes[o.Method+" "+o.PathTemplate] = o
	}
	for _, want := range []string{"GET /api/notes/{note_id}", "GET /api/notes", "POST /api/notes"} {
		if _, ok := routes[want]; !ok {
			t.Errorf("missing observation %q; got %v", want, keys(routes))
		}
	}
	if !routes["POST /api/notes"].Blocked || rep.Coverage.BlockedWrites == 0 {
		t.Errorf("write was not blocked: %+v", routes["POST /api/notes"])
	}
	if app.Creates() != 0 {
		t.Fatalf("explorer let a write through: creates=%d", app.Creates())
	}
	raw, _ := json.Marshal(rep)
	for _, secret := range []string{"synthetic-pass", "synthetic-user", "Seed note", "synthetic\""} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("observations contain a value %q", secret)
		}
	}
	if regexp.MustCompile(`c-[0-9a-f]{24}`).Match(raw) {
		t.Error("observations contain a CSRF token")
	}

	cand, pr, err := plan.Deterministic{}.Plan(ctx, task, rep)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := json.Marshal(cand)
	if _, err := integration.Parse(cb); err != nil {
		t.Fatalf("candidate does not validate: %v\nreport: %+v", err, pr)
	}
	var get *integration.Operation
	for i := range cand.Operations {
		if cand.Operations[i].Name == "get_note" {
			get = &cand.Operations[i]
		}
	}
	if get == nil {
		t.Fatalf("no get_note candidate: %+v", pr.Candidates)
	}

	// Review: publish only the read, with a tightened output schema.
	get.OutputSchema = json.RawMessage(`{"type":"object","required":["id","title"],"properties":{"id":{"type":"integer"},"title":{"type":"string"}}}`)
	get.Response.Output = map[string]string{"id": "/note/id", "title": "/note/title"}
	cand.Operations = []integration.Operation{*get}
	cand.Version = "1"
	cand.Provenance.ReviewedBy = "pipeline test"
	pb, _ := json.Marshal(cand)
	published, err := integration.Parse(pb)
	if err != nil {
		t.Fatal(err)
	}

	brokerToken := "synthetic-broker-token"
	br := lease.New(runtime.HashToken(brokerToken), time.Minute, nil)
	br.Add(&lease.Source{Integration: published, Browser: b, Credentials: creds})
	brSrv := httptest.NewServer(br)
	defer brSrv.Close()

	j, err := journal.Open(filepath.Join(t.TempDir(), "j.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	rt, err := runtime.New(runtime.Config{
		Integrations: []*integration.Compiled{published},
		Principals:   []runtime.Principal{{ID: "agent", TokenSHA256: runtime.HashToken("agent-token"), Grants: []runtime.Grant{{Integration: "synthetic-notes", Operations: []string{"get_note"}, Credential: "leased"}}}},
		Credentials:  map[string]session.Binding{"leased": {Lease: &session.LeaseConfig{URL: brSrv.URL, Token: brokerToken}}},
		Journal:      j,
		HTTPClient:   &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	rtSrv := httptest.NewServer(mcpserver.NewHandler(rt, "test"))
	defer rtSrv.Close()
	c := &mcpcheck.Client{URL: rtSrv.URL, Token: "agent-token"}
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(ctx, "synthetic-notes__get_note", map[string]any{"note_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(string(res.StructuredContent), "Seed note") {
		t.Fatalf("leased read = %s", res.Text())
	}
}

func readOp(name, path string) integration.Operation {
	return integration.Operation{
		Name: name, Description: "check", Effect: integration.EffectRead,
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Request:  integration.RequestSpec{Method: "GET", Path: path},
		Response: integration.ResponseSpec{SuccessStatus: []int{200}, Output: map[string]string{}},
	}
}

func mustArtifact(t *testing.T, task *explore.Task, ops []integration.Operation) *integration.Compiled {
	t.Helper()
	in := integration.Integration{Format: integration.Format, ID: task.IntegrationID, Version: "explore",
		Source: integration.Source{Kind: "web", Origin: task.Origin}, Session: task.Session, Operations: ops}
	b, _ := json.Marshal(in)
	c, err := integration.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func keys(m map[string]explore.Observation) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExploreGraphQLReadsThroughWriteGuard covers a single-page app that
// reads over POST /api/graphql (as Zendesk's agent UI does). In a read-only
// walk the query must reach the app and be recorded per operation, while the
// mutation stays blocked and creates nothing.
func TestExploreGraphQLReadsThroughWriteGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	app := notesapp.New("synthetic-user", "synthetic-pass")
	srv := httptest.NewServer(app)
	defer srv.Close()
	creds := map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := browser.Open(ctx, browser.Options{ProfileDir: filepath.Join(t.TempDir(), "profile"), Headless: true})
	if err != nil {
		t.Skipf("Chrome is not available: %v", err)
	}
	defer b.Close()

	task := taskFor(srv.URL)
	task.GraphQLReadPaths = []string{"/api/graphql"}
	task.Walk = []explore.Step{
		{Navigate: "/gql-app", WaitMS: 800},
		{Click: "#create", WaitMS: 800, Note: "a GraphQL mutation; must be blocked"},
	}
	si := mustArtifact(t, task, []integration.Operation{readOp("signed_in_check", "/api/me")})
	rep, err := explore.Explore(ctx, b, task, si, creds)
	if err != nil {
		t.Fatalf("explore: %v", err)
	}
	ops := map[string]explore.Observation{}
	for _, o := range rep.Observations {
		if o.GraphQLOperation != "" {
			ops[o.GraphQLOperation] = o
		}
	}
	list, ok := ops["ListNotes"]
	if !ok || list.Blocked || !containsStatus(list.Statuses, 200) || list.ResponseShape == nil {
		t.Fatalf("ListNotes query was not let through and recorded: %+v (all: %+v)", list, rep.Observations)
	}
	if !strings.HasPrefix(list.GraphQLQuery, "query ListNotes") {
		t.Errorf("query document not recorded: %q", list.GraphQLQuery)
	}
	if c, ok := ops["CreateNote"]; !ok || !c.Blocked {
		t.Errorf("CreateNote mutation was not blocked: %+v", c)
	}
	if app.Creates() != 0 {
		t.Fatalf("explorer let a GraphQL mutation through: creates=%d", app.Creates())
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "Seed note") || strings.Contains(string(raw), "from gql ui") {
		t.Error("observations contain response or variable values")
	}
}

func containsStatus(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestExploreFollowsNewTabs covers a click that opens a second tab (as a
// person demonstrating often does). The new tab's traffic must be recorded,
// and in a read-only task its write must be blocked like the first tab's.
func TestExploreFollowsNewTabs(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	app := notesapp.New("synthetic-user", "synthetic-pass")
	srv := httptest.NewServer(app)
	defer srv.Close()
	creds := map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := browser.Open(ctx, browser.Options{ProfileDir: filepath.Join(t.TempDir(), "profile"), Headless: true})
	if err != nil {
		t.Skipf("Chrome is not available: %v", err)
	}
	defer b.Close()

	task := taskFor(srv.URL)
	task.Walk = []explore.Step{
		{Navigate: "/opener", WaitMS: 300},
		{Click: "#open", WaitMS: 4000, Note: "opens /popup-app in a new tab, which reads, then tries a write"},
	}
	si := mustArtifact(t, task, []integration.Operation{readOp("signed_in_check", "/api/me")})
	rep, err := explore.Explore(ctx, b, task, si, creds)
	if err != nil {
		t.Fatalf("explore: %v", err)
	}
	routes := map[string]explore.Observation{}
	for _, o := range rep.Observations {
		routes[o.Method+" "+o.PathTemplate] = o
	}
	if o, ok := routes["GET /api/notes/{note_id}"]; !ok || !containsStatus(o.Statuses, 200) {
		t.Errorf("the new tab's read was not recorded; got %v", keys(routes))
	}
	if o, ok := routes["POST /api/notes"]; !ok || !o.Blocked {
		t.Errorf("the new tab's write was not blocked and recorded: %+v", o)
	}
	if app.Creates() != 0 {
		t.Fatalf("a write from a new tab got through a read-only exploration: creates=%d", app.Creates())
	}
	gap := false
	for _, g := range rep.Coverage.Gaps {
		gap = gap || strings.Contains(g, "other tab")
	}
	if !gap {
		t.Errorf("report does not name the extra tab as a gap: %v", rep.Coverage.Gaps)
	}
}

// TestReadOnlyWalkBlocksEveryWriteVector covers writes that leave before a
// new tab could be attached (a form posted to a new window, a new window
// that writes at once) and writes outside any tab (a worker, a beacon), plus
// a GraphQL mutation sent over GET. In a read-only walk none may reach the
// app.
func TestReadOnlyWalkBlocksEveryWriteVector(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	run := func(t *testing.T, vector string, effects []string) (int, *explore.Report) {
		app := notesapp.New("synthetic-user", "synthetic-pass")
		srv := httptest.NewServer(app)
		defer srv.Close()
		creds := map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		b, err := browser.Open(ctx, browser.Options{ProfileDir: filepath.Join(t.TempDir(), "profile"), Headless: true})
		if err != nil {
			t.Skipf("Chrome is not available: %v", err)
		}
		defer b.Close()
		task := taskFor(srv.URL)
		task.AllowEffects = effects
		task.GraphQLReadPaths = []string{"/api/graphql"}
		task.Walk = []explore.Step{
			{Navigate: "/writers", WaitMS: 300},
			{Click: "#" + vector, WaitMS: 2500},
		}
		si := mustArtifact(t, task, []integration.Operation{readOp("signed_in_check", "/api/me")})
		rep, err := explore.Explore(ctx, b, task, si, creds)
		if err != nil {
			t.Fatalf("explore: %v", err)
		}
		if len(effects) == 1 {
			// Whatever the walk held back must stay stopped after it ends, for
			// as long as the browser stays open.
			time.Sleep(1500 * time.Millisecond)
		}
		return app.Creates(), rep
	}
	for _, vector := range []string{"form", "popup", "worker", "beacon", "gqlget", "apqget", "ws", "wsworker", "wsnested", "wsshared", "wsframe", "ping", "sw", "latewrite", "latepopup", "latechurn", "lateopener"} {
		t.Run(vector, func(t *testing.T) {
			// Control: with writes allowed, the vector really writes.
			if n, _ := run(t, vector, []string{"read", "write"}); n == 0 {
				t.Fatalf("control: the %s vector did not write even with writes allowed", vector)
			}
			n, rep := run(t, vector, []string{"read"})
			if n != 0 {
				t.Fatalf("the %s write reached the app in a read-only walk: creates=%d", vector, n)
			}
			if vector == "wsframe" || vector == "sw" { // held paused, and the report says so
				held := false
				for _, g := range rep.Coverage.Gaps {
					held = held || strings.Contains(g, "held paused")
				}
				if !held {
					t.Errorf("the held frame is not named as a gap: %v", rep.Coverage.Gaps)
				}
				return
			}
			if strings.HasPrefix(vector, "ws") { // not a request the guard intercepts; named as a gap
				gap := false
				for _, g := range rep.Coverage.Gaps {
					gap = gap || strings.Contains(g, "WebSocket frames are not checked by the write guard")
				}
				if !gap {
					t.Errorf("the WebSocket attempt is not named as a gap: %v", rep.Coverage.Gaps)
				}
				return
			}
			if rep.Coverage.BlockedWrites == 0 {
				t.Errorf("the %s write was not recorded as blocked", vector)
			}
		})
	}
}
