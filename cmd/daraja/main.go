// Command daraja is the host-side builder: it validates artifacts, signs in
// with a host browser, explores a source, proposes candidates, and serves
// browser session leases. It is not part of the deployable runtime.
//
//	daraja validate FILE...
//	daraja token
//	daraja signin  -artifact A -credentials C -binding B -profile DIR
//	daraja explore -task T -credentials C -binding B -profile DIR -out OBS
//	daraja plan    -task T -observations OBS -out CANDIDATE [-report R]
//	daraja lease   -artifact A -credentials C -binding B -profile DIR -listen ADDR -token-file F
//	daraja record  -task T -credentials C -binding B -profile DIR -out OBS
//	daraja capability -runtime URL -token-file F -integrations DIR -bindings FILE [-credentials C]
//	daraja owner-passphrase -out FILE
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zati-run/daraja/builder/browser"
	"github.com/zati-run/daraja/builder/explore"
	"github.com/zati-run/daraja/builder/lease"
	"github.com/zati-run/daraja/builder/plan"
	"github.com/zati-run/daraja/capability"
	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/oauth"
	"github.com/zati-run/daraja/runtime"
	"github.com/zati-run/daraja/session"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch os.Args[1] {
	case "validate":
		err = validate(os.Args[2:])
	case "token":
		err = token()
	case "signin":
		err = signin(ctx, log, os.Args[2:])
	case "explore":
		err = exploreCmd(ctx, log, os.Args[2:])
	case "plan":
		err = planCmd(ctx, os.Args[2:])
	case "lease":
		err = leaseCmd(ctx, log, os.Args[2:])
	case "record":
		err = recordCmd(ctx, log, os.Args[2:])
	case "capability":
		err = capabilityCmd(ctx, log, os.Args[2:])
	case "owner-passphrase":
		err = ownerPassphrase(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "daraja:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: daraja validate|token|signin|explore|record|plan|lease|capability|owner-passphrase [flags]")
	os.Exit(2)
}

func validate(files []string) error {
	if len(files) == 0 {
		return errors.New("validate needs artifact files")
	}
	failed := false
	for _, f := range files {
		c, err := integration.LoadFile(f)
		if err != nil {
			failed = true
			var ve *integration.ValidationError
			if errors.As(err, &ve) {
				fmt.Printf("INVALID %s\n", f)
				for _, p := range ve.Problems {
					fmt.Printf("  - %s\n", p)
				}
				continue
			}
			fmt.Printf("INVALID %s: %v\n", f, err)
			continue
		}
		reads, writes := 0, 0
		for _, op := range c.Operations {
			if op.Effect == integration.EffectRead {
				reads++
			} else {
				writes++
			}
		}
		fmt.Printf("VALID %s id=%s version=%s %s reads=%d writes=%d\n", f, c.ID, c.Version, c.Hash, reads, writes)
	}
	if failed {
		return errors.New("one or more artifacts are invalid")
	}
	return nil
}

func token() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	t := hex.EncodeToString(b)
	fmt.Printf("token=%s\nsha256=%s\n", t, runtime.HashToken(t))
	return nil
}

type browserFlags struct {
	artifact, credentials, binding, profile string
	headless                                bool
	humanTimeout                            time.Duration
}

func (bf *browserFlags) register(fs *flag.FlagSet, needArtifact bool) {
	if needArtifact {
		fs.StringVar(&bf.artifact, "artifact", "", "integration artifact (for its session recipe)")
	}
	fs.StringVar(&bf.credentials, "credentials", "", "credentials file")
	fs.StringVar(&bf.binding, "binding", "", "credential binding name")
	fs.StringVar(&bf.profile, "profile", "", "browser profile directory (keep in private custody)")
	fs.BoolVar(&bf.headless, "headless", false, "run Chrome headless (some sources refuse headless browsers)")
	fs.DurationVar(&bf.humanTimeout, "human-timeout", 10*time.Minute, "how long a human sign-in step may take")
}

func (bf *browserFlags) creds() (map[string]string, error) {
	if bf.credentials == "" || bf.binding == "" {
		return nil, errors.New("-credentials and -binding are required")
	}
	all, err := runtime.LoadCredentials(bf.credentials)
	if err != nil {
		return nil, err
	}
	b, ok := all[bf.binding]
	if !ok {
		return nil, fmt.Errorf("binding %q not found", bf.binding)
	}
	return b.Values, nil
}

func (bf *browserFlags) open(ctx context.Context, log *slog.Logger) (*browser.Browser, error) {
	if bf.profile == "" {
		return nil, errors.New("-profile is required")
	}
	return browser.Open(ctx, browser.Options{
		ProfileDir: bf.profile, Headless: bf.headless, Logger: log, HumanTimeout: bf.humanTimeout,
		Notify: func(msg string) {
			fmt.Fprintf(os.Stderr, "\n>>> ACTION NEEDED in the Chrome window: %s\n\n", msg)
		},
	})
}

func signin(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("signin", flag.ExitOnError)
	var bf browserFlags
	bf.register(fs, true)
	fs.Parse(args)
	in, err := integration.LoadFile(bf.artifact)
	if err != nil {
		return err
	}
	creds, err := bf.creds()
	if err != nil {
		return err
	}
	b, err := bf.open(ctx, log)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.EnsureSignedIn(ctx, in, creds); err != nil {
		return err
	}
	fmt.Println("signed in; the profile keeps the session")
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// sessionArtifact wraps a task's session block in a minimal artifact so its
// sign-in recipe is validated before use.
func sessionArtifact(t *explore.Task) (*integration.Compiled, error) {
	if t.Session.SignedIn == nil {
		return nil, errors.New("task session needs a signed_in check")
	}
	in := integration.Integration{
		Format: integration.Format, ID: t.IntegrationID, Version: "explore",
		Source: integration.Source{Kind: "web", Origin: t.Origin}, Session: t.Session,
		Operations: []integration.Operation{{
			Name: "signed_in_check", Description: "Signed-in check.", Effect: integration.EffectRead,
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
			Request:  integration.RequestSpec{Method: "GET", Path: t.Session.SignedIn.Request.Path},
			Response: integration.ResponseSpec{SuccessStatus: []int{200}, Output: map[string]string{}},
		}},
	}
	b, _ := json.Marshal(in)
	return integration.Parse(b)
}

func exploreCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("explore", flag.ExitOnError)
	var bf browserFlags
	bf.register(fs, false)
	taskPath := fs.String("task", "", "task file")
	out := fs.String("out", "", "observations output file")
	fs.Parse(args)
	var t explore.Task
	if err := readJSON(*taskPath, &t); err != nil {
		return err
	}
	si, err := sessionArtifact(&t)
	if err != nil {
		return err
	}
	creds, err := bf.creds()
	if err != nil {
		return err
	}
	b, err := bf.open(ctx, log)
	if err != nil {
		return err
	}
	defer b.Close()
	rep, err := explore.Explore(ctx, b, &t, si, creds)
	if rep != nil {
		if werr := writeJSON(*out, rep); werr != nil {
			return werr
		}
		fmt.Printf("observations=%d requests=%d json=%d blocked_writes=%d -> %s\n", len(rep.Observations), rep.Coverage.Requests, rep.Coverage.JSONResponses, rep.Coverage.BlockedWrites, *out)
	}
	return err
}

func planCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	taskPath := fs.String("task", "", "task file")
	obsPath := fs.String("observations", "", "observations file")
	out := fs.String("out", "", "candidate artifact output")
	reportPath := fs.String("report", "", "plan report output (optional)")
	fs.Parse(args)
	var t explore.Task
	if err := readJSON(*taskPath, &t); err != nil {
		return err
	}
	var rep explore.Report
	if err := readJSON(*obsPath, &rep); err != nil {
		return err
	}
	cand, pr, err := plan.Deterministic{}.Plan(ctx, &t, &rep)
	if err != nil {
		return err
	}
	if err := writeJSON(*out, cand); err != nil {
		return err
	}
	if *reportPath != "" {
		if err := writeJSON(*reportPath, pr); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(cand)
	if _, err := integration.Parse(b); err != nil {
		fmt.Printf("candidate written to %s but does not validate yet: %v\n", *out, err)
		return nil
	}
	for _, c := range pr.Candidates {
		fmt.Printf("%-6s %-40s %s\n", c.Effect, c.Operation, c.Route)
	}
	fmt.Printf("candidate -> %s (review before publishing)\n", *out)
	return nil
}

func leaseCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("lease", flag.ExitOnError)
	var bf browserFlags
	bf.register(fs, true)
	listen := fs.String("listen", "127.0.0.1:7766", "listen address")
	tokenFile := fs.String("token-file", "", "file holding the broker bearer token")
	ttl := fs.Duration("ttl", 10*time.Minute, "lease lifetime")
	fs.Parse(args)
	in, err := integration.LoadFile(bf.artifact)
	if err != nil {
		return err
	}
	if in.Session.Provider != integration.ProviderBrowserLease {
		return fmt.Errorf("integration %s does not use the browser_lease provider", in.ID)
	}
	tb, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("-token-file: %w", err)
	}
	creds, err := bf.creds()
	if err != nil {
		return err
	}
	b, err := bf.open(ctx, log)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.EnsureSignedIn(ctx, in, creds); err != nil {
		return err
	}
	br := lease.New(runtime.HashToken(strings.TrimSpace(string(tb))), *ttl, log)
	br.Add(&lease.Source{Integration: in, Browser: b, Credentials: creds})
	srv := &http.Server{Addr: *listen, Handler: br, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("lease broker serving", "addr", *listen, "integration", in.ID)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// recordCmd records a person's demonstration in a visible browser. Each
// demonstrate step in the task's walk waits for the person to press Enter.
func recordCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	var bf browserFlags
	bf.register(fs, false)
	taskPath := fs.String("task", "", "task file with demonstrate steps")
	out := fs.String("out", "", "observations output file")
	fs.Parse(args)
	if bf.headless {
		return errors.New("a demonstration needs a visible browser")
	}
	var t explore.Task
	if err := readJSON(*taskPath, &t); err != nil {
		return err
	}
	si, err := sessionArtifact(&t)
	if err != nil {
		return err
	}
	var creds map[string]string
	if bf.credentials != "" {
		if creds, err = bf.creds(); err != nil {
			return err
		}
	}
	b, err := bf.open(ctx, log)
	if err != nil {
		return err
	}
	defer b.Close()
	lines := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- struct{}{}
		}
		close(lines)
	}()
	demo := func(ctx context.Context, text string) error {
		fmt.Fprintf(os.Stderr, "\n>>> DEMONSTRATE in the Chrome window: %s\n>>> Press Enter here when you are done.\n", text)
		select {
		case _, ok := <-lines:
			if !ok {
				return errors.New("stdin closed before the demonstration finished")
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	rep, err := explore.ExploreWith(ctx, b, &t, si, creds, explore.Options{Demonstrate: demo})
	if rep != nil {
		if werr := writeJSON(*out, rep); werr != nil {
			return werr
		}
		fmt.Printf("observations=%d requests=%d json=%d blocked_writes=%d -> %s\n", len(rep.Observations), rep.Coverage.Requests, rep.Coverage.JSONResponses, rep.Coverage.BlockedWrites, *out)
	}
	return err
}

// capabilityBindings is the capability host's configuration: one dedicated
// browser profile per credential binding, and the operations it may run.
type capabilityBindings struct {
	Bindings []struct {
		Name    string              `json:"name"`
		Profile string              `json:"profile"`
		Grants  map[string][]string `json:"grants"`
	} `json:"bindings"`
}

// capabilityCmd runs the headed browser execution capability. It dials out
// to the runtime, so the host needs no inbound port.
func capabilityCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("capability", flag.ExitOnError)
	rtURL := fs.String("runtime", "", "runtime base URL")
	tokenFile := fs.String("token-file", "", "file holding this capability's token")
	ints := fs.String("integrations", "", "directory of published integration artifacts (this host's own copy)")
	bindingsPath := fs.String("bindings", "", "bindings file")
	credsPath := fs.String("credentials", "", "credentials file for sign-in recipes (optional, mode 0600)")
	headless := fs.Bool("headless", false, "run Chrome headless (some sources refuse headless browsers)")
	id := fs.String("id", "host", "capability ID announced to the runtime")
	pin := fs.String("runtime-pin", "", "base64 SHA-256 of the runtime's certificate public key (SPKI); when set, only that runtime is trusted")
	insecure := fs.Bool("allow-insecure-runtime", false, "allow a plain http runtime URL off loopback (isolated lab networks only)")
	fs.Parse(args)
	if err := capability.CheckRuntimeURL(*rtURL, *insecure); err != nil {
		return err
	}
	client := &http.Client{}
	if *pin != "" {
		pc, err := capability.PinnedClient(*pin, nil)
		if err != nil {
			return err
		}
		client = pc
	} else if strings.HasPrefix(*rtURL, "https://") {
		log.Warn("the runtime is trusted by its certificate alone; set -runtime-pin to trust only this runtime")
	}
	if *insecure {
		log.Warn("plain http runtime allowed: anyone on the network path can send this capability jobs")
	}
	tb, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("-token-file: %w", err)
	}
	all, err := runtime.LoadIntegrations(*ints)
	if err != nil {
		return err
	}
	var browserInts []*integration.Compiled
	for _, in := range all {
		if in.Session.Provider == integration.ProviderBrowser {
			browserInts = append(browserInts, in)
		}
	}
	var cb capabilityBindings
	if err := readJSON(*bindingsPath, &cb); err != nil {
		return err
	}
	creds := map[string]session.Binding{}
	if *credsPath != "" {
		if creds, err = runtime.LoadCredentials(*credsPath); err != nil {
			return err
		}
	}
	var hbs []capability.HostBinding
	for _, b := range cb.Bindings {
		br, err := browser.Open(ctx, browser.Options{
			ProfileDir: b.Profile, Headless: *headless, Logger: log, Credentials: creds[b.Name].Values,
			Notify: func(msg string) {
				fmt.Fprintf(os.Stderr, "\n>>> ACTION NEEDED for binding %s in its Chrome window: %s\n\n", b.Name, msg)
			},
		})
		if err != nil {
			return fmt.Errorf("binding %s: %w", b.Name, err)
		}
		defer br.Close()
		hbs = append(hbs, capability.HostBinding{Name: b.Name, Page: br, Grants: b.Grants})
	}
	host, err := capability.NewHost(capability.HostConfig{ID: *id, Version: "dev", Integrations: browserInts, Bindings: hbs, Logger: log})
	if err != nil {
		return err
	}
	for _, bd := range host.Describe().Bindings {
		for _, in := range bd.Integrations {
			log.Info("serving", "binding", bd.Binding, "integration", in.ID, "artifact", in.SHA256)
		}
	}
	agent := &capability.Agent{URL: *rtURL, Token: strings.TrimSpace(string(tb)), Exec: host, Logger: log, Client: client, AllowInsecure: *insecure}
	log.Info("capability dialing runtime", "runtime", *rtURL)
	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ownerPassphrase reads the owner passphrase from the terminal (or stdin)
// and writes its PBKDF2 hash. The passphrase itself is never written.
func ownerPassphrase(args []string) error {
	fs := flag.NewFlagSet("owner-passphrase", flag.ExitOnError)
	out := fs.String("out", "", "hash output file")
	fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required (the file to write the passphrase hash to)")
	}
	fmt.Fprint(os.Stderr, "Owner passphrase (at least 12 characters): ")
	// Turn off terminal echo when stdin is a terminal; ignore failures
	// (for example when the passphrase is piped in).
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		if off.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = os.Stdin
				_ = on.Run()
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	h, err := oauth.HashPassphrase(strings.TrimRight(line, "\r\n"), oauth.DefaultIterations)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, []byte(h+"\n"), 0o600)
}
