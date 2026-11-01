package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeginOnceAndRecoverDispatching(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	k1 := Key("p", "i", "op", "a")
	k2 := Key("p", "i", "op", "b")
	if _, err := j.Begin(Receipt{Key: k1, Marker: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Begin(Receipt{Key: k1}); !errors.Is(err, ErrExists) {
		t.Fatalf("second Begin err = %v", err)
	}
	r2, _ := j.Begin(Receipt{Key: k2})
	r2.State, r2.Identifier = Succeeded, "42"
	if _, err := j.Update(r2); err != nil {
		t.Fatal(err)
	}
	j.Close() // simulate a stop while k1 is dispatching

	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	r1, _ := j.Get(k1)
	if r1.State != Uncertain || r1.Code != "restart_during_dispatch" || r1.Marker != "m1" {
		t.Fatalf("k1 after restart = %+v", r1)
	}
	if len(j.Recovered) != 1 {
		t.Fatalf("Recovered = %d", len(j.Recovered))
	}
	if r, _ := j.Get(k2); r.State != Succeeded || r.Identifier != "42" {
		t.Fatalf("k2 after restart = %+v", r)
	}
	if _, err := j.Begin(Receipt{Key: k1}); !errors.Is(err, ErrExists) {
		t.Fatal("uncertain receipt could be begun again")
	}
}

func TestTornFinalLineTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := Open(path)
	r, _ := j.Begin(Receipt{Key: "k"})
	r.State = Succeeded
	j.Update(r)
	j.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"key":"x","sta`)
	f.Close()
	j, err := Open(path)
	if err != nil {
		t.Fatalf("torn line: %v", err)
	}
	defer j.Close()
	if r, _ := j.Get("k"); r.State != Succeeded {
		t.Fatalf("k = %+v", r)
	}
}

func TestKeyIsUnambiguous(t *testing.T) {
	if Key("a|b", "c", "d", "e") == Key("a", "b|c", "d", "e") {
		t.Fatal("keys collide across field boundaries")
	}
	if strings.Contains(Key("p", "i", "o", "secret-key"), "secret") {
		t.Fatal("raw key leaked")
	}
}

// After a torn final line, the next append must not fuse onto it: the
// journal has to open again with every receipt.
func TestTornLineDoesNotCorruptNextStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := j.Begin(Receipt{Key: "k"})
	r.State = Succeeded
	if _, err := j.Update(r); err != nil {
		t.Fatal(err)
	}
	j.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"key":"x","sta`)
	f.Close()
	j, err = Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if j.TornBytes != len(`{"key":"x","sta`) {
		t.Fatalf("torn bytes = %d", j.TornBytes)
	}
	if _, err := j.Begin(Receipt{Key: "m"}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j, err = Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer j.Close()
	if r, ok := j.Get("k"); !ok || r.State != Succeeded {
		t.Fatalf("k = %+v", r)
	}
	if r, ok := j.Get("m"); !ok || r.State != Uncertain {
		t.Fatalf("m = %+v, want uncertain after restart", r)
	}
	if _, ok := j.Get("x"); ok {
		t.Fatal("the torn receipt was kept")
	}
}

// Markers are a prefix of the key; keys must be lowercase hex digests long
// enough that the prefix stays unambiguous.
func TestKeyIsHexDigest(t *testing.T) {
	k := Key("p", "i", "o", "idem")
	if len(k) < 24 {
		t.Fatalf("key %q is shorter than the 24-character marker prefix", k)
	}
	for _, c := range k {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("key %q is not lowercase hex", k)
		}
	}
}
