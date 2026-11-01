// Package mcpserver exposes a runtime's operations as MCP tools using the
// official MCP Go SDK. Each principal sees only the tools in its scope.
//
// Tool names are "<integration>__<operation>". A write's input schema adds
// a required idempotency_key. Control tools daraja__receipt and
// daraja__reconcile appear when the principal can call any write.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zati-run/daraja/integration"
	"github.com/zati-run/daraja/journal"
	"github.com/zati-run/daraja/runtime"
)

// Version is the public tool-contract version.
const Version = "v1alpha1"

// ToolName returns the MCP tool name of an operation.
func ToolName(integ, op string) string { return integ + "__" + op }

func splitTool(name string) (string, string, bool) {
	i := strings.Index(name, "__")
	if i <= 0 || i+2 >= len(name) {
		return "", "", false
	}
	return name[:i], name[i+2:], true
}

// NewServer builds an MCP server for one principal.
func NewServer(rt *runtime.Runtime, p *runtime.Principal, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "daraja", Title: "Daraja integration runtime", Version: version}, &mcp.ServerOptions{
		Instructions: "Daraja tools act in a source application with the configured user's own session. " +
			"Writes need an idempotency_key. If a write reports outcome_uncertain, call daraja__reconcile with the same key; never retry it with a new key until the receipt is final.",
	})
	hasWrite := false
	for _, e := range rt.Entries(p) {
		addOperation(s, rt, p, e)
		if e.Op.Effect == integration.EffectWrite {
			hasWrite = true
		}
	}
	if hasWrite {
		addControl(s, rt, p)
	}
	return s
}

func addOperation(s *mcp.Server, rt *runtime.Runtime, p *runtime.Principal, e runtime.Entry) {
	op := e.Op
	in := e.Integration
	var schema map[string]any
	_ = json.Unmarshal(op.InputSchema, &schema)
	var outSchema map[string]any
	_ = json.Unmarshal(op.OutputSchema, &outSchema)
	readOnly := op.Effect == integration.EffectRead
	ann := &mcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: readOnly, OpenWorldHint: ptr(true)}
	desc := op.Description
	if !readOnly {
		ann.DestructiveHint = ptr(op.IsDestructive())
		schema = withIdempotencyKey(schema)
		desc += " This is a write in the source application. Supply a unique idempotency_key; repeating a key never repeats the write."
	}
	if outSchema["type"] != "object" {
		outSchema = nil
	}
	tool := &mcp.Tool{
		Name:        ToolName(in.ID, op.Name),
		Description: desc,
		InputSchema: schema,
		Annotations: ann,
	}
	if outSchema != nil {
		tool.OutputSchema = outSchema
	}
	s.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return failure(&runtime.Error{Code: runtime.CodeInvalidInput, Message: "arguments must be a JSON object"}), nil
			}
		}
		key := ""
		if !readOnly {
			k, _ := args["idempotency_key"].(string)
			key = k
			delete(args, "idempotency_key")
		}
		res, rerr := rt.Invoke(ctx, p, in.ID, op.Name, args, key)
		if rerr != nil {
			return failure(rerr), nil
		}
		return success(res.Output, res.Receipt), nil
	})
}

func withIdempotencyKey(schema map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range schema {
		out[k] = v
	}
	props := map[string]any{}
	if p, ok := schema["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	props["idempotency_key"] = map[string]any{
		"type": "string", "minLength": 1, "maxLength": 200,
		"description": "Caller-chosen unique key for this write. Reuse it only to ask about the same write.",
	}
	out["properties"] = props
	req := []any{"idempotency_key"}
	if r, ok := schema["required"].([]any); ok {
		req = append(req, r...)
	}
	out["required"] = req
	return out
}

var controlInput = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"tool", "idempotency_key"},
	"properties": map[string]any{
		"tool":            map[string]any{"type": "string", "description": "The write tool name, such as app__create_item."},
		"idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
	},
}

func addControl(s *mcp.Server, rt *runtime.Runtime, p *runtime.Principal) {
	parse := func(req *mcp.CallToolRequest) (string, string, string, *runtime.Error) {
		var a struct {
			Tool string `json:"tool"`
			Key  string `json:"idempotency_key"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil || a.Tool == "" || a.Key == "" {
			return "", "", "", &runtime.Error{Code: runtime.CodeInvalidInput, Message: "tool and idempotency_key are required"}
		}
		integ, op, ok := splitTool(a.Tool)
		if !ok {
			return "", "", "", &runtime.Error{Code: runtime.CodeUnknownOperation, Message: fmt.Sprintf("%q is not a Daraja tool name", a.Tool)}
		}
		return integ, op, a.Key, nil
	}
	s.AddTool(&mcp.Tool{
		Name:        "daraja__receipt",
		Description: "Read the receipt of a write by its idempotency key. Sends nothing to the source application.",
		InputSchema: controlInput,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		integ, op, key, e := parse(req)
		if e != nil {
			return failure(e), nil
		}
		rec, e := rt.Receipt(p, integ, op, key)
		if e != nil {
			return failure(e), nil
		}
		return receiptResult(rec), nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "daraja__reconcile",
		Description: "Resolve an uncertain write by reading the source application for its marker. Never re-sends the write.",
		InputSchema: controlInput,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		integ, op, key, e := parse(req)
		if e != nil {
			return failure(e), nil
		}
		rec, e := rt.Reconcile(ctx, p, integ, op, key)
		if e != nil {
			return failure(e), nil
		}
		return receiptResult(rec), nil
	})
}

func receiptResult(rec *journal.Receipt) *mcp.CallToolResult {
	b, _ := json.Marshal(publicReceipt(rec))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: m}
}

// publicReceipt omits the internal key hash and principal.
func publicReceipt(r *journal.Receipt) map[string]any {
	if r == nil {
		return nil
	}
	m := map[string]any{"state": r.State, "integration": r.Integration, "operation": r.Operation, "at": r.At}
	if r.Identifier != "" {
		m["identifier"] = r.Identifier
	}
	if r.HTTPStatus != 0 {
		m["http_status"] = r.HTTPStatus
	}
	if r.Code != "" {
		m["code"] = r.Code
	}
	if r.Detail != "" {
		m["detail"] = r.Detail
	}
	if r.Marker != "" {
		m["marker"] = r.Marker
	}
	return m
}

func success(out map[string]any, rec *journal.Receipt) *mcp.CallToolResult {
	b, _ := json.Marshal(out)
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: out}
	if rec != nil {
		res.Meta = mcp.Meta{"daraja/receipt": publicReceipt(rec)}
	}
	return res
}

func failure(e *runtime.Error) *mcp.CallToolResult {
	body := map[string]any{"code": e.Code, "message": e.Message}
	if e.Receipt != nil {
		body["receipt"] = publicReceipt(e.Receipt)
	}
	b, _ := json.Marshal(body)
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

func ptr[T any](v T) *T { return &v }

type ctxKey struct{}

// Handler serves MCP over Streamable HTTP with bearer-token principals.
type Handler struct {
	rt        *runtime.Runtime
	auth      Authenticator
	challenge string
	version   string
	mu        sync.Mutex
	servers   map[string]*mcp.Server
	owners    map[string]string // MCP session ID -> principal ID
	inner     http.Handler
}

// Authenticator maps a bearer token to a principal.
type Authenticator func(token string) (*runtime.Principal, bool)

// Option configures a Handler.
type Option func(*Handler)

// WithAuthenticator replaces the default authenticator (the runtime's
// static principal tokens), for example with OAuth access tokens.
func WithAuthenticator(a Authenticator) Option { return func(h *Handler) { h.auth = a } }

// WithChallenge sets the WWW-Authenticate value sent with a 401, for
// example one that points to OAuth protected resource metadata.
func WithChallenge(v string) Option { return func(h *Handler) { h.challenge = v } }

// AnyOf tries authenticators in order.
func AnyOf(as ...Authenticator) Authenticator {
	return func(token string) (*runtime.Principal, bool) {
		for _, a := range as {
			if p, ok := a(token); ok {
				return p, true
			}
		}
		return nil, false
	}
}

// NewHandler returns an http.Handler for the MCP endpoint.
func NewHandler(rt *runtime.Runtime, version string, opts ...Option) *Handler {
	h := &Handler{rt: rt, auth: rt.Authenticate, challenge: `Bearer realm="daraja"`, version: version, servers: map[string]*mcp.Server{}, owners: map[string]string{}}
	for _, o := range opts {
		o(h)
	}
	h.inner = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, _ := r.Context().Value(ctxKey{}).(*runtime.Principal)
		if p == nil {
			return nil
		}
		return h.serverFor(p)
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})
	return h
}

func (h *Handler) serverFor(p *runtime.Principal) *mcp.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.servers[p.ID]
	if !ok {
		s = NewServer(h.rt, p, h.version)
		h.servers[p.ID] = s
	}
	return s
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	p, found := h.auth(strings.TrimSpace(token))
	if !ok || !found {
		w.Header().Set("WWW-Authenticate", h.challenge)
		http.Error(w, "a valid bearer token is required", http.StatusUnauthorized)
		return
	}
	// A session started by one principal must not be usable by another.
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		owner, known := h.owners[sid]
		h.mu.Unlock()
		if known && owner != p.ID {
			http.Error(w, "session belongs to another principal", http.StatusNotFound)
			return
		}
	}
	sw := &sessionWriter{ResponseWriter: w, h: h, principal: p.ID}
	h.inner.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
}

// sessionWriter records which principal created each MCP session.
type sessionWriter struct {
	http.ResponseWriter
	h         *Handler
	principal string
	done      bool
}

func (s *sessionWriter) record() {
	if s.done {
		return
	}
	s.done = true
	if sid := s.Header().Get("Mcp-Session-Id"); sid != "" {
		s.h.mu.Lock()
		if _, ok := s.h.owners[sid]; !ok {
			s.h.owners[sid] = s.principal
		}
		s.h.mu.Unlock()
	}
}

func (s *sessionWriter) WriteHeader(code int) { s.record(); s.ResponseWriter.WriteHeader(code) }
func (s *sessionWriter) Write(b []byte) (int, error) {
	s.record()
	return s.ResponseWriter.Write(b)
}
func (s *sessionWriter) Flush() {
	s.record()
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *sessionWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
