package oauth

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zati-run/daraja/runtime"
)

// Paths served by Routes.
const (
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
	RevokePath    = "/oauth/revoke"
)

// Routes registers the metadata, authorization, token and revocation
// endpoints.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handleResourceMeta)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleResourceMeta)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleServerMeta)
	mux.HandleFunc("GET "+AuthorizePath, s.handleAuthorize)
	mux.HandleFunc("POST "+AuthorizePath, s.handleConsent)
	mux.HandleFunc("POST "+TokenPath, s.handleToken)
	mux.HandleFunc("POST "+RevokePath, s.handleRevoke)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleResourceMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resource,
		"authorization_servers":    []string{s.cfg.Issuer},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Daraja",
	})
}

func (s *Server) handleServerMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.cfg.Issuer,
		"authorization_endpoint":                         s.cfg.Issuer + AuthorizePath,
		"token_endpoint":                                 s.cfg.Issuer + TokenPath,
		"revocation_endpoint":                            s.cfg.Issuer + RevokePath,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// redirectError sends an OAuth error to a validated redirect URI.
func redirectError(w http.ResponseWriter, r *http.Request, redirect, state, issuer, code, desc string) {
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("error", code)
	if desc != "" {
		q.Set("error_description", desc)
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func pageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

func errorPage(w http.ResponseWriter, status int, msg string) {
	pageHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = errorTmpl.Execute(w, msg)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID, redirect := q.Get("client_id"), q.Get("redirect_uri")
	md, err := s.client(r.Context(), clientID)
	if err != nil {
		// Never redirect to an unverified URI.
		errorPage(w, http.StatusBadRequest, "This client could not be verified: "+err.Error())
		return
	}
	if !slices.Contains(md.RedirectURIs, redirect) {
		errorPage(w, http.StatusBadRequest, "The redirect_uri is not one the client's metadata document lists.")
		return
	}
	state := q.Get("state")
	switch {
	case q.Get("response_type") != "code":
		redirectError(w, r, redirect, state, s.cfg.Issuer, "unsupported_response_type", "only code is supported")
		return
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 43 || len(q.Get("code_challenge")) > 128:
		redirectError(w, r, redirect, state, s.cfg.Issuer, "invalid_request", "PKCE with S256 is required")
		return
	case q.Get("resource") != "" && q.Get("resource") != s.resource:
		redirectError(w, r, redirect, state, s.cfg.Issuer, "invalid_target", "unknown resource")
		return
	}
	id := randomToken()
	s.mu.Lock()
	for k, p := range s.pending {
		if time.Now().After(p.expires) {
			delete(s.pending, k)
		}
	}
	if len(s.pending) >= maxPending {
		s.mu.Unlock()
		errorPage(w, http.StatusServiceUnavailable, "Too many authorization requests are open. Try again later.")
		return
	}
	s.pending[id] = &pendingAuth{client: md, redirect: redirect, challenge: q.Get("code_challenge"), state: state, expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	s.renderConsent(w, id, md, redirect, "")
}

type consentOp struct {
	Value, Name, Description string
	Write, Destructive       bool
}

type consentIntegration struct {
	ID, Description string
	Ops             []consentOp
}

func (s *Server) renderConsent(w http.ResponseWriter, id string, md *ClientMetadata, redirect, problem string) {
	var integs []consentIntegration
	for _, in := range s.cfg.Catalog.Integrations() {
		ci := consentIntegration{ID: in.ID, Description: in.Description}
		for _, op := range in.Operations {
			ci.Ops = append(ci.Ops, consentOp{Value: in.ID + "/" + op.Name, Name: op.Name, Description: op.Description, Write: op.Effect == "write", Destructive: op.IsDestructive()})
		}
		integs = append(integs, ci)
	}
	ru, _ := url.Parse(redirect)
	pageHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentTmpl.Execute(w, map[string]any{
		"ID": id, "Client": md.ClientName, "ClientID": md.ClientID, "RedirectHost": ru.Host,
		"Integrations": integs, "Bindings": s.cfg.Bindings, "Problem": problem,
	})
}

func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errorPage(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	id := r.PostForm.Get("request")
	s.mu.Lock()
	p := s.pending[id]
	if p != nil && time.Now().After(p.expires) {
		delete(s.pending, id)
		p = nil
	}
	s.mu.Unlock()
	if p == nil {
		errorPage(w, http.StatusBadRequest, "This authorization request has expired. Start again from the client.")
		return
	}
	if r.PostForm.Get("action") != "approve" {
		s.dropPending(id)
		redirectError(w, r, p.redirect, p.state, s.cfg.Issuer, "access_denied", "the owner denied access")
		return
	}
	if wait := s.lockedOut(); wait > 0 {
		s.renderConsent(w, id, p.client, p.redirect, "Too many wrong passphrases. Try again in "+wait.Round(time.Second).String()+".")
		return
	}
	if !checkPassphrase(s.cfg.OwnerPassphraseHash, r.PostForm.Get("passphrase")) {
		s.mu.Lock()
		s.recordFailureLocked()
		p.failures++
		tooMany := p.failures >= 5
		if tooMany {
			delete(s.pending, id)
		}
		s.mu.Unlock()
		s.log.Warn("consent passphrase rejected", "client", p.client.ClientID)
		if tooMany {
			redirectError(w, r, p.redirect, p.state, s.cfg.Issuer, "access_denied", "too many failed attempts")
			return
		}
		s.renderConsent(w, id, p.client, p.redirect, "The owner passphrase is not correct.")
		return
	}
	byInteg := map[string][]string{}
	for _, v := range r.PostForm["op"] {
		integ, op, ok := strings.Cut(v, "/")
		if ok {
			byInteg[integ] = append(byInteg[integ], op)
		}
	}
	var grants []runtime.Grant
	keys := make([]string, 0, len(byInteg))
	for k := range byInteg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, integ := range keys {
		g := runtime.Grant{Integration: integ, Operations: byInteg[integ], Credential: r.PostForm.Get("binding_" + integ)}
		if err := s.cfg.Catalog.ValidateGrant(g); err != nil {
			s.renderConsent(w, id, p.client, p.redirect, "That grant is not valid: "+err.Error())
			return
		}
		grants = append(grants, g)
	}
	if len(grants) == 0 {
		s.renderConsent(w, id, p.client, p.redirect, "Choose at least one operation.")
		return
	}
	code := randomToken()
	s.mu.Lock()
	delete(s.pending, id)
	g := &Grant{ID: randomToken()[:22], ClientID: p.client.ClientID, ClientName: p.client.ClientName, Grants: grants, CreatedAt: time.Now().UTC()}
	s.state.Grants = append(s.state.Grants, g)
	err := s.saveLocked()
	s.codes[hashToken(code)] = &authCode{grantID: g.ID, clientID: p.client.ClientID, redirect: p.redirect, challenge: p.challenge, expires: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	if err != nil {
		s.log.Error("saving oauth state failed", "error", err)
		redirectError(w, r, p.redirect, p.state, s.cfg.Issuer, "server_error", "the grant could not be recorded")
		return
	}
	s.log.Info("owner granted access", "client", p.client.ClientID, "grant", g.ID)
	u, _ := url.Parse(p.redirect)
	q := u.Query()
	q.Set("code", code)
	if p.state != "" {
		q.Set("state", p.state)
	}
	q.Set("iss", s.cfg.Issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) dropPending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func tokenError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", "the form could not be read")
		return
	}
	f := r.PostForm
	if id := clientIDFrom(r); id != "" && f.Get("client_id") == "" {
		f.Set("client_id", id)
	}
	if f.Get("resource") != "" && f.Get("resource") != s.resource {
		tokenError(w, "invalid_target", "unknown resource")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, f)
	case "refresh_token":
		s.refresh(w, f)
	default:
		tokenError(w, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

// clientIDFrom reads a public client's ID from HTTP Basic authentication
// (RFC 6749 section 2.3.1), which some OAuth libraries send by default.
func clientIDFrom(r *http.Request) string {
	user, _, ok := r.BasicAuth()
	if !ok {
		return ""
	}
	id, err := url.QueryUnescape(user)
	if err != nil {
		return ""
	}
	return id
}

func (s *Server) exchangeCode(w http.ResponseWriter, f url.Values) {
	h := hashToken(f.Get("code"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, spent := s.used[h]; spent {
		// A replayed code revokes what it issued (RFC 9700).
		if g := s.grantLocked(u.grantID); g != nil {
			s.revokeLocked(g)
			_ = s.saveLocked()
		}
		tokenError(w, "invalid_grant", "the code was already used")
		return
	}
	c := s.codes[h]
	if c == nil || time.Now().After(c.expires) {
		tokenError(w, "invalid_grant", "unknown or expired code")
		return
	}
	if f.Get("client_id") != c.clientID {
		// Not this client's code; leave it for its owner.
		tokenError(w, "invalid_grant", "the code was issued to another client")
		return
	}
	delete(s.codes, h)
	s.used[h] = usedCode{grantID: c.grantID, expires: time.Now().Add(10 * time.Minute)}
	for k, u := range s.used {
		if time.Now().After(u.expires) {
			delete(s.used, k)
		}
	}
	if f.Get("redirect_uri") != c.redirect {
		tokenError(w, "invalid_grant", "redirect_uri does not match the authorization")
		return
	}
	if v := f.Get("code_verifier"); len(v) < 43 || s256(v) != c.challenge {
		tokenError(w, "invalid_grant", "the PKCE code_verifier does not match")
		return
	}
	access, refresh, err := s.issueLocked(c.grantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(s.cfg.AccessTTL.Seconds()), "refresh_token": refresh})
}

func (s *Server) refresh(w http.ResponseWriter, f url.Values) {
	h := hashToken(f.Get("refresh_token"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.state.Tokens, func(r *record) bool { return r.SHA256 == h && r.Kind == "spent" }); i >= 0 {
		// A rotated-out refresh token came back: one of its holders is not
		// the client. Revoke the whole grant (RFC 9700 section 4.14.2).
		if g := s.grantLocked(s.state.Tokens[i].GrantID); g != nil {
			s.revokeLocked(g)
			_ = s.saveLocked()
			s.log.Warn("refresh token reuse; grant revoked", "grant", g.ID, "client", g.ClientID)
		}
		tokenError(w, "invalid_grant", "this refresh token was already used; the grant is revoked")
		return
	}
	idx := slices.IndexFunc(s.state.Tokens, func(r *record) bool { return r.SHA256 == h && r.Kind == "refresh" })
	if idx < 0 || time.Now().After(s.state.Tokens[idx].Expires) {
		tokenError(w, "invalid_grant", "unknown or expired refresh token")
		return
	}
	g := s.grantLocked(s.state.Tokens[idx].GrantID)
	if g == nil || g.Revoked || f.Get("client_id") != g.ClientID {
		tokenError(w, "invalid_grant", "the grant is revoked or belongs to another client")
		return
	}
	s.state.Tokens[idx].Kind = "spent" // rotate, and remember it until it expires
	access, refresh, err := s.issueLocked(g.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(s.cfg.AccessTTL.Seconds()), "refresh_token": refresh})
}

// handleRevoke implements RFC 7009: revoking a token revokes its grant.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", "the form could not be read")
		return
	}
	if id := clientIDFrom(r); id != "" && r.PostForm.Get("client_id") == "" {
		r.PostForm.Set("client_id", id)
	}
	h := hashToken(r.PostForm.Get("token"))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.state.Tokens {
		if rec.SHA256 == h {
			if g := s.grantLocked(rec.GrantID); g != nil && g.ClientID == r.PostForm.Get("client_id") {
				s.revokeLocked(g)
				_ = s.saveLocked()
			}
			break
		}
	}
	w.WriteHeader(http.StatusOK) // RFC 7009: unknown tokens are not an error
}

var errorTmpl = template.Must(template.New("e").Parse(`<!doctype html><meta charset=utf-8><title>Daraja</title>
<body style="font-family:system-ui;max-width:40rem;margin:3rem auto;padding:0 1rem"><h1>Authorization failed</h1><p>{{.}}</p></body>`))

var consentTmpl = template.Must(template.New("c").Parse(`<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width">
<title>Authorize {{.Client}} — Daraja</title>
<body style="font-family:system-ui;max-width:44rem;margin:2rem auto;padding:0 1rem;line-height:1.4">
<h1>Authorize {{.Client}}</h1>
<p><code>{{.ClientID}}</code> wants to call Daraja operations as you. It will be sent back to <b>{{.RedirectHost}}</b>.</p>
<p>Choose exactly what it may do. Nothing outside your choice is granted, whatever the client asked for.</p>
{{if .Problem}}<p style="color:#b00020"><b>{{.Problem}}</b></p>{{end}}
<form method=post>
<input type=hidden name=request value="{{.ID}}">
{{range .Integrations}}{{$id := .ID}}
<fieldset style="margin:1rem 0"><legend><b>{{.ID}}</b></legend>
{{if .Description}}<p>{{.Description}}</p>{{end}}
{{range .Ops}}<label style="display:block;margin:.3rem 0"><input type=checkbox name=op value="{{.Value}}"> <code>{{.Name}}</code>
{{if .Destructive}}<b style="color:#b00020">destructive write</b>{{else if .Write}}<b>write</b>{{else}}read{{end}} — {{.Description}}</label>{{end}}
<label>Credential binding: <select name="binding_{{$id}}">{{range $.Bindings}}<option>{{.}}</option>{{end}}</select></label>
</fieldset>{{end}}
<p><label>Owner passphrase: <input type=password name=passphrase autocomplete=current-password required></label></p>
<button name=action value=approve>Approve</button> <button name=action value=deny formnovalidate>Deny</button>
</form></body>`))

// Consent passphrase guessing is limited across all requests, not only per
// pending request, because pending requests are free to create.
const (
	maxPending     = 64
	maxClients     = 256
	failureBudget  = 10               // failures allowed per window before lockout
	failureWindow  = 15 * time.Minute // failures older than this are forgotten
	lockoutMinimum = time.Minute
)

// lockedOut reports how long consent is locked after too many failures.
func (s *Server) lockedOut() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Until(s.lockUntil)
}

func (s *Server) recordFailureLocked() {
	now := time.Now()
	kept := s.failures[:0]
	for _, t := range s.failures {
		if now.Sub(t) < failureWindow {
			kept = append(kept, t)
		}
	}
	s.failures = append(kept, now)
	if over := len(s.failures) - failureBudget; over >= 0 {
		// Doubles with each failure past the budget, capped at the window.
		d := lockoutMinimum << min(over, 4)
		s.lockUntil = now.Add(min(d, failureWindow))
	}
}
