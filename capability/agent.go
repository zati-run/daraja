package capability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Executor executes jobs on the capability side.
type Executor interface {
	Describe() Description
	Execute(ctx context.Context, job Job) Outcome
}

// Timeout ordering across processes: in-page fetch < capability job <
// runtime broker (-capability-timeout) < the MCP client's patience.
const (
	InPageFetchTimeout = 45 * time.Second
	DefaultJobTimeout  = 60 * time.Second
)

// Agent connects a capability to a runtime. It dials out, long-polls for
// jobs, executes each one and posts its outcome.
type Agent struct {
	// URL is the runtime's base URL, for example https://daraja.example.
	URL    string
	Token  string
	Exec   Executor
	Client *http.Client
	Logger *slog.Logger
	// JobTimeout bounds one job's execution. Zero means 60 seconds.
	JobTimeout time.Duration
	// AllowInsecure permits a plain http runtime URL that is not loopback.
	// A runtime URL is where the capability takes its orders from, so this
	// is for an isolated lab network only.
	AllowInsecure bool
}

// CheckRuntimeURL refuses a runtime URL that does not authenticate the
// runtime: anything but https, except loopback http, unless allowInsecure.
func CheckRuntimeURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("runtime URL %q is not an absolute URL", raw)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme != "http":
		return fmt.Errorf("runtime URL scheme %q is not supported", u.Scheme)
	case allowInsecure:
		return nil
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && ip.IsLoopback() {
		return nil
	}
	if u.Hostname() == "localhost" {
		return nil
	}
	return fmt.Errorf("runtime URL %s is plain http: anyone on the path could send this capability jobs; use https", raw)
}

// PinnedClient returns an HTTP client that accepts the runtime only when
// a certificate in its verified chain has the given SubjectPublicKeyInfo
// SHA-256 (base64, as in HPKP), on top of normal certificate verification
// against roots (nil means the system roots).
func PinnedClient(spkiSHA256 string, roots *x509.CertPool) (*http.Client, error) {
	want, err := base64.StdEncoding.DecodeString(spkiSHA256)
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("the runtime pin must be a base64 SHA-256 of a SubjectPublicKeyInfo")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, VerifyConnection: func(cs tls.ConnectionState) error {
		for _, chain := range cs.VerifiedChains {
			for _, c := range chain {
				got := sha256.Sum256(c.RawSubjectPublicKeyInfo)
				if subtle.ConstantTimeCompare(got[:], want) == 1 {
					return nil
				}
			}
		}
		return errors.New("the runtime's certificate does not match the pin")
	}}
	return &http.Client{Transport: tr}, nil
}

// Run polls until ctx ends. Connection failures are retried with backoff.
func (a *Agent) Run(ctx context.Context) error {
	if err := CheckRuntimeURL(a.URL, a.AllowInsecure); err != nil {
		return err
	}
	if a.Client == nil {
		a.Client = &http.Client{}
	}
	if a.Logger == nil {
		a.Logger = slog.New(slog.DiscardHandler)
	}
	if a.JobTimeout == 0 {
		a.JobTimeout = DefaultJobTimeout
	}
	a.Logger.Info("timeouts", "job", a.JobTimeout, "in_page_fetch", InPageFetchTimeout)
	base := strings.TrimRight(a.URL, "/")
	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := time.Second
	for ctx.Err() == nil {
		job, err := a.poll(ctx, base)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			a.Logger.Warn("capability poll failed", "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if job == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			jctx, cancel := context.WithTimeout(ctx, a.JobTimeout)
			out := a.Exec.Execute(jctx, *job)
			cancel()
			out.JobID = job.ID
			a.post(ctx, base, out)
		}()
	}
	return ctx.Err()
}

func (a *Agent) poll(ctx context.Context, base string) (*Job, error) {
	body, err := json.Marshal(a.Exec.Describe())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+PollPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var j Job
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&j); err != nil {
			return nil, fmt.Errorf("decoding job: %w", err)
		}
		return &j, nil
	default:
		return nil, fmt.Errorf("poll answered %d", resp.StatusCode)
	}
}

// post delivers an outcome, retrying briefly. If it never arrives the
// runtime records the job as uncertain, which is the safe reading.
func (a *Agent) post(ctx context.Context, base string, out Outcome) {
	body, err := json.Marshal(out)
	if err != nil {
		a.Logger.Error("capability outcome encoding failed", "error", err)
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ResultPath, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Client.Do(req)
		if err == nil {
			resp.Body.Close()
			switch {
			case resp.StatusCode < 400, resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
				return // delivered, or the runtime already resolved the job
			case resp.StatusCode < 500:
				// Retrying the same body cannot help.
				a.Logger.Error("capability outcome refused; the runtime will treat it as uncertain", "job", out.JobID, "status", resp.StatusCode)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	a.Logger.Warn("capability outcome not delivered; the runtime will treat it as uncertain", "job", out.JobID)
}
