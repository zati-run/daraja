package runtime_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zati-run/daraja/runtime"
)

// Where file modes mean nothing (a unikernel image), a credentials file may
// hold only synthetic bindings.
func TestUncheckedCredentialsMustBeSynthetic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	real := write("real.json", `{"bindings":{"owner":{"values":{"username":"u","password":"p"}}}}`)
	synth := write("synth.json", `{"bindings":{"notes":{"synthetic":true,"values":{"username":"u","password":"p"}}}}`)
	if _, err := runtime.LoadCredentialsMode(real, false); err == nil || !strings.Contains(err.Error(), "not marked synthetic") {
		t.Fatalf("real binding without custody check: %v", err)
	}
	if _, err := runtime.LoadCredentialsMode(synth, false); err != nil {
		t.Fatalf("synthetic binding: %v", err)
	}
	// With the mode check, a group-readable file is refused either way.
	if _, err := runtime.LoadCredentialsMode(synth, true); err == nil {
		t.Fatal("group-readable credentials accepted with the mode check")
	}
}
