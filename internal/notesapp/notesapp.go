// Package notesapp is a small synthetic web application for Daraja tests.
// It has a form login with a CSRF token, a session cookie, a JSON API, and
// a CSRF-protected create endpoint. It holds only synthetic data.
package notesapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App is the synthetic application.
type App struct {
	User, Pass string
	// SlowTitle makes a create with this title commit, then wait before
	// answering, so tests can lose or interrupt the response.
	SlowTitle string
	SlowFor   time.Duration
	// RedirectUnauthenticated makes the API redirect a request without a
	// session to the sign-in page, as many web apps do, instead of 401.
	RedirectUnauthenticated bool

	mu       sync.Mutex
	sessions map[string]bool
	notes    []Note
	creates  int
	logins   int
}

// Note is a stored note.
type Note struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

// New returns an app with synthetic credentials and one seed note.
func New(user, pass string) *App {
	return &App{User: user, Pass: pass, sessions: map[string]bool{}, notes: []Note{{ID: 1, Title: "Seed note", Body: "synthetic"}}}
}

// Creates reports how many create requests were applied.
func (a *App) Creates() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creates
}

// Logins reports how many successful logins happened.
func (a *App) Logins() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logins
}

// ExpireSessions forgets every session.
func (a *App) ExpireSessions() {
	a.mu.Lock()
	a.sessions = map[string]bool{}
	a.mu.Unlock()
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) sid(r *http.Request) (string, bool) {
	c, err := r.Cookie("sid")
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return c.Value, a.sessions[c.Value]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ServeHTTP implements the application.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/login" && r.Method == http.MethodGet:
		pre := token()
		http.SetCookie(w, &http.Cookie{Name: "pre", Value: pre, Path: "/"})
		fmt.Fprintf(w, `<html><form method=post><input type="hidden" name="csrf" value="%s"><input name=user><input name=pass type=password><button type=submit>Sign in</button></form></html>`, pre)
	case r.URL.Path == "/login" && r.Method == http.MethodPost:
		pre, err := r.Cookie("pre")
		if err != nil || r.FormValue("csrf") != pre.Value {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		if r.FormValue("user") != a.User || r.FormValue("pass") != a.Pass {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		s := token()
		a.mu.Lock()
		a.sessions[s] = true
		a.logins++
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: s, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/app", http.StatusSeeOther)
	case r.URL.Path == "/app":
		if _, ok := a.sid(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		fmt.Fprint(w, appPage)
	case r.URL.Path == "/writers":
		fmt.Fprint(w, writersPage)
	case r.URL.Path == "/worker.js":
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, `fetch('/api/sink', {method: 'POST', body: 'worker'});`)
	case r.URL.Path == "/popup-now":
		fmt.Fprint(w, `<!doctype html><html><body><script>fetch('/api/sink', {method: 'POST', body: 'popup'});</script></body></html>`)
	case r.URL.Path == "/api/sink" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions:
		// Counts any write that reaches the app, from any vector.
		a.mu.Lock()
		a.creates++
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/ws":
		// A WebSocket handshake that reaches the app counts as a write
		// channel opened.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			a.mu.Lock()
			a.creates++
			a.mu.Unlock()
		}
		http.Error(w, "no sockets here", http.StatusBadRequest)
	case r.URL.Path == "/sw.js":
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, `self.addEventListener('install', e => e.waitUntil(fetch('/api/sink', {method: 'POST', body: 'sw'}).catch(() => {})));`)
	case r.URL.Path == "/api/graphql" && r.Method == http.MethodGet:
		q := r.URL.Query()
		// A hash-only persisted query: this fixture treats it as the
		// CreateNote mutation, as a real server could.
		persistedMutation := q.Get("query") == "" && q.Get("extensions") != "" && q.Get("operationName") == "CreateNote"
		if persistedMutation || strings.HasPrefix(strings.TrimSpace(q.Get("query")), "mutation") {
			a.mu.Lock()
			a.creates++
			a.mu.Unlock()
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{}})
	case r.URL.Path == "/opener":
		fmt.Fprint(w, `<!doctype html><html><body><button id=open onclick="window.open('/popup-app', '_blank')">Open</button></body></html>`)
	case r.URL.Path == "/popup-app":
		if _, ok := a.sid(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		fmt.Fprint(w, popupPage)
	case r.URL.Path == "/gql-app":
		if _, ok := a.sid(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		fmt.Fprint(w, gqlPage)
	case r.URL.Path == "/api/graphql" && r.Method == http.MethodPost:
		s, ok := a.sid(r)
		if !ok {
			writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
			return
		}
		a.graphQL(w, r, s)
	case r.URL.Path == "/api/me":
		s, ok := a.sid(r)
		if !ok {
			writeJSON(w, 200, map[string]any{"user": map[string]any{"id": nil}})
			return
		}
		writeJSON(w, 200, map[string]any{"user": map[string]any{"id": 7}, "csrf": "c-" + s})
	case strings.HasPrefix(r.URL.Path, "/api/notes"):
		s, ok := a.sid(r)
		if !ok && a.RedirectUnauthenticated {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !ok {
			writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
			return
		}
		a.notesAPI(w, r, s)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) notesAPI(w http.ResponseWriter, r *http.Request, sid string) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/notes")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		ref := r.URL.Query().Get("ref")
		a.mu.Lock()
		out := []Note{}
		for _, n := range a.notes {
			if ref == "" || n.Ref == ref {
				out = append(out, n)
			}
		}
		a.mu.Unlock()
		writeJSON(w, 200, map[string]any{"notes": out})
	case rest == "" && r.Method == http.MethodPost:
		if r.Header.Get("X-CSRF-Token") != "c-"+sid {
			writeJSON(w, 403, map[string]any{"error": "csrf"})
			return
		}
		var in struct {
			Note Note `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Note.Title == "" {
			writeJSON(w, 422, map[string]any{"error": "title required"})
			return
		}
		a.mu.Lock()
		in.Note.ID = len(a.notes) + 1
		a.notes = append(a.notes, in.Note)
		a.creates++
		a.mu.Unlock()
		if a.SlowTitle != "" && in.Note.Title == a.SlowTitle {
			time.Sleep(a.SlowFor)
		}
		writeJSON(w, 201, map[string]any{"note": in.Note})
	case r.Method == http.MethodGet:
		id, err := strconv.Atoi(strings.TrimPrefix(rest, "/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, n := range a.notes {
			if n.ID == id {
				writeJSON(w, 200, map[string]any{"note": n})
				return
			}
		}
		writeJSON(w, 404, map[string]any{"error": "not found"})
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// graphQL is a tiny GraphQL-over-POST endpoint, as single-page apps often
// use for reads: ListNotes is a query; CreateNote is a mutation that
// creates a note (it needs the CSRF token, like the REST create).
func (a *App) graphQL(w http.ResponseWriter, r *http.Request, sid string) {
	var in struct {
		OperationName string         `json:"operationName"`
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "bad request"}}})
		return
	}
	switch {
	case strings.HasPrefix(strings.TrimSpace(in.Query), "mutation"):
		if r.Header.Get("X-CSRF-Token") != "c-"+sid {
			writeJSON(w, 403, map[string]any{"errors": []any{map[string]any{"message": "csrf"}}})
			return
		}
		title, _ := in.Variables["title"].(string)
		a.mu.Lock()
		n := Note{ID: len(a.notes) + 1, Title: title}
		a.notes = append(a.notes, n)
		a.creates++
		a.mu.Unlock()
		writeJSON(w, 200, map[string]any{"data": map[string]any{"createNote": n}})
	default:
		a.mu.Lock()
		out := append([]Note{}, a.notes...)
		a.mu.Unlock()
		writeJSON(w, 200, map[string]any{"data": map[string]any{"notes": out}})
	}
}

// writersPage tries every write vector a read-only walk must block: a form
// posted to a new window, a new window that writes at once, a dedicated
// worker, a beacon, and a GraphQL mutation over GET. Each lands on a route
// that counts a create.
const writersPage = `<!doctype html><html><body>
<form id=f method=post action="/api/sink" target="_blank"><input name=x value=1></form>
<button id=form onclick="document.getElementById('f').submit()">form</button>
<button id=popup onclick="window.open('/popup-now', '_blank')">popup</button>
<button id=worker onclick="new Worker('/worker.js')">worker</button>
<button id=beacon onclick="navigator.sendBeacon('/api/sink', 'beacon')">beacon</button>
<button id=apqget onclick="fetch('/api/graphql?operationName=CreateNote&extensions=' + encodeURIComponent('{&quot;persistedQuery&quot;:{&quot;version&quot;:1,&quot;sha256Hash&quot;:&quot;ab&quot;}}'))">apqget</button>
<button id=ws onclick="try { new WebSocket('ws://' + location.host + '/ws') } catch (e) {}">ws</button>
<a id=ping href="/writers#pinged" ping="/api/sink">ping</a>
<button id=sw onclick="navigator.serviceWorker.register('/sw.js')">sw</button>
<button id=gqlget onclick="fetch('/api/graphql?query=' + encodeURIComponent('mutation M { createNote(title: &quot;x&quot;) { id } }'))">gqlget</button>
</body></html>`

// popupPage opens in a second tab. After a pause (so a recorder can attach),
// it reads a note and then tries to create one.
const popupPage = `<!doctype html><html><body><script>
setTimeout(async () => {
  await fetch('/api/notes/1');
  const me = await (await fetch('/api/me')).json();
  await fetch('/api/notes', {method: 'POST', headers: {'Content-Type': 'application/json', 'X-CSRF-Token': me.csrf}, body: JSON.stringify({note: {title: 'from popup'}})});
  document.title = 'done';
}, 1500);
</script></body></html>`

// gqlPage is a synthetic UI that reads and writes through GraphQL over POST.
const gqlPage = `<!doctype html><html><body>
<h1>Notes</h1><ul id=list></ul><button id=create>Create</button>
<script>
async function gql(body, headers) {
  return (await fetch('/api/graphql', {method: 'POST', headers: Object.assign({'Content-Type': 'application/json'}, headers || {}), body: JSON.stringify(body)})).json();
}
async function load() {
  const r = await gql({operationName: 'ListNotes', query: 'query ListNotes($first: Int = 10) { notes(first: $first) { id title } }', variables: {first: 10}});
  document.getElementById('list').innerHTML = r.data.notes.map(n => '<li>' + n.title + '</li>').join('');
}
document.getElementById('create').onclick = async () => {
  const me = await (await fetch('/api/me')).json();
  await gql({operationName: 'CreateNote', query: 'mutation CreateNote($title: String!) { createNote(title: $title) { id } }', variables: {title: 'from gql ui'}}, {'X-CSRF-Token': me.csrf});
};
load();
</script></body></html>`

// appPage is the synthetic UI. It reads through the JSON API and has a
// button that creates a note with the CSRF token.
const appPage = `<!doctype html><html><body>
<h1>Notes</h1><ul id=list></ul><button id=create>Create</button>
<script>
async function load() {
  const one = await (await fetch('/api/notes/1')).json();
  const all = await (await fetch('/api/notes?ref=')).json();
  document.getElementById('list').innerHTML = all.notes.map(n => '<li>' + n.title + '</li>').join('') + '<li>' + one.note.title + '</li>';
}
document.getElementById('create').onclick = async () => {
  const me = await (await fetch('/api/me')).json();
  await fetch('/api/notes', {method: 'POST', headers: {'Content-Type': 'application/json', 'X-CSRF-Token': me.csrf},
    body: JSON.stringify({note: {title: 'From UI', body: 'synthetic'}})});
};
load();
</script></body></html>`
