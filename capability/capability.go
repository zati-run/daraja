// Package capability defines the contract between the Daraja runtime and an
// execution capability: a separately qualified process that owns a user
// session (for example a headed browser on the owner's computer) and
// executes operations for the runtime.
//
// The capability dials out to the runtime and long-polls for jobs, so it
// needs no inbound port. The runtime stays the only scheduler and the only
// receipt journal; the capability holds no receipts. The capability is an
// authority boundary because it holds the session: it runs only operations
// from its own copy of published artifacts, refuses unknown artifact hashes,
// and enforces each binding's grant itself.
//
// This package imports no browser code, so the runtime can use it on Nanos.
package capability

// Paths served by the runtime's broker.
const (
	PollPath   = "/capability/v1/poll"
	ResultPath = "/capability/v1/result"
)

// Dispatch says whether a request may have reached the source.
type Dispatch string

// Dispatch values. Only NotSent proves that nothing reached the source.
const (
	NotSent Dispatch = "not_sent"
	Sent    Dispatch = "sent"
	Unknown Dispatch = "unknown"
)

// Outcome codes a capability or the broker may report with NotSent or Unknown.
const (
	CodeUnavailable   = "capability_unavailable"
	CodeBusy          = "capability_busy"
	CodeLost          = "capability_lost"
	CodeNeedsHuman    = "session_needs_human"
	CodeSession       = "session_unavailable"
	CodeSourceChanged = "source_changed"
	CodeUnknownOp     = "unknown_artifact"
	CodeDenied        = "capability_denied"
	CodeInvalidInput  = "invalid_input"
	CodeProtocol      = "protocol_error"
)

// OpRef names one operation of one exact published artifact.
type OpRef struct {
	IntegrationID  string `json:"integration_id"`
	Version        string `json:"version"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	Operation      string `json:"operation"`
}

// Job is one operation the runtime asks a capability to execute. It never
// carries a request body or plan: the capability renders the request from
// its own copy of the artifact.
type Job struct {
	ID      string         `json:"id"`
	OpRef   OpRef          `json:"op_ref"`
	Binding string         `json:"binding"`
	Input   map[string]any `json:"input"`
	Marker  string         `json:"marker,omitempty"`
}

// Outcome is a capability's answer to a Job. When Dispatch is Sent, Status
// and Body carry the source's answer and the runtime classifies it.
type Outcome struct {
	JobID    string   `json:"job_id"`
	Dispatch Dispatch `json:"dispatch"`
	Status   int      `json:"status,omitempty"`
	Body     []byte   `json:"body,omitempty"`
	Code     string   `json:"code,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	// Redirected reports that the source answered with a redirect, which
	// is never followed for an operation. A browser hides the redirect's
	// status, so a redirected outcome may carry status 0.
	Redirected bool `json:"redirected,omitempty"`
	// Location is a redirect's target, when the capability could read it.
	Location string `json:"location,omitempty"`
}

// MaxBody is the largest source response an outcome carries. Outcome.Body
// travels base64-encoded in JSON, so a result request may be about 4/3 of
// it plus the envelope; MaxResult bounds that.
const (
	MaxBody   = 8 << 20
	MaxResult = MaxBody/3*4 + 1<<20
)

// Legal reports whether an outcome is a legal combination. The runtime
// treats an illegal outcome as uncertain.
func (o Outcome) Legal() bool {
	switch o.Dispatch {
	case NotSent:
		return o.Status == 0 && o.Code != ""
	case Sent:
		return (o.Status >= 100 && o.Status <= 599) || (o.Status == 0 && o.Redirected)
	case Unknown:
		return o.Status == 0
	}
	return false
}

// Description is what a capability announces on every poll.
type Description struct {
	ID       string        `json:"id"`
	Version  string        `json:"version"`
	Bindings []BindingDesc `json:"bindings"`
}

// BindingDesc lists the published artifacts a binding can execute.
type BindingDesc struct {
	Binding      string            `json:"binding"`
	Integrations []IntegrationDesc `json:"integrations"`
}

// IntegrationDesc names one artifact by content hash.
type IntegrationDesc struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

func (d Description) serves(binding string, ref OpRef) bool {
	for _, b := range d.Bindings {
		if b.Binding != binding {
			continue
		}
		for _, in := range b.Integrations {
			if in.ID == ref.IntegrationID && in.SHA256 == ref.ArtifactSHA256 {
				return true
			}
		}
	}
	return false
}
