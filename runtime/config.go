package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/session"
)

func decodeStrict(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// LoadPrincipals reads {"principals": [...]}.
func LoadPrincipals(path string) ([]Principal, error) {
	var f struct {
		Principals []Principal `json:"principals"`
	}
	if err := decodeStrict(path, &f); err != nil {
		return nil, err
	}
	return f.Principals, nil
}

// LoadCredentials reads {"bindings": {"name": {...}}}. The file must not be
// readable by other users.
func LoadCredentials(path string) (map[string]session.Binding, error) {
	return LoadCredentialsMode(path, true)
}

// LoadCredentialsMode is LoadCredentials with the file-mode check optional.
// Skip it only where file modes carry no meaning, such as a Nanos unikernel,
// whose filesystem has no owners and runs a single process.
func LoadCredentialsMode(path string, checkMode bool) (map[string]session.Binding, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if checkMode && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: credentials file must not be readable by group or others (mode %v)", path, st.Mode().Perm())
	}
	var f struct {
		Bindings map[string]session.Binding `json:"bindings"`
	}
	if err := decodeStrict(path, &f); err != nil {
		return nil, err
	}
	if !checkMode {
		// Without file modes the custody of this file cannot be checked
		// (for example, it was built into a unikernel image), so it may hold
		// only synthetic test credentials.
		for name, b := range f.Bindings {
			if !b.Synthetic {
				return nil, fmt.Errorf("%s: binding %q is not marked synthetic; real credentials are refused where file custody cannot be verified", path, name)
			}
		}
	}
	return f.Bindings, nil
}

// LoadIntegrations reads every *.json artifact in dir, in name order.
func LoadIntegrations(dir string) ([]*integration.Compiled, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no integration artifacts (*.json) in %s", dir)
	}
	var out []*integration.Compiled
	for _, p := range paths {
		c, err := integration.LoadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
