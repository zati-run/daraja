package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

// ClientMetadata is the subset of a Client ID Metadata Document Daraja uses.
type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type cachedClient struct {
	md      *ClientMetadata
	fetched time.Time
}

// PublicOnlyClient fetches client metadata without following redirects and
// refuses to connect to loopback, private, link-local or otherwise
// non-public addresses, so a client_id URL cannot be used to reach the
// runtime's own network.
func PublicOnlyClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return err
		}
		if !publicAddr(ip) {
			return fmt.Errorf("client metadata address %s is not public", ip)
		}
		return nil
	}}
	tr := &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: noRedirects}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// validClientID reports whether id is a non-root https URL without a
// fragment, credentials or dot segments.
func validClientID(id string) bool {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Path == "" || u.Path == "/" {
		return false
	}
	for _, seg := range []string{"/./", "/../"} {
		if strings.Contains(u.Path+"/", seg) {
			return false
		}
	}
	return true
}

// client fetches and validates a client metadata document, caching it for
// five minutes.
func (s *Server) client(ctx context.Context, id string) (*ClientMetadata, error) {
	if !validClientID(id) {
		return nil, errors.New("client_id must be a non-root https URL (a Client ID Metadata Document)")
	}
	s.mu.Lock()
	c, ok := s.clients[id]
	s.mu.Unlock()
	if ok && time.Since(c.fetched) < 5*time.Minute {
		return c.md, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := *s.meta
	client.CheckRedirect = noRedirects
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching client metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.log.Info("client metadata fetch refused", "status", resp.StatusCode)
		return nil, errors.New("client metadata could not be fetched")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 64<<10 {
		return nil, errors.New("client metadata exceeds 64 KiB")
	}
	var md ClientMetadata
	if err := json.Unmarshal(body, &md); err != nil {
		return nil, errors.New("client metadata is not JSON")
	}
	if md.ClientID != id {
		return nil, errors.New("client metadata client_id does not equal its URL")
	}
	if len(md.RedirectURIs) == 0 {
		return nil, errors.New("client metadata lists no redirect_uris")
	}
	if md.TokenEndpointAuthMethod != "" && md.TokenEndpointAuthMethod != "none" {
		return nil, fmt.Errorf("token_endpoint_auth_method %q is not supported (only none)", md.TokenEndpointAuthMethod)
	}
	if len(md.GrantTypes) > 0 && !slices.Contains(md.GrantTypes, "authorization_code") {
		return nil, errors.New("client metadata does not allow authorization_code")
	}
	for _, r := range md.RedirectURIs {
		u, err := url.Parse(r)
		if err != nil || u.Fragment != "" || (u.Scheme != "https" && !isLoopbackURL(r)) {
			return nil, fmt.Errorf("redirect_uri %q must be https or loopback http", r)
		}
	}
	if md.ClientName == "" {
		md.ClientName = id
	}
	s.mu.Lock()
	if len(s.clients) >= maxClients {
		for k, c := range s.clients {
			if time.Since(c.fetched) >= 5*time.Minute {
				delete(s.clients, k)
			}
		}
	}
	if len(s.clients) >= maxClients {
		s.mu.Unlock()
		return &md, nil // valid, but not cached
	}
	s.clients[id] = cachedClient{md: &md, fetched: time.Now()}
	s.mu.Unlock()
	return &md, nil
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.IsLoopback()
}

// nonPublic lists ranges that are not reachable public internet beyond what
// netip classifies: shared address space (CGNAT, used by tailnets),
// benchmarking, IETF protocol assignments, "this network", documentation,
// and NAT64, whose embedded IPv4 address is checked separately.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
}

var nat64 = []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48")}

// publicAddr reports whether ip is a public unicast address.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	for _, p := range nat64 {
		if p.Contains(ip) {
			b := ip.As16()
			return publicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		}
	}
	return true
}
