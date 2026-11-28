// Package mcpstdio serves typed tools over a newline-delimited JSON-RPC stdio MCP loop.
package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// DefaultProtocolVersion is the newest MCP revision served; 2026-07-28 is absent: it retires initialize.
const DefaultProtocolVersion = "2025-11-25"

const (
	rpcInternalError  = -32603
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcParseError     = -32700
	readBufferBytes   = 64 * 1024
	// requestConcurrency caps simultaneous tool requests.
	requestConcurrency = 8
	// maxLineBytes caps one JSON-RPC line; a longer line is drained and answered with a parse error.
	maxLineBytes = 4 << 20
)

// Tool is one MCP tool. An error from Call becomes an isError tool result carrying its text.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(ctx context.Context, args json.RawMessage) (text string, err error)
}

// Server serves Tools over stdio.
type Server struct {
	Name, Version string
	Tools         []Tool
	CancelOnClose bool // cancel in-flight calls when input ends or a write fails, instead of joining them
}

// rpcRequest is one JSON-RPC 2.0 request. Missing "id" (checked separately) marks a notification.
type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func result(id json.RawMessage, res any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": res}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "error": map[string]any{"code": code, "message": message}}
}

// rawID returns the id as-is, or JSON null when absent, so error responses always carry an id field.
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

// toolError is an isError tool result, not a JSON-RPC error, so the model sees the failure and reacts.
func toolError(text string) map[string]any {
	res := toolText(text)
	res["isError"] = true
	return res
}

func toolText(text string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
}

// supportedProtocolVersions are the MCP revisions served.
var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true, "2025-03-26": true, "2025-06-18": true, "2025-11-25": true,
}

// negotiateProtocolVersion echoes the client's revision when supported, else our newest (the spec's rule).
func negotiateProtocolVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err == nil && supportedProtocolVersions[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return DefaultProtocolVersion
}

func (s *Server) descriptors() []map[string]any {
	out := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	return out
}

// handle dispatches one request. A notification (no id) gets no reply, whatever its method.
func (s *Server) handle(ctx context.Context, req rpcRequest) map[string]any {
	if len(req.ID) == 0 {
		return nil
	}
	switch req.Method {
	case "initialize":
		return result(req.ID, map[string]any{
			"protocolVersion": negotiateProtocolVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		})
	case "ping":
		return result(req.ID, map[string]any{})
	case "tools/list":
		return result(req.ID, map[string]any{"tools": s.descriptors()})
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "notifications/initialized":
		return nil
	}
	return rpcError(req.ID, rpcMethodNotFound, fmt.Sprintf("Method not found: %q", req.Method))
}

func (s *Server) handleToolsCall(ctx context.Context, req rpcRequest) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return result(req.ID, toolError(fmt.Sprintf("invalid tool arguments: %v", err)))
	}
	for _, t := range s.Tools {
		if t.Name != p.Name {
			continue
		}
		text, err := t.Call(ctx, p.Arguments)
		if err != nil {
			return result(req.ID, toolError(err.Error()))
		}
		return result(req.ID, toolText(text))
	}
	return rpcError(req.ID, rpcInvalidParams, fmt.Sprintf("Unknown tool: %q", p.Name))
}

// Serve bounds concurrent JSON-RPC requests, serializes replies, and joins in-flight work at EOF.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, requestConcurrency)
	reader := bufio.NewReaderSize(in, readBufferBytes)
	enc := json.NewEncoder(out)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex // guards enc and writeErr
		writeErr error
	)
	send := func(resp map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if writeErr == nil {
			writeErr = enc.Encode(resp)
			if writeErr != nil && s.CancelOnClose {
				cancel()
			}
		}
	}
	readErr := s.readLoop(ctx, reader, send, slots, &wg)
	if s.CancelOnClose {
		cancel()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// readLoop launches one call per request line until EOF, a read error or ctx ends; EOF is nil.
func (s *Server) readLoop(ctx context.Context, reader *bufio.Reader, send func(map[string]any), slots chan struct{}, wg *sync.WaitGroup) error {
	for {
		raw, tooLong, err := readRequestLine(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if tooLong {
			send(rpcError(nil, rpcParseError, "Parse error: line too long"))
			continue
		}
		req, ok := parseRequest(raw, send)
		if !ok {
			continue
		}
		if !admitRequest(ctx, slots) {
			return context.Cause(ctx)
		}
		s.launch(ctx, req, send, slots, wg)
	}
}

func admitRequest(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) launch(ctx context.Context, req rpcRequest, send func(map[string]any), slots chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	finish := func() { <-slots; wg.Done() }
	parallel.Go(func() {
		if resp := s.handle(ctx, req); resp != nil {
			send(resp)
		}
		finish()
	}, func(error) {
		defer finish()
		send(rpcError(req.ID, rpcInternalError, "Internal error"))
	})
}

// readRequestLine reads one capped line; an unterminated last line is served and the next read reports EOF.
func readRequestLine(r *bufio.Reader) (raw []byte, tooLong bool, err error) {
	raw, tooLong, err = textutil.ReadLineCapped(r, maxLineBytes)
	if err == io.EOF && (len(raw) > 0 || tooLong) {
		err = nil
	}
	return raw, tooLong, err
}

// parseRequest decodes one line; ok is false for a blank line (skipped) or bad JSON (answered via send).
func parseRequest(raw []byte, send func(map[string]any)) (req rpcRequest, ok bool) {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 {
		return req, false
	}
	if err := json.Unmarshal(line, &req); err != nil {
		send(rpcError(nil, rpcParseError, "Parse error"))
		return req, false
	}
	return req, true
}
