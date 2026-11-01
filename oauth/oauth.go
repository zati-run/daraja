// Package oauth is Daraja's consumer authorization: OAuth 2.1 with the
// authorization code grant and PKCE (S256 only), where clients identify
// themselves with a Client ID Metadata Document (an https URL that serves
// the client's metadata), as the MCP authorization specification describes.
//
// The runtime is the protected resource and, in v1, also embeds this
// minimal authorization server. Consent is a grant: the owner signs in to
// the consent page with an owner passphrase and chooses which integrations,
// operations and credential binding the client may use. A scope the client
// requests never grants more than the owner chose.
//
// Tokens are opaque, stored only as SHA-256, short-lived, audience-bound to
// the runtime's resource URL, and refresh tokens rotate on use. This
// authorizes an agent to Daraja; the source application still sees only
// the user's own session.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/runtime"
)

// Catalog is what the consent page offers and how grants are checked.
// runtime.Runtime implements it.
type Catalog interface {
	Integrations() []*integration.Compiled
	ValidateGrant(runtime.Grant) error
}

// Config configures a Server.
type Config struct {
	// Issuer is the runtime's public base URL, for example
	// https://daraja.example. The MCP endpoint is Issuer + "/mcp".
	Issuer string
	// OwnerPassphraseHash is produced by HashPassphrase.
	OwnerPassphraseHash string
	// StatePath persists grants and token hashes. Empty keeps them in memory.
	StatePath string
	Catalog   Catalog
	// Bindings are the credential binding names the owner may choose.
	Bindings []string
	// MetadataClient fetches client metadata documents. Nil uses a client
	// that refuses non-public addresses and redirects.
	MetadataClient *http.Client
	AccessTTL      time.Duration // zero means 1 hour
	RefreshTTL     time.Duration // zero means 30 days
	Logger         *slog.Logger
}

// Server is the authorization server and token verifier.
type Server struct {
	cfg      Config
	resource string
	log      *slog.Logger
	meta     *http.Client

	mu      sync.Mutex
	state   persisted
	pending map[string]*pendingAuth
	codes   map[string]*authCode // by code hash
	used    map[string]usedCode  // spent code hash -> grant
	clients map[string]cachedClient

	failures  []time.Time // recent consent passphrase failures
	lockUntil time.Time
}

type persisted struct {
	Grants []*Grant  `json:"grants"`
	Tokens []*record `json:"tokens"`
}

// Grant is one owner consent for one client.
type Grant struct {
	ID         string          `json:"id"`
	ClientID   string          `json:"client_id"`
	ClientName string          `json:"client_name"`
	Grants     []runtime.Grant `json:"grants"`
	CreatedAt  time.Time       `json:"created_at"`
	Revoked    bool            `json:"revoked,omitempty"`
}

type record struct {
	SHA256  string    `json:"sha256"`
	Kind    string    `json:"kind"` // access, refresh, or spent (a rotated-out refresh token)
	GrantID string    `json:"grant_id"`
	Expires time.Time `json:"expires"`
}

type pendingAuth struct {
	client    *ClientMetadata
	redirect  string
	challenge string
	state     string
	expires   time.Time
	failures  int
}

type authCode struct {
	grantID   string
	clientID  string
	redirect  string
	challenge string
	expires   time.Time
}

type usedCode struct {
	grantID string
	expires time.Time
}

// New validates the configuration and loads persisted state.
func New(cfg Config) (*Server, error) {
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if cfg.Issuer == "" || (!strings.HasPrefix(cfg.Issuer, "https://") && !isLoopbackURL(cfg.Issuer)) {
		return nil, errors.New("oauth: issuer must be an https URL (or loopback http for tests)")
	}
	if _, _, _, err := parseHash(cfg.OwnerPassphraseHash); err != nil {
		return nil, fmt.Errorf("oauth: owner passphrase hash: %w", err)
	}
	if cfg.Catalog == nil {
		return nil, errors.New("oauth: a catalog is required")
	}
	if cfg.AccessTTL == 0 {
		cfg.AccessTTL = time.Hour
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		cfg: cfg, resource: cfg.Issuer + "/mcp", log: cfg.Logger, meta: cfg.MetadataClient,
		pending: map[string]*pendingAuth{}, codes: map[string]*authCode{}, used: map[string]usedCode{}, clients: map[string]cachedClient{},
	}
	if s.meta == nil {
		s.meta = PublicOnlyClient()
	}
	if cfg.StatePath != "" {
		b, err := os.ReadFile(cfg.StatePath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("oauth: reading state: %w", err)
		default:
			if err := json.Unmarshal(b, &s.state); err != nil {
				return nil, fmt.Errorf("oauth: decoding state: %w", err)
			}
		}
	}
	return s, nil
}

// Resource is the protected resource URL (the MCP endpoint).
func (s *Server) Resource() string { return s.resource }

// ResourceMetadataURL is where the protected resource metadata is served.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.Issuer + "/.well-known/oauth-protected-resource/mcp"
}

// Challenge is the WWW-Authenticate value for an unauthenticated request.
func (s *Server) Challenge() string {
	return `Bearer resource_metadata="` + s.ResourceMetadataURL() + `"`
}

// Verify maps an access token to a principal.
func (s *Server) Verify(token string) (*runtime.Principal, bool) {
	if token == "" {
		return nil, false
	}
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.state.Tokens {
		if r.SHA256 != h || r.Kind != "access" {
			continue
		}
		if time.Now().After(r.Expires) {
			return nil, false
		}
		g := s.grantLocked(r.GrantID)
		if g == nil || g.Revoked {
			return nil, false
		}
		return &runtime.Principal{ID: "oauth:" + g.ID, Grants: g.Grants, ReceiptScope: "oauth-client:" + g.ClientID}, true
	}
	return nil, false
}

// Grants lists consents, newest last.
func (s *Server) Grants() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Grant, 0, len(s.state.Grants))
	for _, g := range s.state.Grants {
		out = append(out, *g)
	}
	return out
}

// Revoke revokes a grant and every token issued under it.
func (s *Server) Revoke(grantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grantLocked(grantID)
	if g == nil {
		return fmt.Errorf("no grant %q", grantID)
	}
	s.revokeLocked(g)
	return s.saveLocked()
}

func (s *Server) revokeLocked(g *Grant) {
	g.Revoked = true
	kept := s.state.Tokens[:0]
	for _, r := range s.state.Tokens {
		if r.GrantID != g.ID {
			kept = append(kept, r)
		}
	}
	s.state.Tokens = kept
}

func (s *Server) grantLocked(id string) *Grant {
	for _, g := range s.state.Grants {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// saveLocked writes state atomically and durably, dropping expired tokens.
func (s *Server) saveLocked() error {
	now := time.Now()
	kept := s.state.Tokens[:0]
	for _, r := range s.state.Tokens {
		if now.Before(r.Expires) {
			kept = append(kept, r)
		}
	}
	s.state.Tokens = kept
	if s.cfg.StatePath == "" {
		return nil
	}
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.cfg.StatePath), ".oauth-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.cfg.StatePath)
}

func (s *Server) issueLocked(grantID string) (access, refresh string, err error) {
	access, refresh = randomToken(), randomToken()
	now := time.Now()
	s.state.Tokens = append(s.state.Tokens,
		&record{SHA256: hashToken(access), Kind: "access", GrantID: grantID, Expires: now.Add(s.cfg.AccessTTL)},
		&record{SHA256: hashToken(refresh), Kind: "refresh", GrantID: grantID, Expires: now.Add(s.cfg.RefreshTTL)},
	)
	return access, refresh, s.saveLocked()
}

func randomToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func s256(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
