package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
)

// While a person is on a pending sign-in step, the capability neither
// fetches nor navigates in that window; once the page leaves the step, it
// resumes.
func TestPendingHumanStepBlocksPage(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/x" {
			fetches++
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, err := Open(ctx, Options{ProfileDir: t.TempDir(), Headless: true})
	if err != nil {
		t.Skipf("Chrome is not available: %v", err)
	}
	defer b.Close()
	if err := chromedp.Do(b.Ctx, chromedp.Navigate(srv.URL+"/auth/tsv")); err != nil {
		t.Fatal(err)
	}
	prefix := "/auth/tsv"
	b.human.Store(&prefix)
	in := &integration.Compiled{Integration: &integration.Integration{}}
	in.Source.Origin = srv.URL
	req := capability.FetchRequest{Method: http.MethodGet, URL: srv.URL + "/api/x", Headers: map[string]string{}}
	if _, err := b.Fetch(ctx, in, req); !errors.Is(err, capability.ErrHumanActive) || !errors.Is(err, capability.ErrNotSent) {
		t.Fatalf("fetch during human step: %v", err)
	}
	if err := b.Recover(ctx, in); !errors.Is(err, capability.ErrHumanActive) {
		t.Fatalf("recover during human step: %v", err)
	}
	if b.path() != "/auth/tsv" || fetches != 0 {
		t.Fatalf("page moved to %s or fetched %d times during the human step", b.path(), fetches)
	}
	// The person finishes: the page leaves the step.
	if err := chromedp.Do(b.Ctx, chromedp.Navigate(srv.URL+"/agent")); err != nil {
		t.Fatal(err)
	}
	if res, err := b.Fetch(ctx, in, req); err != nil || res.Status != 200 || fetches != 1 {
		t.Fatalf("fetch after the step: %v %+v fetches=%d", err, res, fetches)
	}
}
