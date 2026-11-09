package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestToolsListNotificationHasNoResponse(t *testing.T) {
	resps := drive(t, &Server{},
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want only the identified tools/list response: %v", len(resps), resps)
	}
	if resps[0]["id"] != float64(1) {
		t.Fatalf("response id = %v, want 1", resps[0]["id"])
	}
}

func TestToolsCallRejectsMalformedArguments(t *testing.T) {
	for name, params := range map[string]string{
		"search":  `{"name":"web_search","arguments":{"query":"golang","max_results":"bad"}}`,
		"extract": `{"name":"web_extract","arguments":{"urls":[123,"http://127.0.0.1/"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := &Server{Search: func(context.Context, string, int) (string, error) {
				t.Error("search backend called for malformed arguments")
				return "unexpected search", nil
			}}
			resps := drive(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			res, _ := resps[0]["result"].(map[string]any)
			if res["isError"] != true {
				t.Fatalf("malformed arguments must return a tool error, got %v", resps[0])
			}
			content, err := json.Marshal(res["content"])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), "invalid tool arguments:") {
				t.Errorf("tool error = %s, want argument decoding error", content)
			}
		})
	}
}

func closedTestOutput(t *testing.T) *os.File {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOneShotRejectsOutputWriteFailure(t *testing.T) {
	search := func(context.Context, string, int) (string, error) { return "the answer", nil }
	if code := OneShot(context.Background(), "golang", 3, search, io.Discard); code != 0 {
		t.Fatalf("successful search exit = %d, want 0", code)
	}
	if code := OneShot(context.Background(), "golang", 3, search, closedTestOutput(t)); code == 0 {
		t.Fatal("closed output writer must return a nonzero exit code")
	}
}

func TestExtractRejectsOutputWriteFailure(t *testing.T) {
	t.Setenv(keyName, "test-key")
	stubExtract(t, func(urls []string) extractResponse {
		return extractResponse{Results: []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		}{{urls[0], "BODY"}}}
	})
	urls := []string{"https://example.com/a"}
	out := captureStdout(t, func() {
		if code := Extract(context.Background(), urls); code != 0 {
			t.Errorf("successful extract exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "BODY") {
		t.Fatalf("extract output = %q, want fetched page", out)
	}
	orig := os.Stdout
	os.Stdout = closedTestOutput(t)
	t.Cleanup(func() { os.Stdout = orig })
	if code := Extract(context.Background(), urls); code == 0 {
		t.Fatal("closed stdout must return a nonzero exit code despite a successfully fetched page")
	}
}
