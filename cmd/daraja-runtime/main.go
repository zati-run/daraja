// Command daraja-runtime serves published Daraja integrations over MCP.
//
// It is the deployable runtime and is built to run as the single process of
// a Nanos unikernel image: it starts no subprocess and needs only its
// artifacts, a credentials file outside the image, and a writable journal
// path.
//
// Consumers authenticate with OAuth 2.1 and Client ID Metadata Documents
// (-public-url and -owner-passphrase-file). Static principal tokens are for
// stdio and operator-run tests; on HTTP they need -allow-static-tokens.
// Execution capabilities (for example a headed browser on the owner's
// computer) dial in with tokens from -capabilities.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/mcpserver"
	"github.com/zati-run/daraja/oauth"
	"github.com/zati-run/daraja/runtime"
	"github.com/zati-run/daraja/session"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "daraja-runtime:", err)
		os.Exit(1)
	}
}

func run() error {
	start := time.Now()
	var (
		integrations = flag.String("integrations", envOr("DARAJA_INTEGRATIONS", "/daraja/integrations"), "directory of integration artifacts")
		principals   = flag.String("principals", envOr("DARAJA_PRINCIPALS", "/daraja/principals.json"), "principals file")
		credentials  = flag.String("credentials", envOr("DARAJA_CREDENTIALS", ""), "credentials file (kept outside the image)")
		journalPath  = flag.String("journal", envOr("DARAJA_JOURNAL", "/data/journal.jsonl"), "receipt journal file")
		listen       = flag.String("listen", envOr("DARAJA_LISTEN", ":8080"), "HTTP listen address")
		stdio        = flag.String("stdio-principal", "", "serve MCP on stdio as this principal instead of HTTP")
		timeout      = flag.Duration("request-timeout", 30*time.Second, "timeout for each source request")
		capsPath     = flag.String("capabilities", envOr("DARAJA_CAPABILITIES", ""), "execution capability registrations file")
		capTimeout   = flag.Duration("capability-timeout", 90*time.Second, "timeout for one brokered capability call")
		publicURL    = flag.String("public-url", envOr("DARAJA_PUBLIC_URL", ""), "public base URL; enables OAuth with client ID metadata documents")
		ownerPass    = flag.String("owner-passphrase-file", envOr("DARAJA_OWNER_PASSPHRASE_FILE", ""), "file holding the owner passphrase hash (daraja owner-passphrase)")
		oauthState   = flag.String("oauth-state", envOr("DARAJA_OAUTH_STATE", "/data/oauth.json"), "OAuth grants and token hashes")
		allowStatic  = flag.Bool("allow-static-tokens", false, "also accept static principal tokens on HTTP (operator tests only)")
		syncAll      = flag.Bool("journal-sync-all", false, "call sync(2) after each journal fsync")
		noModeCheck  = flag.Bool("unikernel-file-modes", false, "skip the credentials file-mode check (Nanos has no file owners and one process)")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "daraja-runtime", "version", version)

	ints, err := runtime.LoadIntegrations(*integrations)
	if err != nil {
		return err
	}
	// The broker must outwait the capability's own job bound (60 s by
	// default, above its 45 s in-page fetch), or a slow but answered call
	// is recorded as uncertain.
	if *capTimeout <= *timeout {
		return fmt.Errorf("-capability-timeout (%s) must exceed -request-timeout (%s)", *capTimeout, *timeout)
	}
	log.Info("timeouts", "request", *timeout, "capability", *capTimeout, "capability_job_default", capability.DefaultJobTimeout)
	var ps []runtime.Principal
	if *principals != "" {
		if ps, err = runtime.LoadPrincipals(*principals); err != nil && !(errors.Is(err, os.ErrNotExist) && *publicURL != "") {
			return err
		}
	}
	creds := map[string]session.Binding{}
	if *credentials != "" {
		if creds, err = runtime.LoadCredentialsMode(*credentials, !*noModeCheck); err != nil {
			return err
		}
	}
	j, err := journal.Open(*journalPath)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	defer j.Close()
	j.SyncAll = *syncAll
	var broker *capability.Broker
	bindings := []string{}
	for name := range creds {
		bindings = append(bindings, name)
	}
	if *capsPath != "" {
		var f struct {
			Capabilities []capability.Registration `json:"capabilities"`
		}
		b, err := os.ReadFile(*capsPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("%s: %w", *capsPath, err)
		}
		if broker, err = capability.NewBroker(capability.BrokerConfig{Registrations: f.Capabilities, Logger: log}); err != nil {
			return err
		}
		for _, r := range f.Capabilities {
			bindings = append(bindings, r.Bindings...)
		}
	}
	slices.Sort(bindings)
	bindings = slices.Compact(bindings)
	cfg := runtime.Config{
		Integrations: ints, Principals: ps, Credentials: creds, Journal: j,
		HTTPClient: &http.Client{}, RequestTimeout: *timeout, Logger: log, CapabilityTimeout: *capTimeout,
	}
	if broker != nil {
		cfg.Capabilities = broker
	}
	rt, err := runtime.New(cfg)
	if err != nil {
		return err
	}
	ids := []string{}
	for _, in := range ints {
		ids = append(ids, in.ID+"@"+in.Version+" "+in.Hash)
	}
	log.Info("loaded", "integrations", ids, "principals", len(ps), "recovered_uncertain", len(j.Recovered), "journal_torn_bytes_dropped", j.TornBytes, "startup_ms", time.Since(start).Milliseconds())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *stdio != "" {
		p, ok := rt.Principal(*stdio)
		if !ok {
			return fmt.Errorf("unknown principal %q", *stdio)
		}
		return mcpserver.NewServer(rt, p, version).Run(ctx, &mcp.StdioTransport{})
	}

	mux := http.NewServeMux()
	var opts []mcpserver.Option
	switch {
	case *publicURL != "":
		hash, err := os.ReadFile(*ownerPass)
		if err != nil {
			return fmt.Errorf("owner passphrase: %w", err)
		}
		as, err := oauth.New(oauth.Config{
			Issuer: *publicURL, OwnerPassphraseHash: string(hash), StatePath: *oauthState,
			Catalog: rt, Bindings: bindings, Logger: log,
		})
		if err != nil {
			return err
		}
		as.Routes(mux)
		auth := mcpserver.Authenticator(as.Verify)
		if *allowStatic {
			auth = mcpserver.AnyOf(as.Verify, rt.Authenticate)
		}
		opts = append(opts, mcpserver.WithAuthenticator(auth), mcpserver.WithChallenge(as.Challenge()))
		log.Info("oauth enabled", "issuer", *publicURL, "resource", as.Resource())
	case !*allowStatic:
		return errors.New("no consumer authentication: set -public-url and -owner-passphrase-file for OAuth, or -allow-static-tokens for operator tests")
	}
	mux.Handle("/mcp", mcpserver.NewHandler(rt, version, opts...))
	if broker != nil {
		mux.Handle("/capability/", broker.Handler())
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "serving", "version": version, "contract": mcpserver.Version,
			"integrations": len(ints), "uptime_ms": time.Since(start).Milliseconds(),
		})
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// Bind first, so that "serving" is true when it's logged.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("serving", "addr", *listen, "ready_ms", time.Since(start).Milliseconds())
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
