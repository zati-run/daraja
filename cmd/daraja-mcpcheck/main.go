// Command daraja-mcpcheck calls a Daraja runtime over MCP Streamable HTTP
// with a client that does not use the MCP SDK. It prints JSON lines with
// timings, for evidence.
//
//	daraja-mcpcheck -url http://host:8080/mcp -token-file t -list
//	daraja-mcpcheck -url ... -token-file t -call app__get_item -args '{"id":1}'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zati-run/daraja/mcpcheck"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/mcp", "MCP endpoint")
	tokenFile := flag.String("token-file", "", "file holding the bearer token")
	list := flag.Bool("list", false, "list tools")
	call := flag.String("call", "", "tool to call")
	args := flag.String("args", "{}", "tool arguments as a JSON object")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	flag.Parse()

	token := os.Getenv("DARAJA_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fail(err)
		}
		token = strings.TrimSpace(string(b))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := &mcpcheck.Client{URL: *url, Token: token}
	t0 := time.Now()
	if err := c.Initialize(ctx); err != nil {
		fail(fmt.Errorf("initialize: %w", err))
	}
	emit(map[string]any{"step": "initialize", "ms": ms(t0), "server": c.ServerInfo})
	if *list {
		t := time.Now()
		tools, err := c.ListTools(ctx)
		if err != nil {
			fail(fmt.Errorf("tools/list: %w", err))
		}
		names := []string{}
		for _, tl := range tools {
			names = append(names, tl.Name)
		}
		emit(map[string]any{"step": "tools/list", "ms": ms(t), "tools": names})
	}
	if *call != "" {
		var a map[string]any
		if err := json.Unmarshal([]byte(*args), &a); err != nil {
			fail(fmt.Errorf("-args: %w", err))
		}
		t := time.Now()
		r, err := c.CallTool(ctx, *call, a)
		if err != nil {
			fail(fmt.Errorf("tools/call: %w", err))
		}
		emit(map[string]any{"step": "tools/call", "tool": *call, "ms": ms(t), "is_error": r.IsError, "result": json.RawMessage(orNull(r.StructuredContent)), "text": r.Text(), "meta": r.Meta})
		if r.IsError {
			os.Exit(2)
		}
	}
}

func orNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func fail(err error) {
	emit(map[string]any{"error": err.Error()})
	os.Exit(1)
}
