// Command fakecli is a hermetic stand-in for the copilot CLI: it speaks the SDK's stdio JSON-RPC (no network, no auth).
//
// Env: FAKECLI_VERSION (reported version), FAKECLI_MODELS (comma list switchTo accepts), FAKECLI_RECORD (file that
// receives every request as "method params").
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

func main() {
	serve(os.Stdin, os.Stdout)
}

func serve(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	known := strings.Split(os.Getenv("FAKECLI_MODELS"), ",")
	rec, _ := os.OpenFile(os.Getenv("FAKECLI_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	for {
		body, ok := readBody(r)
		if !ok {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || req.Method == "" || len(req.ID) == 0 {
			continue // a response or notification: nothing to answer
		}
		fmt.Fprintf(rec, "%s %s\n", req.Method, req.Params)
		res, errMsg := reply(req.Method, req.Params, known)
		if errMsg != "" {
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": errMsg}})
			continue
		}
		if req.Method == "session.sendAndWait" { // the turn's events precede the result naming its idle event
			res["completionEventId"] = sendTurn(send, req.Params)
		}
		send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}
}

// sendTurn emits the deterministic assistant message, usage and idle events; it returns the idle event's id.
func sendTurn(send func(any), params json.RawMessage) string {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	event := func(n int, typ string, ephemeral bool, data map[string]any) string {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		send(map[string]any{"jsonrpc": "2.0", "method": "session.event", "params": map[string]any{
			"sessionId": p.SessionID,
			"event": map[string]any{
				"id": id, "parentId": nil,
				"timestamp": "2026-01-01T00:00:00Z", "type": typ, "ephemeral": ephemeral, "data": data,
			},
		}})
		return id
	}
	event(1, "assistant.message", false, map[string]any{"messageId": "fake-msg", "content": " fake reply\n",
		"model": "gpt-5-5"})
	event(2, "assistant.usage", true, map[string]any{"model": "gpt-5-5", "inputTokens": 1000, "outputTokens": 50,
		"cacheReadTokens": 600, "cacheWriteTokens": 100})
	return event(3, "session.idle", true, map[string]any{})
}

// readBody reads one Content-Length framed message body; ok is false at EOF or a short read.
func readBody(r *bufio.Reader) (body []byte, ok bool) {
	n := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, false
		}
		if line = strings.TrimSpace(line); line == "" {
			break
		}
		if v, found := strings.CutPrefix(line, "Content-Length:"); found {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	body = make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, false
	}
	return body, true
}

// reply answers one method; a non-empty errMsg is a JSON-RPC error (an unknown switchTo model).
func reply(method string, params json.RawMessage, known []string) (res map[string]any, errMsg string) {
	var p struct {
		SessionID string `json:"sessionId"`
		ModelID   string `json:"modelId"`
	}
	_ = json.Unmarshal(params, &p)
	version := os.Getenv("FAKECLI_VERSION")
	res = map[string]any{}
	switch method {
	case "connect":
		res = map[string]any{"ok": true, "protocolVersion": 3, "version": version}
	case "status.get":
		res = map[string]any{"version": version, "protocolVersion": 3}
	case "session.create", "session.resume":
		id := p.SessionID
		if id == "" {
			id = "fake-session"
		}
		res = map[string]any{"sessionId": id}
	case "session.sendAndWait":
		res = map[string]any{"messageId": "fake-msg"}
	case "session.detach":
		res = map[string]any{"success": true}
	case "session.model.switchTo":
		if !slices.Contains(known, p.ModelID) {
			return nil, "unknown model " + p.ModelID
		}
	}
	return res, ""
}
