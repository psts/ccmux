// Minimal MCP stdio server: newline-delimited JSON-RPC 2.0 on stdout, logs on
// stderr. Hand-rolled because the load-bearing surface is tiny and nonstandard
// — the experimental claude/channel + claude/channel/permission capabilities
// and their custom notifications are the whole point of this process.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent → notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// mcpServer owns the stdio transport. Handlers run on the read loop; Notify is
// safe from any goroutine (writes are mutex-serialized).
type mcpServer struct {
	in  *bufio.Scanner
	out io.Writer
	mu  sync.Mutex

	// onRequest maps method → handler returning (result, error). onNotify maps
	// method → handler for client-initiated notifications.
	onRequest map[string]func(params json.RawMessage) (any, *rpcError)
	onNotify  map[string]func(params json.RawMessage)
}

func newMCPServer() *mcpServer { return newMCPServerIO(os.Stdin, os.Stdout) }

func newMCPServerIO(r io.Reader, w io.Writer) *mcpServer {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // permission previews / long prompts
	return &mcpServer{
		in: sc, out: w,
		onRequest: map[string]func(json.RawMessage) (any, *rpcError){},
		onNotify:  map[string]func(json.RawMessage){},
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[claude-peers] "+format+"\n", args...)
}

func (m *mcpServer) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b = append(b, '\n')
	_, err = m.out.Write(b)
	return err
}

// Notify emits a server-initiated notification (the channel push).
func (m *mcpServer) Notify(method string, params any) error {
	return m.writeJSON(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

// Serve reads stdin until EOF (parent exit), dispatching requests and
// notifications. Returns when the pipe closes.
func (m *mcpServer) Serve() {
	for m.in.Scan() {
		line := m.in.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			logf("bad frame: %v", err)
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			if h := m.onNotify[req.Method]; h != nil {
				h(req.Params)
			}
			continue
		}
		h := m.onRequest[req.Method]
		if h == nil {
			_ = m.writeJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
			continue
		}
		result, rpcErr := h(req.Params)
		if rpcErr != nil {
			_ = m.writeJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
			continue
		}
		_ = m.writeJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
	}
}

// toolText renders a tools/call result: one text content block.
func toolText(text string, isErr bool) any {
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
	if isErr {
		res["isError"] = true
	}
	return res
}
