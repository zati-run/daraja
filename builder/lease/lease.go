// Package lease is the host-side broker for the browser_lease session
// provider. It keeps a signed-in browser per integration, in that
// integration's own profile, and leases the session (cookies and user
// agent) to an authenticated runtime for a bounded time.
//
// The broker owns the browser session. A runtime that holds a lease can act
// as that user until the lease or the source session expires, so the broker
// token and the network path between broker and runtime must be protected.
package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zati-run/daraja/builder/browser"
	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/runtime"
)

// Source is one integration the broker serves.
type Source struct {
	Integration *integration.Compiled
	Browser     *browser.Browser
	Credentials map[string]string
}

// Broker serves leases.
type Broker struct {
	TokenSHA256 string
	TTL         time.Duration
	Log         *slog.Logger

	mu      sync.Mutex
	sources map[string]*Source
}

// New returns a broker.
func New(tokenSHA256 string, ttl time.Duration, log *slog.Logger) *Broker {
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Broker{TokenSHA256: tokenSHA256, TTL: ttl, Log: log, sources: map[string]*Source{}}
}

// Add registers a signed-in source.
func (b *Broker) Add(s *Source) {
	b.mu.Lock()
	b.sources[s.Integration.ID] = s
	b.mu.Unlock()
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || runtime.HashToken(token) != b.TokenSHA256 {
		http.Error(w, "a valid bearer token is required", http.StatusUnauthorized)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/leases/")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	b.mu.Lock()
	src := b.sources[id]
	b.mu.Unlock()
	if src == nil {
		http.Error(w, "unknown integration", http.StatusNotFound)
		return
	}
	// Serialize use of the one browser tab.
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	ok, err := src.Browser.SignedIn(ctx, src.Integration)
	if err != nil || !ok {
		b.Log.Warn("lease source not signed in; signing in again", "integration", id, "error", err)
		if err := src.Browser.EnsureSignedIn(ctx, src.Integration, src.Credentials); err != nil {
			b.Log.Error("lease sign-in failed", "integration", id, "error", err)
			http.Error(w, "the browser session is not signed in: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	l, err := src.Browser.Lease(ctx, src.Integration.Source.Origin, b.TTL)
	if err != nil {
		http.Error(w, "lease failed", http.StatusInternalServerError)
		return
	}
	b.Log.Info("lease issued", "integration", id, "cookies", len(l.Cookies), "expires_at", l.ExpiresAt, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(l); err != nil {
		b.Log.Error("lease write failed", "error", fmt.Sprint(err))
	}
}
