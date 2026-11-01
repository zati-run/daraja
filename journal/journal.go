// Package journal is the runtime's durable receipt journal for writes.
//
// It is an append-only JSON Lines file. Each record is flushed to stable
// storage (fsync) before Append returns, so a write's "dispatching" record
// is durable before the request leaves the process. On open, any receipt
// whose latest state is "dispatching" becomes "uncertain": the process
// stopped after it may have sent the request and before it recorded the
// outcome.
//
// Records hold no credentials, cookies, request bodies or response bodies.
package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// States of a write receipt.
const (
	Dispatching = "dispatching" // durable before send; never a final state
	Succeeded   = "succeeded"   // the source confirmed the effect
	Failed      = "failed"      // the source definitively refused, or nothing was sent
	Uncertain   = "uncertain"   // the request may have been applied
	NotApplied  = "not_applied" // reconciliation found no effect
)

// Receipt is the latest known state of one write invocation.
type Receipt struct {
	Key         string `json:"key"` // hash of principal, integration, operation, idempotency key
	Principal   string `json:"principal"`
	Integration string `json:"integration"`
	Operation   string `json:"operation"`
	State       string `json:"state"`
	Marker      string `json:"marker,omitempty"`
	// InputSHA256 is a hash of the validated input. A key reused with
	// different input is a conflict, never a replay.
	InputSHA256 string    `json:"input_sha256,omitempty"`
	HTTPStatus  int       `json:"http_status,omitempty"`
	Identifier  string    `json:"identifier,omitempty"`
	Code        string    `json:"code,omitempty"`   // machine-readable reason
	Detail      string    `json:"detail,omitempty"` // human-readable, no secrets or data
	DispatchAt  time.Time `json:"dispatch_at,omitzero"`
	At          time.Time `json:"at"`
	Seq         int64     `json:"seq"`
}

// Final reports whether no further transition is expected without
// reconciliation.
func (r Receipt) Final() bool {
	return r.State == Succeeded || r.State == Failed || r.State == NotApplied
}

// Key derives a receipt key. Raw idempotency keys are not stored.
func Key(principal, integration, operation, idempotencyKey string) string {
	h := sha256.New()
	for _, s := range []string{principal, integration, operation, idempotencyKey} {
		fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Journal is safe for concurrent use.
type Journal struct {
	// TornBytes is how many bytes of a torn final line Open dropped.
	TornBytes int

	mu     sync.Mutex
	f      *os.File
	latest map[string]Receipt
	seq    int64
	now    func() time.Time
	// Recovered lists receipts that were dispatching at open and are now uncertain.
	Recovered []Receipt
	// SyncAll also calls sync(2) after each record's fsync, for platforms
	// where fsync of one file is not known to reach the disk (set before
	// the first write).
	SyncAll bool
}

// Open opens or creates the journal at path and replays it.
func Open(path string) (*Journal, error) {
	torn, err := dropTornTail(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Replay through a separate read-only handle. Reading through the
	// O_APPEND handle depends on its initial offset being 0, which is not
	// true everywhere: on Nanos it starts at the end, so the journal read
	// as empty after a restart (RFC A11 on Nanos).
	rf, err := os.Open(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	defer rf.Close()
	j := &Journal{TornBytes: torn, f: f, latest: map[string]Receipt{}, now: func() time.Time { return time.Now().UTC() }}
	sc := bufio.NewScanner(rf)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		var r Receipt
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			// A torn final line from a crash mid-write is tolerated; anything
			// else is corruption.
			if !sc.Scan() {
				break
			}
			f.Close()
			return nil, fmt.Errorf("journal %s line %d: %w", path, line, err)
		}
		j.latest[r.Key] = r
		j.seq = max(j.seq, r.Seq)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, err
	}
	for _, r := range j.latest {
		if r.State == Dispatching {
			r.State = Uncertain
			r.Code = "restart_during_dispatch"
			r.Detail = "the runtime stopped after the request may have been sent"
			if err := j.appendLocked(r); err != nil {
				f.Close()
				return nil, err
			}
			j.Recovered = append(j.Recovered, j.latest[r.Key])
		}
	}
	return j, nil
}

// dropTornTail truncates a final line left without its newline by a crash
// mid-append. Appending after it would fuse the next record onto it and
// make the journal unreadable on the following start. Dropping it is safe:
// Begin is durable before dispatch, so a torn line is either a receipt
// whose request was never sent or an outcome update whose earlier
// dispatching line still becomes uncertain.
func dropTornTail(path string) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return 0, nil
	}
	keep := bytes.LastIndexByte(b, '\n') + 1
	if err := os.Truncate(path, int64(keep)); err != nil {
		return 0, fmt.Errorf("journal %s: dropping a torn final line: %w", path, err)
	}
	return len(b) - keep, nil
}

// Get returns the latest receipt for a key.
func (j *Journal) Get(key string) (Receipt, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.latest[key]
	return r, ok
}

// ErrExists reports an attempt to begin a receipt that already exists.
var ErrExists = errors.New("receipt already exists")

// Begin durably records a new receipt in the dispatching state. It fails
// with ErrExists if the key has any receipt, so a key is dispatched at most
// once.
func (j *Journal) Begin(r Receipt) (Receipt, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if old, ok := j.latest[r.Key]; ok {
		return old, ErrExists
	}
	r.State = Dispatching
	r.DispatchAt = j.now()
	if err := j.appendLocked(r); err != nil {
		return Receipt{}, err
	}
	return j.latest[r.Key], nil
}

// Update durably records a new state for an existing receipt.
func (j *Journal) Update(r Receipt) (Receipt, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	old, ok := j.latest[r.Key]
	if !ok {
		return Receipt{}, fmt.Errorf("journal: no receipt %s", r.Key)
	}
	r.DispatchAt = old.DispatchAt
	if err := j.appendLocked(r); err != nil {
		return Receipt{}, err
	}
	return j.latest[r.Key], nil
}

func (j *Journal) appendLocked(r Receipt) error {
	j.seq++
	r.Seq = j.seq
	r.At = j.now()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := j.f.Write(b); err != nil {
		return fmt.Errorf("journal write: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("journal sync: %w", err)
	}
	if j.SyncAll {
		syscall.Sync()
	}
	j.latest[r.Key] = r
	return nil
}

// Close closes the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}
