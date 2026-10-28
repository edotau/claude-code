package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

// registry points two test providers (one per dialect) at srv.
func registry(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TEST_ANTH_KEY", "anth-key")
	t.Setenv("TEST_OAI_KEY", "oai-key")
	body := fmt.Sprintf(`{"providers":{
	  "anthropic":{"tanth":{"kind":"anthropic","base_url":%q,"auth":{"type":"key","header":"x-api-key","env":"TEST_ANTH_KEY"},"models":{"opus":"claude-opus-5","haiku":"claude-haiku-4-5"}}},
	  "openai":{"toai":{"kind":"openai","base_url":%q,"auth":{"type":"key","env":"TEST_OAI_KEY"},"models":{"opus":"gpt-x"}}}}}`, srv.URL, srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"o"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"k"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

func TestClaudeNoToolsAnthropic(t *testing.T) {
	var gotKey, gotAuth, gotBody string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotKey, gotAuth, gotBody = r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), string(b)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if status != http.StatusOK {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(status)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer srv.Close()
	registry(t, srv)
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")

	var out strings.Builder
	res, err := claudeRunner{}.Run(context.Background(), Request{Prompt: "say ok", Provider: "tanth", Model: "haiku", Effort: "low", NoTools: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ok" || res.Answer != "ok" || res.Model != "claude-haiku-4-5-20251001" || res.Usage != (Usage{InputTokens: 7, OutputTokens: 2}) {
		t.Errorf("res = %+v, stream %q", res, out.String())
	}
	if gotKey != "anth-key" || gotAuth != "" {
		t.Errorf("auth headers: x-api-key=%q authorization=%q", gotKey, gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"claude-haiku-4-5"`) || !strings.Contains(gotBody, `"effort":"low"`) {
		t.Errorf("body = %s", gotBody)
	}

	status = http.StatusTooManyRequests
	_, err = claudeRunner{}.Run(context.Background(), Request{Prompt: "x", Provider: "tanth", NoTools: true}, io.Discard)
	if e := Classify("claude", err, false); e.Kind != KindRateLimit || e.RetryAfter != 3*time.Second || e.Status != 429 {
		t.Errorf("429 classified as %+v", e)
	}
}

func TestClaudeNoToolsOpenAI(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(b)
		if r.URL.Path == "/v1/chat/completions" && strings.Contains(gotBody, "boom") {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"model":"gpt-x-2026","choices":[{"delta":{"role":"assistant","content":"he"}}]}`,
			`{"model":"gpt-x-2026","choices":[{"delta":{"content":"llo"}}]}`,
			`{"model":"gpt-x-2026","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	defer srv.Close()
	registry(t, srv)

	var out strings.Builder
	res, err := claudeRunner{}.Run(context.Background(), Request{Prompt: "hi", Provider: "toai", Effort: "high", NoTools: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" || res.Model != "gpt-x-2026" || res.Provider != "toai" || res.Usage != (Usage{InputTokens: 5, OutputTokens: 2}) {
		t.Errorf("res = %+v stream %q", res, out.String())
	}
	if gotAuth != "Bearer oai-key" || !strings.Contains(gotBody, `"reasoning_effort":"high"`) || !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("auth=%q body=%s", gotAuth, gotBody)
	}

	_, err = claudeRunner{}.Run(context.Background(), Request{Prompt: "boom", Provider: "toai", NoTools: true}, io.Discard)
	var he *httpError
	if !errors.As(err, &he) || Classify("claude", err, false).Kind != KindTransient {
		t.Errorf("502 = %v", err)
	}
}

// fakeFailingClaude points CLAUDE_CONFIG_DIR at a bin/ holding a claude that exits 1, so no real claude is spawned.
func fakeFailingClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "claude"), []byte("#!/bin/sh\necho fake claude failure >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeNoToolsCLIAuthRunsTheCLI proves a cli-auth profile (no key to call the provider with) falls through to the
// claude CLI itself, which answers tool-less anyway: the envelope names no fallback and carries claude's own error.
func TestClaudeNoToolsCLIAuthRunsTheCLI(t *testing.T) {
	fakeFailingClaude(t)
	req := Request{Prompt: "x", Provider: "max"}

	_, directErr := claudeRunner{}.Run(context.Background(), req, io.Discard)
	if directErr == nil {
		t.Fatal("claudeRunner.Run must fail in this sandbox (fake failing claude) for this test to prove anything")
	}
	req.NoTools = true
	res, err := claudeRunner{}.Run(context.Background(), req, io.Discard)
	if res.Fallback != "" {
		t.Errorf("res.Fallback = %q, want none: there is no second agent any more", res.Fallback)
	}
	if err == nil || err.Error() != directErr.Error() {
		t.Errorf("err = %v, want the claude CLI's own error verbatim: %v", err, directErr)
	}
}

// TestClaudeNoToolsAndToolsExclusive: asking for both is refused before anything runs.
func TestClaudeNoToolsAndToolsExclusive(t *testing.T) {
	_, err := claudeRunner{}.Run(context.Background(), Request{Prompt: "x", Tools: true, NoTools: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("err = %v, want the --no-tools/--tools exclusivity error", err)
	}
}

// TestNoToolsDroppedLoudlyByOtherAgents: only claude honors --no-tools; every other agent names the drop.
func TestNoToolsDroppedLoudlyByOtherAgents(t *testing.T) {
	_, dropped := applyCaps(Caps{Model: true}, Request{NoTools: true})
	if len(dropped) != 1 || dropped[0] != "--no-tools" {
		t.Errorf("dropped = %v, want [--no-tools]", dropped)
	}
	if _, dropped := applyCaps((claudeRunner{}).Caps(), Request{NoTools: true}); len(dropped) != 0 {
		t.Errorf("claude dropped %v, want nothing", dropped)
	}
}

func TestParseChatStream(t *testing.T) {
	const first = `data: {"model":"served-model","choices":[{"delta":{"content":"one"}},{"delta":{"content":""}},{"delta":{"content":"two"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}` + "\n\n"
	cases := []struct {
		name, suffix, output, err string
		writer                    io.Writer
	}{
		{name: "done ignores trailing data", suffix: "data: [DONE]\ndata: invalid\n", output: "onetwo"},
		{name: "EOF", output: "onetwo"},
		{name: "malformed chunk", suffix: "data: invalid\n", output: "onetwo", err: "decode chat chunk:"},
		{name: "upstream error", suffix: "data: {\"error\":{\"message\":\"failed\"}}\n", output: "onetwo", err: `upstream error mid-stream: {"message":"failed"}`},
		{name: "scanner error", suffix: strings.Repeat("x", 4<<20), output: "onetwo", err: "bufio.Scanner: token too long"},
		{name: "writer error", writer: failingStreamWriter{}, err: io.ErrClosedPipe.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			w := tc.writer
			if w == nil {
				w = &out
			}
			stream := ": comment\nevent: message\ndata:\ndata:  \n" + first + tc.suffix
			usage, model, err := parseChatStream(strings.NewReader(stream), w)
			if (err == nil) != (tc.err == "") || err != nil && !strings.HasPrefix(err.Error(), tc.err) {
				t.Fatalf("err = %v, want prefix %q", err, tc.err)
			}
			if usage != (Usage{InputTokens: 7, OutputTokens: 3}) || model != "served-model" || out.String() != tc.output {
				t.Fatalf("usage = %+v, model = %q, output = %q", usage, model, out.String())
			}
		})
	}
}
