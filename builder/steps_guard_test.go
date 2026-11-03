package builder_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zati-run/daraja/builder/browser"
	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
)

// guardPages serves a page per path and counts POSTs.
type guardPages struct {
	*httptest.Server
	posts atomic.Int32
}

func servePages(t *testing.T, pages map[string]string) *guardPages {
	t.Helper()
	g := &guardPages{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			g.posts.Add(1)
		}
		page, ok := pages[r.URL.Path]
		if !ok {
			page = "<!doctype html><body>nothing here</body>"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	t.Cleanup(g.Close)
	return g
}

func runPlan(t *testing.T, origin string, timeout time.Duration, steps ...integration.Step) capability.StepsResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	b, err := browser.Open(ctx, browser.Options{ProfileDir: filepath.Join(t.TempDir(), "profile"), Headless: true, StepTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	in := &integration.Compiled{Integration: &integration.Integration{}}
	in.Source.Origin = origin
	return b.RunSteps(ctx, in, steps)
}

func TestStepsFillCannotSubmit(t *testing.T) {
	form := `<!doctype html><body><form method="post" action="/done"><input id="f" name="f"><button id="b">Go</button></form></body>`
	for _, value := range []string{"a\nb", "a\rb", "\r"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			g := servePages(t, map[string]string{"/": form})
			res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"}, integration.Step{Fill: "#f", Value: value})
			if res.Err == nil || !res.Drift || res.Committed {
				t.Errorf("err=%v drift=%v committed=%v, want drift before commit", res.Err, res.Drift, res.Committed)
			}
			if n := g.posts.Load(); n != 0 {
				t.Errorf("the fill submitted the form %d times", n)
			}
		})
	}
}

func TestStepsFillStaysOnItsField(t *testing.T) {
	// The field's handler moves focus to one that saves whatever reaches it.
	page := `<!doctype html><body><input id="f" value="draft" oninput="document.getElementById('auto').focus()">
<input id="auto" oninput="fetch('/save', {method: 'POST'})"></body>`
	g := servePages(t, map[string]string{"/": page})
	res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"}, integration.Step{Fill: "#f", Value: "x"}, integration.Step{WaitFor: "#f"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := g.posts.Load(); n != 0 {
		t.Errorf("the other field saved %d times", n)
	}
}

func TestStepsClickIgnoresSmoothScrolling(t *testing.T) {
	// While the page scrolls smoothly, a saved point would land on #submit.
	page := `<!doctype html><html style="scroll-behavior:smooth"><body style="margin:0">
<script>document.addEventListener("click", e => { if (e.target.id === "submit") fetch("/wrong", {method: "POST"}) }, true)</script>
<button id="submit" style="position:absolute;top:0;left:0;width:100%;height:3000px">Submit</button>
<button id="target" style="position:absolute;top:2800px;left:0;width:100%;height:40px;z-index:1">Open</button></body></html>`
	g := servePages(t, map[string]string{"/": page})
	res := runPlan(t, g.URL, 2*time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "#target"}, integration.Step{WaitFor: "#target"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := g.posts.Load(); n != 0 {
		t.Errorf("the click reached the other control %d times", n)
	}
}

func TestStepsActionRechecksExpect(t *testing.T) {
	// The assertion holds at first; when the target appears, it no longer does.
	page := `<!doctype html><body><h1 id="state">Ready</h1><script>setTimeout(() => {
  document.getElementById("state").textContent = "Changed";
  const b = document.createElement("button"); b.id = "b";
  b.onclick = () => fetch("/go", {method: "POST"});
  document.body.append(b); b.textContent = "Go";
}, 200)</script></body>`
	g := servePages(t, map[string]string{"/": page})
	res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"},
		integration.Step{Click: "#b", Expect: []integration.Expect{{CSS: "h1", Text: "Ready"}}})
	if res.Err == nil || !res.Drift {
		t.Errorf("err=%v drift=%v, want drift", res.Err, res.Drift)
	}
	if n := g.posts.Load(); n != 0 {
		t.Errorf("the action ran %d times after its assertion stopped holding", n)
	}
}

func TestStepsFillKeepsLinesInATextarea(t *testing.T) {
	g := servePages(t, map[string]string{"/": `<!doctype html><body><textarea id="f">old</textarea></body>`})
	res := runPlan(t, g.URL, 2*time.Second, integration.Step{Navigate: "/"}, integration.Step{Fill: "#f", Value: "one\ntwo"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(string(res.Body), "<textarea") {
		t.Errorf("final page: %s", res.Body)
	}
}

func TestStepsClickHitsOnlyItsTarget(t *testing.T) {
	counter := `<script>document.addEventListener("click", e => { fetch("/hit", {method: "POST"}) }, true)</script>`
	cases := []struct {
		name string
		page string
		text string
		hits int32
	}{
		{"a hidden match is skipped", `<button style="display:none">Save</button><button id="real">Save</button>`, "Save", 1},
		{"an invisible match is skipped", `<button style="visibility:hidden">Save</button><button id="real">Save</button>`, "Save", 1},
		{"a covered target isn't clicked", `<button id="real" style="position:fixed;left:0;top:0;width:100px;height:40px">Save</button><div style="position:fixed;left:0;top:0;width:200px;height:80px;background:#000"></div>`, "Save", 0},
		{"a disabled target isn't clicked", `<button id="real" disabled>Save</button>`, "Save", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := servePages(t, map[string]string{"/": `<!doctype html><body>` + counter + tc.page + `</body>`})
			res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "button", Text: tc.text})
			if tc.hits == 0 && (res.Err == nil || !res.Drift) {
				t.Errorf("err=%v drift=%v, want drift", res.Err, res.Drift)
			}
			if tc.hits > 0 && res.Err != nil {
				t.Error(res.Err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for tc.hits > 0 && g.posts.Load() < tc.hits && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if n := g.posts.Load(); n != tc.hits {
				t.Errorf("clicks = %d, want %d", n, tc.hits)
			}
		})
	}
}

func TestStepsStayOnTheOrigin(t *testing.T) {
	// The other origin has what every step looks for and counts what it gets.
	other := servePages(t, map[string]string{"/": `<!doctype html><body><h1>Ready</h1><form method="post" action="/x"><input id="f" name="f"><button id="b">Go</button></form></body>`})
	leave := fmt.Sprintf(`<!doctype html><body>moving<script>setTimeout(() => { location.href = %q }, 150)</script></body>`, other.URL+"/")

	t.Run("a page that moves during the check", func(t *testing.T) {
		g := servePages(t, map[string]string{"/": leave})
		res := runPlan(t, g.URL, 1500*time.Millisecond, integration.Step{Navigate: "/"},
			integration.Step{Click: "#b", Commit: true, Expect: []integration.Expect{{CSS: "h1", Text: "Ready"}}})
		if res.Err == nil || !res.Drift || res.Committed {
			t.Errorf("err=%v drift=%v committed=%v, want drift before commit", res.Err, res.Drift, res.Committed)
		}
		if n := other.posts.Load(); n != 0 {
			t.Errorf("the other origin got %d submissions", n)
		}
	})
	t.Run("a page that moves after the commit", func(t *testing.T) {
		g := servePages(t, map[string]string{"/": `<!doctype html><body><button id="b" onclick="location.href='` + other.URL + `/'">Go</button></body>`})
		res := runPlan(t, g.URL, 1500*time.Millisecond, integration.Step{Navigate: "/"}, integration.Step{Click: "#b", Commit: true},
			integration.Step{Click: "#b"})
		if res.Err == nil || !res.Committed {
			t.Errorf("err=%v committed=%v, want a failure after commit", res.Err, res.Committed)
		}
		if n := other.posts.Load(); n != 0 {
			t.Errorf("the other origin got %d submissions", n)
		}
	})
}

func TestStepsExpectAfterCommitIsChecked(t *testing.T) {
	g := servePages(t, map[string]string{"/": `<!doctype html><body><button id="b">Go</button></body>`})
	res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "#b", Commit: true},
		integration.Step{WaitFor: "#b", Expect: []integration.Expect{{CSS: "#never"}}})
	if res.Err == nil || !res.Committed || !res.Drift {
		t.Errorf("err=%v committed=%v drift=%v, want drift after commit", res.Err, res.Committed, res.Drift)
	}
}

func TestStepsCommitExpectFailureComesBeforeTheAction(t *testing.T) {
	g := servePages(t, map[string]string{"/": `<!doctype html><body><form method="post" action="/x"><button id="b">Go</button></form></body>`})
	res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"},
		integration.Step{Click: "#b", Commit: true, Expect: []integration.Expect{{CSS: "#never"}}})
	if res.Err == nil || res.Committed {
		t.Errorf("err=%v committed=%v, want a failure before the commit action", res.Err, res.Committed)
	}
	if n := g.posts.Load(); n != 0 {
		t.Errorf("posts = %d", n)
	}
}

func TestStepsDialogStopsAtTheTimeout(t *testing.T) {
	g := servePages(t, map[string]string{"/": `<!doctype html><body><button id="b" onclick="alert('x')">Go</button></body>`})
	start := time.Now()
	res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "#b", Commit: true},
		integration.Step{WaitFor: "#never"})
	if res.Err == nil || !res.Committed {
		t.Errorf("err=%v committed=%v, want a failure after commit", res.Err, res.Committed)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestStepsFailureOmitsPageText(t *testing.T) {
	page := `<!doctype html><body><script>
JSON.stringify = () => '"SECRET-PAGE-TEXT"';
const q = Document.prototype.querySelectorAll;
Document.prototype.querySelectorAll = function () { throw new Error("SECRET-PAGE-TEXT") };
</script></body>`
	g := servePages(t, map[string]string{"/": page})
	res := runPlan(t, g.URL, time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "#never"})
	if res.Err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(res.Err.Error(), "SECRET") {
		t.Errorf("page text in the error: %v", res.Err)
	}
}

func TestStepsBrokenBuiltinsDontRepeatAClick(t *testing.T) {
	// A page that breaks built-ins can't make the runner repeat the action.
	page := `<!doctype html><body><button id="b" onclick="fetch('/go', {method: 'POST'})">Go</button><script>
let n = 0;
const real = JSON.stringify;
JSON.stringify = function (...a) { if (n++ < 3) throw new Error("broken"); return real.apply(this, a) };
</script></body>`
	g := servePages(t, map[string]string{"/": page})
	res := runPlan(t, g.URL, 2*time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "#b", Commit: true}, integration.Step{WaitFor: "#b"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for g.posts.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := g.posts.Load(); n != 1 {
		t.Errorf("clicks = %d, want 1", n)
	}
}

func TestStepsFinalPageComesWithItsOwnURL(t *testing.T) {
	other := servePages(t, map[string]string{"/": `<!doctype html><body><p id="id">foreign</p></body>`})
	g := servePages(t, map[string]string{"/": `<!doctype html><body><button id="b" onclick="location.href='` + other.URL + `/'">Go</button></body>`})
	res := runPlan(t, g.URL, 2*time.Second, integration.Step{Navigate: "/"}, integration.Step{Click: "#b", Commit: true}, integration.Step{WaitPathNot: "/nope"})
	// The page may or may not have moved when the plan ends. Either way, a
	// foreign body never comes back as the answer.
	if strings.Contains(string(res.Body), "foreign") {
		t.Errorf("a page from another origin was returned: %s (%s)", res.Body, res.Location)
	}
}
