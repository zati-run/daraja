package runtime_test

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

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/internal/notesapp"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/runtime"
	"github.com/zati-run/daraja/session"
)

type formEnv struct {
	app *notesapp.App
	rt  *runtime.Runtime
	p   *runtime.Principal
	// beforePost runs before the app sees a form submission.
	beforePost func()
}

func newFormEnv(t *testing.T, timeout time.Duration) *formEnv {
	t.Helper()
	e := &formEnv{app: notesapp.New("synthetic-user", "synthetic-pass")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/notes" && e.beforePost != nil {
			e.beforePost()
		}
		e.app.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	read := func(path string) map[string]any {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// The same operations as the capability fixture, but with the session in
	// the runtime.
	art := read("../integration/testdata/notes-form.json")
	art["session"] = read("../integration/testdata/notes.json")["session"]
	art["source"].(map[string]any)["origin"] = srv.URL
	b, err := json.Marshal(art)
	if err != nil {
		t.Fatal(err)
	}
	in, err := integration.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "j.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	e.rt, err = runtime.New(runtime.Config{
		Integrations: []*integration.Compiled{in},
		Principals:   []runtime.Principal{{ID: "p", TokenSHA256: runtime.HashToken("t"), Grants: []runtime.Grant{{Integration: in.ID, Operations: []string{"get_note", "find_note_by_ref", "create_note"}, Credential: "alice"}}}},
		Credentials:  map[string]session.Binding{"alice": {Values: map[string]string{"username": "synthetic-user", "password": "synthetic-pass"}}},
		Journal:      j, RequestTimeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.p, _ = e.rt.Principal("p")
	return e
}

func (e *formEnv) create(title, key string) (*runtime.Result, *runtime.Error) {
	return e.rt.Invoke(context.Background(), e.p, "synthetic-notes-form", "create_note", map[string]any{"title": title}, key)
}

func TestFormRead(t *testing.T) {
	t.Parallel()
	e := newFormEnv(t, 5*time.Second)
	res, err := e.rt.Invoke(context.Background(), e.p, "synthetic-notes-form", "get_note", map[string]any{"id": 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output["id"] != 1.0 || res.Output["title"] != "Seed note" {
		t.Errorf("output = %v", res.Output)
	}
}

func TestFormWriteRedirectIsSuccessOnce(t *testing.T) {
	t.Parallel()
	e := newFormEnv(t, 5*time.Second)
	res, err := e.create("Form note", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Receipt.State != journal.Succeeded || res.Receipt.Identifier != "2" || res.Output["id"] != 2.0 {
		t.Fatalf("receipt %+v output %v", res.Receipt, res.Output)
	}
	again, err := e.create("Form note", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Output["id"] != 2.0 || e.app.Creates() != 1 {
		t.Errorf("replay output %v, creates %d", again.Output, e.app.Creates())
	}
}

func TestFormWriteOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		title      string
		arrange    func(*formEnv)
		code       string
		state      string
		wantWrites int
	}{
		{"validation page is a declared failure", "", nil, runtime.CodeSourceRefused, journal.Failed, 0},
		{"form drift sends nothing", "Drift", func(e *formEnv) { e.app.FormDrift = true }, runtime.CodeSourceChanged, journal.Failed, 0},
		{"session gone at the form page sends nothing", "Expired", func(e *formEnv) { e.app.ExpireSessions() }, runtime.CodeSessionUnavailable, journal.Failed, 0},
		{"redirected to sign-in after submitting stays uncertain", "Lost", func(e *formEnv) { e.beforePost = e.app.ExpireSessions }, runtime.CodeOutcomeUncertain, journal.Uncertain, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newFormEnv(t, 5*time.Second)
			// Sign in first, so that arrange acts on an established session.
			if _, err := e.rt.Invoke(context.Background(), e.p, "synthetic-notes-form", "get_note", map[string]any{"id": 1}, ""); err != nil {
				t.Fatal(err)
			}
			if tc.arrange != nil {
				tc.arrange(e)
			}
			_, err := e.create(tc.title, "k")
			if err == nil {
				t.Fatal("want an error")
			}
			if err.Code != tc.code || err.Receipt == nil || err.Receipt.State != tc.state {
				t.Fatalf("code %s receipt %+v, want %s %s (%s)", err.Code, err.Receipt, tc.code, tc.state, err.Message)
			}
			if e.app.Creates() != tc.wantWrites {
				t.Errorf("creates = %d, want %d", e.app.Creates(), tc.wantWrites)
			}
			if tc.state == journal.Failed && strings.Contains(err.Message, "source_changed") {
				t.Errorf("message leaks a code: %s", err.Message)
			}
		})
	}
}

func TestFormPageOverLimitSendsNothing(t *testing.T) {
	t.Parallel()
	e := newFormEnv(t, 5*time.Second)
	if _, err := e.rt.Invoke(context.Background(), e.p, "synthetic-notes-form", "get_note", map[string]any{"id": 1}, ""); err != nil {
		t.Fatal(err)
	}
	e.app.FormPadding = 8<<20 + 1
	_, err := e.create("Big", "k")
	if err == nil || err.Code != runtime.CodeSourceChanged || err.Receipt == nil || err.Receipt.State != journal.Failed {
		t.Fatalf("err = %+v", err)
	}
	if n := e.app.Creates(); n != 0 {
		t.Errorf("creates = %d, want 0", n)
	}
}

func TestFormLostAnswerReconcilesFromTheList(t *testing.T) {
	t.Parallel()
	e := newFormEnv(t, 500*time.Millisecond)
	e.app.SlowTitle, e.app.SlowFor = "Slow", 1500*time.Millisecond
	_, err := e.create("Slow", "k")
	if err == nil || err.Code != runtime.CodeOutcomeUncertain {
		t.Fatalf("err = %v", err)
	}
	rec, err := e.rt.Reconcile(context.Background(), e.p, "synthetic-notes-form", "create_note", "k")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.State != journal.Succeeded || rec.Identifier != "2" || e.app.Creates() != 1 {
		t.Errorf("receipt %+v creates %d", rec, e.app.Creates())
	}
}
