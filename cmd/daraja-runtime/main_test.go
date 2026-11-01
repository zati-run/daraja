package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zati-run/daraja/internal/notesapp"
	"github.com/zati-run/daraja/mcpcheck"
	"github.com/zati-run/daraja/runtime"
)

// TestBinaryKilledDuringWrite runs the real runtime binary, kills it with
// SIGKILL while a write is in flight, restarts it on the same journal, and
// checks the write is reported uncertain, never re-sent, and reconciled.
func TestBinaryKilledDuringWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "daraja-runtime")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	app := notesapp.New("synthetic-user", "synthetic-pass")
	app.SlowTitle, app.SlowFor = "Slow synthetic note", 3*time.Second
	srv := httptest.NewServer(app)
	defer srv.Close()

	ints := filepath.Join(dir, "integrations")
	os.Mkdir(ints, 0o755)
	fixture, _ := os.ReadFile("../../integration/testdata/notes.json")
	os.WriteFile(filepath.Join(ints, "notes.json"), []byte(strings.Replace(string(fixture), "http://127.0.0.1:1", srv.URL, 1)), 0o644)
	token := "synthetic-binary-token"
	principals := fmt.Sprintf(`{"principals":[{"id":"alice","token_sha256":%q,"grants":[{"integration":"synthetic-notes","operations":["get_note","find_note_by_ref","create_note"],"credential":"c"}]}]}`, runtime.HashToken(token))
	os.WriteFile(filepath.Join(dir, "principals.json"), []byte(principals), 0o644)
	os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"bindings":{"c":{"values":{"username":"synthetic-user","password":"synthetic-pass"}}}}`), 0o600)

	start := func() (*exec.Cmd, string) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := l.Addr().String()
		l.Close()
		cmd := exec.Command(bin, "-integrations", ints, "-principals", filepath.Join(dir, "principals.json"),
			"-credentials", filepath.Join(dir, "credentials.json"), "-journal", filepath.Join(dir, "journal.jsonl"), "-listen", addr, "-allow-static-tokens")
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(stderr)
			once := false
			for sc.Scan() {
				if !once && strings.Contains(sc.Text(), `"msg":"serving"`) {
					once = true
					close(ready)
				}
			}
		}()
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatal("runtime did not start")
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd, "http://" + addr + "/mcp"
	}

	ctx := context.Background()
	cmd, url := start()
	c := &mcpcheck.Client{URL: url, Token: token}
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	go c.CallTool(ctx, "synthetic-notes__create_note", map[string]any{"title": "Slow synthetic note", "idempotency_key": "slow-1"})
	deadline := time.Now().Add(5 * time.Second)
	for app.Creates() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if app.Creates() != 1 {
		t.Fatal("write did not reach the app")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	_, url = start()
	c = &mcpcheck.Client{URL: url, Token: token}
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := c.CallTool(ctx, "synthetic-notes__create_note", map[string]any{"title": "Slow synthetic note", "idempotency_key": "slow-1"})
	if err != nil || !r.IsError || !strings.Contains(r.Text(), "outcome_uncertain") {
		t.Fatalf("retry after kill: %v %s", err, r.Text())
	}
	r, err = c.CallTool(ctx, "daraja__reconcile", map[string]any{"tool": "synthetic-notes__create_note", "idempotency_key": "slow-1"})
	if err != nil || r.IsError {
		t.Fatalf("reconcile: %v %s", err, r.Text())
	}
	var rec map[string]any
	json.Unmarshal(r.StructuredContent, &rec)
	if rec["state"] != "succeeded" || rec["identifier"] != "2" {
		t.Fatalf("reconciled receipt = %v", rec)
	}
	if app.Creates() != 1 {
		t.Fatalf("creates = %d, want 1", app.Creates())
	}
}
