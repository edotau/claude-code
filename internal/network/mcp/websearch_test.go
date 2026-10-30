package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

// drive runs the server over a scripted set of request lines and returns the decoded responses.
func drive(t *testing.T, srv *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := srv.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("response not JSON: %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	// Concurrent dispatch answers out of order by design; ids are the contract, so tests index a
	// deterministic id-sorted view (a nil id — parse errors — sorts first).
	sort.Slice(resps, func(i, j int) bool {
		fi, _ := resps[i]["id"].(float64)
		fj, _ := resps[j]["id"].(float64)
		return fi < fj
	})
	return resps
}

func TestInitializeAndToolsList(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "unused", nil }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	)
	// notifications/initialized yields NO response, so we expect exactly 2 responses.
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (notification must not respond)", len(resps))
	}
	si, _ := resps[0]["result"].(map[string]any)
	if si["protocolVersion"] != defaultProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", si["protocolVersion"], defaultProtocolVersion)
	}
	list, _ := resps[1]["result"].(map[string]any)
	tools, _ := list["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", tools)
	}
	if tool, _ := tools[0].(map[string]any); tool["name"] != toolName {
		t.Errorf("tool name = %v, want %s", tool["name"], toolName)
	}
}

func TestToolsCallSuccess(t *testing.T) {
	called := ""
	srv := &Server{Search: func(_ context.Context, q string, _ int) (string, error) {
		called = q
		return "RESULT for " + q, nil
	}}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"golang","max_results":3}}}`,
	)
	if called != "golang" {
		t.Errorf("searcher got query %q, want golang", called)
	}
	res, _ := resps[0]["result"].(map[string]any)
	if res["isError"] == true {
		t.Errorf("unexpected isError: %v", res)
	}
	content, _ := res["content"].([]any)
	first, _ := content[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "RESULT for golang") {
		t.Errorf("content = %v, want the search result text", first["text"])
	}
}

func TestToolsCallEmptyQuery(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"  "}}}`,
	)
	res, _ := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("empty query should be a tool error, got %v", res)
	}
}

func TestToolsCallSearcherError(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "", errString("boom") }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`,
	)
	// A backend error is a tool error (isError=true result), not a JSON-RPC protocol error.
	if _, isRPCErr := resps[0]["error"]; isRPCErr {
		t.Errorf("backend error must be a tool result, not a JSON-RPC error: %v", resps[0])
	}
	res, _ := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("searcher error should surface as isError result, got %v", res)
	}
}

func TestUnknownTool(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if _, ok := resps[0]["error"]; !ok {
		t.Errorf("unknown tool should be a JSON-RPC error, got %v", resps[0])
	}
}

func TestUnknownMethodNotificationIgnored(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	// A notification (no id) for an unknown method must produce no response.
	resps := drive(t, srv, `{"jsonrpc":"2.0","method":"some/notify"}`)
	if len(resps) != 0 {
		t.Errorf("notification should yield no response, got %v", resps)
	}
}

func TestParseError(t *testing.T) {
	srv := &Server{}
	resps := drive(t, srv, `{not json`)
	if _, ok := resps[0]["error"]; !ok {
		t.Errorf("malformed line should yield a parse error, got %v", resps[0])
	}
}

func TestOneShotSuccess(t *testing.T) {
	var out strings.Builder
	got := ""
	code := OneShot(context.Background(), "golang generics", 3, func(_ context.Context, q string, n int) (string, error) {
		got = q
		if n != 3 {
			t.Errorf("max_results = %d, want 3", n)
		}
		return "the answer", nil
	}, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got != "golang generics" {
		t.Errorf("query = %q, want golang generics", got)
	}
	if strings.TrimSpace(out.String()) != "the answer" {
		t.Errorf("output = %q, want 'the answer'", out.String())
	}
}

func TestOneShotEmptyQuery(t *testing.T) {
	var out strings.Builder
	search := func(context.Context, string, int) (string, error) { return "x", nil }
	code := OneShot(context.Background(), "   ", 5, search, &out)
	if code != 2 {
		t.Errorf("empty query exit = %d, want 2", code)
	}
	if out.String() != "" {
		t.Errorf("empty query should print nothing to out, got %q", out.String())
	}
}

func TestOneShotDefaultsMaxResults(t *testing.T) {
	var out strings.Builder
	OneShot(context.Background(), "q", 0, func(_ context.Context, _ string, n int) (string, error) {
		if n != 5 {
			t.Errorf("default max_results = %d, want 5", n)
		}
		return "ok", nil
	}, &out)
}

func TestOneShotSearcherError(t *testing.T) {
	var out strings.Builder
	search := func(context.Context, string, int) (string, error) { return "", errString("boom") }
	code := OneShot(context.Background(), "q", 5, search, &out)
	if code != 1 {
		t.Errorf("searcher error exit = %d, want 1", code)
	}
}

// Cancellability is worthless unless the server's ctx actually reaches the backend, so assert the
// searcher observes the caller's cancellation rather than a fresh background context.
func TestServeThreadsContextToSearcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var seen error
	srv := &Server{Search: func(c context.Context, _ string, _ int) (string, error) {
		seen = c.Err()
		return "text", nil
	}}
	var out strings.Builder
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`
	if err := srv.Serve(ctx, strings.NewReader(line+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("searcher ctx.Err() = %v, want context.Canceled", seen)
	}
}

// The Bash/skill entry point carries the same guarantee as Serve.
func TestOneShotThreadsContextToSearcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var seen error
	var out strings.Builder
	if code := OneShot(ctx, "q", 5, func(c context.Context, _ string, _ int) (string, error) {
		seen = c.Err()
		return "ok", nil
	}, &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("searcher ctx.Err() = %v, want context.Canceled", seen)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestInitializeNegotiatesProtocolVersion pins the MCP negotiation contract: the server echoes the
// client's requested revision when it supports it, and otherwise falls back to its newest.
func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	for name, tc := range map[string]struct{ params, want string }{
		"echoes a supported revision":     {`{"protocolVersion":"2025-06-18"}`, "2025-06-18"},
		"echoes the newest supported":     {`{"protocolVersion":"2025-11-25"}`, "2025-11-25"},
		"echoes the oldest supported":     {`{"protocolVersion":"2024-11-05"}`, "2024-11-05"},
		"absent version falls back":       {`{}`, defaultProtocolVersion},
		"unknown future version falls to": {`{"protocolVersion":"2099-01-01"}`, defaultProtocolVersion},
		"garbage version falls back":      {`{"protocolVersion":"not-a-version"}`, defaultProtocolVersion},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &Server{Search: func(context.Context, string, int) (string, error) { return "unused", nil }}
			resps := drive(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+tc.params+`}`)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			res, _ := resps[0]["result"].(map[string]any)
			if got := res["protocolVersion"]; got != tc.want {
				t.Errorf("protocolVersion = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestDefaultProtocolVersionIsHonest guards against advertising a revision whose semantics this
// tools-only server does not implement: 2026-07-28 retired initialize and requires _meta routing.
func TestDefaultProtocolVersionIsHonest(t *testing.T) {
	if defaultProtocolVersion != "2025-11-25" {
		t.Errorf("defaultProtocolVersion = %s, want 2025-11-25 (newest revision whose semantics we implement)", defaultProtocolVersion)
	}
	if supportedProtocolVersions[defaultProtocolVersion] != true {
		t.Errorf("defaultProtocolVersion %s missing from supportedProtocolVersions", defaultProtocolVersion)
	}
	if supportedProtocolVersions["2026-07-28"] {
		t.Error("must not advertise 2026-07-28: stateless core, no initialize handshake, _meta per-request routing unimplemented")
	}
}
