package capability_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
)

// page is a test double for a browser page: a signed-in session on a
// synthetic origin that answers every operation request with a fixed body.
type page struct {
	fetches atomic.Int32
	block   chan struct{}
}

func (p *page) Fetch(ctx context.Context, in *integration.Compiled, r capability.FetchRequest) (capability.FetchResult, error) {
	if strings.HasSuffix(r.URL, "/api/me") {
		return capability.FetchResult{Status: 200, Body: []byte(`{"user":{"id":7},"csrf":"c-1"}`)}, nil
	}
	p.fetches.Add(1)
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
		}
	}
	return capability.FetchResult{Status: 200, Body: []byte(`{"note":{"id":1,"title":"Seed note"}}`)}, nil
}

func (p *page) RunSteps(context.Context, *integration.Compiled, []integration.Step) capability.StepsResult {
	return capability.StepsResult{Err: errors.New("not used by these tests")}
}

func (p *page) Recover(context.Context, *integration.Compiled) error { return capability.ErrNeedsHuman }

func artifact(t *testing.T) *integration.Compiled {
	t.Helper()
	b, err := os.ReadFile("../integration/testdata/notes-browser.json")
	if err != nil {
		t.Fatal(err)
	}
	in, err := integration.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func host(t *testing.T, p capability.Page, ops ...string) (*capability.Host, *integration.Compiled) {
	t.Helper()
	in := artifact(t)
	h, err := capability.NewHost(capability.HostConfig{
		ID: "h", Integrations: []*integration.Compiled{in}, MaxQueue: 1,
		Bindings: []capability.HostBinding{{Name: "alice", Page: p, Grants: map[string][]string{in.ID: ops}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, in
}

func job(in *integration.Compiled, op string) capability.Job {
	return capability.Job{
		OpRef:   capability.OpRef{IntegrationID: in.ID, Version: in.Version, ArtifactSHA256: in.Hash, Operation: op},
		Binding: "alice", Input: map[string]any{"id": 1},
	}
}

func TestHostRefusesWhatItDoesNotOwn(t *testing.T) {
	t.Parallel()
	p := &page{}
	h, in := host(t, p, "get_note")
	cases := []struct {
		name string
		mut  func(*capability.Job)
		code string
	}{
		{"unknown hash", func(j *capability.Job) { j.OpRef.ArtifactSHA256 = "sha256:00" }, capability.CodeUnknownOp},
		{"other version", func(j *capability.Job) { j.OpRef.Version = "2" }, capability.CodeUnknownOp},
		{"not granted on host", func(j *capability.Job) { j.OpRef.Operation = "find_note_by_ref"; j.Input = map[string]any{"ref": "x"} }, capability.CodeDenied},
		{"unknown binding", func(j *capability.Job) { j.Binding = "mallory" }, capability.CodeDenied},
		{"invalid input", func(j *capability.Job) { j.Input = map[string]any{"id": 0} }, capability.CodeInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := job(in, "get_note")
			tc.mut(&j)
			out := h.Execute(context.Background(), j)
			if out.Dispatch != capability.NotSent || out.Code != tc.code {
				t.Fatalf("got %+v, want not_sent %s", out, tc.code)
			}
		})
	}
	if n := p.fetches.Load(); n != 0 {
		t.Fatalf("operation fetches = %d, want 0", n)
	}
	if out := h.Execute(context.Background(), job(in, "get_note")); out.Dispatch != capability.Sent || out.Status != 200 {
		t.Fatalf("granted read: %+v", out)
	}
}

func TestHostBusyWhenQueueFull(t *testing.T) {
	t.Parallel()
	p := &page{block: make(chan struct{})}
	h, in := host(t, p, "get_note")
	first := make(chan capability.Outcome)
	go func() { first <- h.Execute(context.Background(), job(in, "get_note")) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.fetches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// One call runs and one may wait (MaxQueue 1); the third is refused.
	second := make(chan capability.Outcome)
	go func() { second <- h.Execute(context.Background(), job(in, "get_note")) }()
	// Until the second call is waiting, a probe would itself wait; bound it
	// and retry until the refusal is immediate.
	var out capability.Outcome
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		out = h.Execute(ctx, job(in, "get_note"))
		cancel()
		if out.Code == capability.CodeBusy && time.Since(start) < 100*time.Millisecond {
			break
		}
	}
	if out.Dispatch != capability.NotSent || out.Code != capability.CodeBusy {
		t.Fatalf("got %+v, want capability_busy", out)
	}
	close(p.block)
	<-first
	<-second
}

func TestBrokerWithoutCapabilityIsNotSent(t *testing.T) {
	t.Parallel()
	b, err := capability.NewBroker(capability.BrokerConfig{Registrations: []capability.Registration{{ID: "h", TokenSHA256: capability.HashToken("t"), Bindings: []string{"alice"}}}})
	if err != nil {
		t.Fatal(err)
	}
	out := b.Execute(context.Background(), capability.Job{Binding: "alice", OpRef: capability.OpRef{IntegrationID: "x"}})
	if out.Dispatch != capability.NotSent || out.Code != capability.CodeUnavailable {
		t.Fatalf("got %+v", out)
	}
}

func TestBrokerRefusesUnauthenticatedPoll(t *testing.T) {
	t.Parallel()
	b, err := capability.NewBroker(capability.BrokerConfig{Registrations: []capability.Registration{{ID: "h", TokenSHA256: capability.HashToken("t"), Bindings: []string{"alice"}}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	for _, tok := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+capability.PollPath, strings.NewReader(`{}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", tok, resp.StatusCode)
		}
	}
}

// illegal answers every job with an outcome that is not a legal pair.
type illegal struct{ desc capability.Description }

func (i illegal) Describe() capability.Description { return i.desc }
func (illegal) Execute(context.Context, capability.Job) capability.Outcome {
	return capability.Outcome{Dispatch: capability.NotSent, Status: 201}
}

func TestBrokerTreatsIllegalOutcomeAsUnknown(t *testing.T) {
	t.Parallel()
	b, err := capability.NewBroker(capability.BrokerConfig{
		Registrations: []capability.Registration{{ID: "h", TokenSHA256: capability.HashToken("t"), Bindings: []string{"alice"}}},
		PollWait:      200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	ref := capability.OpRef{IntegrationID: "x", ArtifactSHA256: "sha256:1"}
	desc := capability.Description{Bindings: []capability.BindingDesc{{Binding: "alice", Integrations: []capability.IntegrationDesc{{ID: "x", SHA256: "sha256:1"}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &capability.Agent{URL: srv.URL, Token: "t", Exec: illegal{desc}}
	go func() { _ = a.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !b.Live("alice", ref) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out := b.Execute(context.Background(), capability.Job{Binding: "alice", OpRef: ref})
	if out.Dispatch != capability.Unknown || out.Code != capability.CodeProtocol {
		t.Fatalf("got %+v, want unknown protocol_error", out)
	}
}

func TestCheckRuntimeURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://daraja.example", false, true},
		{"http://127.0.0.1:8080", false, true},
		{"http://[::1]:8080", false, true},
		{"http://localhost:8080", false, true},
		{"http://192.168.1.20:18080", false, false},
		{"http://daraja.example", false, false},
		{"http://192.168.1.20:18080", true, true},
		{"ftp://daraja.example", true, false},
		{"daraja.example", false, false},
	}
	for _, tc := range cases {
		if err := capability.CheckRuntimeURL(tc.url, tc.insecure); (err == nil) != tc.ok {
			t.Errorf("CheckRuntimeURL(%q, %v) = %v, want ok=%v", tc.url, tc.insecure, err, tc.ok)
		}
	}
	a := &capability.Agent{URL: "http://192.168.1.20:18080", Token: "t", Exec: illegal{}}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("agent ran against a plain http runtime off loopback")
	}
}

func TestPinnedClient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	good, err := capability.PinnedClient(base64.StdEncoding.EncodeToString(sum[:]), roots)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatalf("pinned runtime refused: %v", err)
	}
	resp.Body.Close()
	other := sha256.Sum256([]byte("another key"))
	bad, err := capability.PinnedClient(base64.StdEncoding.EncodeToString(other[:]), roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Get(srv.URL); err == nil {
		t.Fatal("a runtime with another key was accepted")
	}
	if _, err := capability.PinnedClient("not-a-pin", nil); err == nil {
		t.Fatal("a malformed pin was accepted")
	}
}

// big answers every job with a source response of n bytes.
type big struct {
	desc capability.Description
	n    int
}

func (b big) Describe() capability.Description { return b.desc }
func (b big) Execute(context.Context, capability.Job) capability.Outcome {
	return capability.Outcome{Dispatch: capability.Sent, Status: 200, Body: bytes.Repeat([]byte("x"), b.n)}
}

// An outcome carrying the largest allowed source response is delivered,
// not dropped by the result size limit.
func TestBrokerAcceptsLargestOutcome(t *testing.T) {
	t.Parallel()
	b, err := capability.NewBroker(capability.BrokerConfig{
		Registrations: []capability.Registration{{ID: "h", TokenSHA256: capability.HashToken("t"), Bindings: []string{"alice"}}},
		PollWait:      200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	ref := capability.OpRef{IntegrationID: "x", ArtifactSHA256: "sha256:1"}
	desc := capability.Description{Bindings: []capability.BindingDesc{{Binding: "alice", Integrations: []capability.IntegrationDesc{{ID: "x", SHA256: "sha256:1"}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &capability.Agent{URL: srv.URL, Token: "t", Exec: big{desc, capability.MaxBody}}
	go func() { _ = a.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !b.Live("alice", ref) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ectx, ecancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer ecancel()
	out := b.Execute(ectx, capability.Job{Binding: "alice", OpRef: ref})
	if out.Dispatch != capability.Sent || len(out.Body) != capability.MaxBody {
		t.Fatalf("got dispatch=%s code=%s body=%d, want sent with %d bytes", out.Dispatch, out.Code, len(out.Body), capability.MaxBody)
	}
}

// formPage answers the form page itself; everything else is the session check.
type formPage struct {
	page
	headers map[string]string
	err     error
}

func (p *formPage) Fetch(ctx context.Context, in *integration.Compiled, r capability.FetchRequest) (capability.FetchResult, error) {
	if !strings.HasSuffix(r.URL, "/notes/new") {
		return p.page.Fetch(ctx, in, r)
	}
	p.headers = r.Headers
	return capability.FetchResult{}, p.err
}

func formHost(t *testing.T, p capability.Page) (*capability.Host, *integration.Compiled) {
	t.Helper()
	raw, err := os.ReadFile("../integration/testdata/notes-form.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	session := m["session"].(map[string]any)
	session["extract"] = map[string]any{"csrf": map[string]any{"request": map[string]any{"method": "GET", "path": "/api/me"}, "json_pointer": "/csrf"}}
	session["request_headers"] = map[string]string{"X-CSRF-Token": "{{session.csrf}}"}
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	in, err := integration.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	h, err := capability.NewHost(capability.HostConfig{
		ID: "h", Integrations: []*integration.Compiled{in}, MaxQueue: 1,
		Bindings: []capability.HostBinding{{Name: "alice", Page: p, Grants: map[string][]string{in.ID: {"create_note"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, in
}

func TestHostFormPage(t *testing.T) {
	t.Parallel()
	p := &formPage{err: capability.ErrBodyTooLarge}
	h, in := formHost(t, p)
	out := h.Execute(context.Background(), capability.Job{
		OpRef:   capability.OpRef{IntegrationID: in.ID, Version: in.Version, ArtifactSHA256: in.Hash, Operation: "create_note"},
		Binding: "alice", Input: map[string]any{"title": "t"}, Marker: "m",
	})
	if out.Dispatch != capability.NotSent || out.Code != capability.CodeSourceChanged {
		t.Errorf("a form page over the limit: got %+v, want not sent with %s", out, capability.CodeSourceChanged)
	}
	if p.headers["X-CSRF-Token"] != "c-1" {
		t.Errorf("the form page fetch has headers %v, want the session's", p.headers)
	}
}
