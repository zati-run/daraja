package capability

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Registration authorizes one capability to serve some credential bindings.
type Registration struct {
	ID          string   `json:"id"`
	TokenSHA256 string   `json:"token_sha256"`
	Bindings    []string `json:"bindings"`
}

// BrokerConfig configures a Broker.
type BrokerConfig struct {
	Registrations []Registration
	// PollWait is how long a poll is held open when no job is queued.
	// Zero means 25 seconds.
	PollWait time.Duration
	// LiveFor is how long after its last poll a capability counts as
	// registered. Zero means PollWait plus 15 seconds.
	LiveFor time.Duration
	// PickupTimeout bounds how long a job waits to be picked up before it is
	// withdrawn as not sent. Zero means 10 seconds.
	PickupTimeout time.Duration
	// MaxQueue bounds queued jobs per binding. Zero means 8.
	MaxQueue int
	Logger   *slog.Logger
}

// Broker runs inside the runtime. It hands jobs to capabilities that poll
// for them and waits for their outcomes.
type Broker struct {
	cfg  BrokerConfig
	regs []Registration
	log  *slog.Logger

	mu     sync.Mutex
	live   map[string]*liveCap
	queue  map[string][]*pending // by binding
	byID   map[string]*pending
	wakeup chan struct{}
}

type liveCap struct {
	desc     Description
	lastSeen time.Time
	polling  int
}

type jobState int

const (
	queued jobState = iota
	delivered
	withdrawn
	done
)

type pending struct {
	job    Job
	state  jobState
	capID  string
	result chan Outcome
}

// NewBroker validates registrations and returns a Broker.
func NewBroker(cfg BrokerConfig) (*Broker, error) {
	if cfg.PollWait == 0 {
		cfg.PollWait = 25 * time.Second
	}
	if cfg.LiveFor == 0 {
		cfg.LiveFor = cfg.PollWait + 15*time.Second
	}
	if cfg.PickupTimeout == 0 {
		cfg.PickupTimeout = 10 * time.Second
	}
	if cfg.MaxQueue == 0 {
		cfg.MaxQueue = 8
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	seen := map[string]bool{}
	for _, r := range cfg.Registrations {
		if r.ID == "" || seen[r.ID] {
			return nil, fmt.Errorf("capability: registration id %q is empty or duplicate", r.ID)
		}
		seen[r.ID] = true
		if len(r.TokenSHA256) != 64 {
			return nil, fmt.Errorf("capability %q: token_sha256 must be 64 hex characters", r.ID)
		}
		if len(r.Bindings) == 0 {
			return nil, fmt.Errorf("capability %q: at least one binding is required", r.ID)
		}
	}
	return &Broker{
		cfg: cfg, regs: cfg.Registrations, log: cfg.Logger,
		live: map[string]*liveCap{}, queue: map[string][]*pending{}, byID: map[string]*pending{},
		wakeup: make(chan struct{}),
	}, nil
}

// HashToken returns the hex SHA-256 of a capability token.
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

func (b *Broker) authenticate(req *http.Request) (*Registration, bool) {
	tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, false
	}
	h := []byte(HashToken(tok))
	var found *Registration
	for i := range b.regs {
		if subtle.ConstantTimeCompare(h, []byte(b.regs[i].TokenSHA256)) == 1 {
			found = &b.regs[i]
		}
	}
	return found, found != nil
}

// Handler serves the poll and result endpoints.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PollPath, b.handlePoll)
	mux.HandleFunc("POST "+ResultPath, b.handleResult)
	return mux
}

// Live reports whether a capability serving binding and ref is registered.
func (b *Broker) Live(binding string, ref OpRef) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.liveFor(binding, ref)
}

func (b *Broker) liveFor(binding string, ref OpRef) bool {
	for id, lc := range b.live {
		reg := b.reg(id)
		if reg == nil || !slices.Contains(reg.Bindings, binding) {
			continue
		}
		if (lc.polling > 0 || time.Since(lc.lastSeen) < b.cfg.LiveFor) && lc.desc.serves(binding, ref) {
			return true
		}
	}
	return false
}

func (b *Broker) reg(id string) *Registration {
	for i := range b.regs {
		if b.regs[i].ID == id {
			return &b.regs[i]
		}
	}
	return nil
}

func (b *Broker) broadcast() {
	close(b.wakeup)
	b.wakeup = make(chan struct{})
}

func (b *Broker) handlePoll(w http.ResponseWriter, req *http.Request) {
	reg, ok := b.authenticate(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var desc Description
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&desc); err != nil {
		http.Error(w, "bad description", http.StatusBadRequest)
		return
	}
	desc.ID = reg.ID
	// A capability may only describe bindings it is registered for.
	desc.Bindings = slices.DeleteFunc(desc.Bindings, func(bd BindingDesc) bool { return !slices.Contains(reg.Bindings, bd.Binding) })

	b.mu.Lock()
	lc := b.live[reg.ID]
	if lc == nil {
		lc = &liveCap{}
		b.live[reg.ID] = lc
		b.log.Info("capability registered", "capability", reg.ID, "version", desc.Version)
	}
	lc.desc, lc.lastSeen = desc, time.Now()
	lc.polling++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		lc.polling--
		lc.lastSeen = time.Now()
		b.mu.Unlock()
	}()

	timer := time.NewTimer(b.cfg.PollWait)
	defer timer.Stop()
	for {
		b.mu.Lock()
		p := b.takeLocked(reg.ID, desc)
		wake := b.wakeup
		b.mu.Unlock()
		if p != nil {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(p.job); err != nil {
				// The job may or may not have reached the capability; it
				// stays delivered and resolves as lost if no result comes.
				b.log.Warn("capability job delivery failed", "capability", reg.ID, "error", err)
			}
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-req.Context().Done():
			return
		}
	}
}

// takeLocked marks the first queued job this capability can serve as
// delivered.
func (b *Broker) takeLocked(capID string, desc Description) *pending {
	reg := b.reg(capID)
	for _, binding := range reg.Bindings {
		q := b.queue[binding]
		for i, p := range q {
			if p.state == queued && desc.serves(binding, p.job.OpRef) {
				p.state, p.capID = delivered, capID
				b.queue[binding] = slices.Delete(q, i, i+1)
				return p
			}
		}
	}
	return nil
}

func (b *Broker) handleResult(w http.ResponseWriter, req *http.Request) {
	reg, ok := b.authenticate(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var out Outcome
	if err := json.NewDecoder(io.LimitReader(req.Body, MaxResult)).Decode(&out); err != nil {
		http.Error(w, "bad outcome", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	p := b.byID[out.JobID]
	if p == nil || p.capID != reg.ID {
		b.mu.Unlock()
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	if p.state != delivered {
		b.mu.Unlock()
		// Already resolved (for example as lost after a timeout).
		w.WriteHeader(http.StatusGone)
		return
	}
	p.state = done
	b.mu.Unlock()
	p.result <- out
	w.WriteHeader(http.StatusNoContent)
}

// Execute queues a job for a capability serving its binding and waits for
// the outcome. It always returns an outcome: failures to hand the job over
// are NotSent, and a job handed over without an answer is Unknown.
func (b *Broker) Execute(ctx context.Context, job Job) Outcome {
	job.ID = newID()
	b.mu.Lock()
	if !b.liveFor(job.Binding, job.OpRef) {
		b.mu.Unlock()
		return Outcome{JobID: job.ID, Dispatch: NotSent, Code: CodeUnavailable, Detail: "no registered capability serves this binding and artifact"}
	}
	if len(b.queue[job.Binding]) >= b.cfg.MaxQueue {
		b.mu.Unlock()
		return Outcome{JobID: job.ID, Dispatch: NotSent, Code: CodeBusy, Detail: "the capability queue for this binding is full"}
	}
	p := &pending{job: job, result: make(chan Outcome, 1)}
	b.queue[job.Binding] = append(b.queue[job.Binding], p)
	b.byID[job.ID] = p
	b.broadcast()
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.byID, job.ID)
		b.mu.Unlock()
	}()

	pickup := time.NewTimer(b.cfg.PickupTimeout)
	defer pickup.Stop()
	for {
		select {
		case out := <-p.result:
			out.JobID = job.ID
			if !out.Legal() {
				return Outcome{JobID: job.ID, Dispatch: Unknown, Code: CodeProtocol, Detail: "the capability returned an illegal outcome"}
			}
			return out
		case <-pickup.C:
			if o, ok := b.withdraw(p, "no capability picked up the job in time"); ok {
				return o
			}
		case <-ctx.Done():
			if o, ok := b.withdraw(p, "the call ended before a capability picked up the job"); ok {
				return o
			}
			b.mu.Lock()
			resolved := p.state == done
			if !resolved {
				p.state = withdrawn
			}
			b.mu.Unlock()
			if resolved {
				continue // the result is in the channel
			}
			return Outcome{JobID: job.ID, Dispatch: Unknown, Code: CodeLost, Detail: "the capability received the job but its answer did not arrive"}
		}
	}
}

// withdraw removes a job that no capability has picked up. It reports false
// once the job has been delivered.
func (b *Broker) withdraw(p *pending, detail string) (Outcome, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.state != queued {
		return Outcome{}, false
	}
	p.state = withdrawn
	q := b.queue[p.job.Binding]
	if i := slices.Index(q, p); i >= 0 {
		b.queue[p.job.Binding] = slices.Delete(q, i, i+1)
	}
	return Outcome{JobID: p.job.ID, Dispatch: NotSent, Code: CodeUnavailable, Detail: detail}, true
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
