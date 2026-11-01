// Package mcpcheck is a minimal MCP client for the Streamable HTTP
// transport, written directly against the protocol (JSON-RPC 2.0 over HTTP
// POST) without the MCP SDK. It exists so Daraja's server is exercised by
// an implementation that does not share the server's code.
package mcpcheck

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// ProtocolVersion is the MCP revision this client requests.
const ProtocolVersion = "2025-06-18"

// Client is one MCP session.
type Client struct {
	URL        string
	Token      string
	HTTP       *http.Client
	session    string
	negotiated string
	id         atomic.Int64
	// ServerInfo is set by Initialize.
	ServerInfo map[string]any
}

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message) }

// HTTPError is a non-2xx HTTP response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Body) }

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) post(ctx context.Context, msg map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.negotiated != "" {
		req.Header.Set("MCP-Protocol-Version", c.negotiated)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil // notification accepted
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	_, isRequest := msg["id"]
	if !isRequest {
		return nil, nil
	}
	ct := resp.Header.Get("Content-Type")
	var raw []byte
	switch {
	case strings.HasPrefix(ct, "application/json"):
		raw, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, err
		}
	case strings.HasPrefix(ct, "text/event-stream"):
		raw, err = readSSEResponse(resp.Body, msg["id"])
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unexpected content type %q", ct)
	}
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if env.JSONRPC != "2.0" {
		return nil, errors.New("response is not JSON-RPC 2.0")
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Result, nil
}

// readSSEResponse returns the data of the first event whose JSON-RPC id
// matches.
func readSSEResponse(r io.Reader, id any) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var data strings.Builder
	want, _ := json.Marshal(id)
	flush := func() []byte {
		d := data.String()
		data.Reset()
		if d == "" {
			return nil
		}
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(d), &probe) == nil && bytes.Equal(probe.ID, want) {
			return []byte(d)
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if b := flush(); b != nil {
				return b, nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(v, " "))
		}
	}
	if b := flush(); b != nil {
		return b, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("event stream ended without a response")
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.post(ctx, msg)
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) error {
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "daraja-mcpcheck", "version": "0"},
	})
	if err != nil {
		return err
	}
	var r struct {
		ProtocolVersion string         `json:"protocolVersion"`
		ServerInfo      map[string]any `json:"serverInfo"`
		Capabilities    map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	if r.ProtocolVersion == "" {
		return errors.New("initialize result has no protocolVersion")
	}
	if _, ok := r.Capabilities["tools"]; !ok {
		return errors.New("server does not advertise the tools capability")
	}
	c.negotiated = r.ProtocolVersion
	c.ServerInfo = r.ServerInfo
	_, err = c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return err
}

// Tool is a listed tool.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  map[string]any  `json:"annotations"`
}

// ListTools returns every tool, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		res, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var r struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Tools...)
		if r.NextCursor == "" {
			return all, nil
		}
		cursor = r.NextCursor
	}
}

// ToolResult is a tools/call result.
type ToolResult struct {
	IsError           bool            `json:"isError"`
	Content           []ContentItem   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Meta              map[string]any  `json:"_meta"`
}

// ContentItem is one content block.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text joins text content.
func (r *ToolResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// CallTool calls a tool.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var r ToolResult
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SessionID returns the MCP session ID the server assigned.
func (c *Client) SessionID() string { return c.session }
