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
	  "tanth":{"kind":"anthropic","base_url":%q,"auth":{"type":"x-api-key","env":"TEST_ANTH_KEY"},"models":{"opus":"claude-opus-5","haiku":"claude-haiku-4-5"}},
	  "toai":{"kind":"openai","base_url":%q,"auth":{"type":"bearer","env":"TEST_OAI_KEY"},"models":{"opus":"gpt-x"}}}}`, srv.URL, srv.URL+"/v1")
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

func TestAPIRunnerAnthropic(t *testing.T) {
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
	res, err := apiRunner{}.Run(context.Background(), Request{Prompt: "say ok", Provider: "tanth", Model: "haiku", Effort: "low"}, &out)
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
	_, err = apiRunner{}.Run(context.Background(), Request{Prompt: "x", Provider: "tanth"}, io.Discard)
	if e := Classify("api", err, false); e.Kind != KindRateLimit || e.RetryAfter != 3*time.Second || e.Status != 429 {
		t.Errorf("429 classified as %+v", e)
	}
}

func TestAPIRunnerOpenAI(t *testing.T) {
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
	res, err := apiRunner{}.Run(context.Background(), Request{Prompt: "hi", Provider: "toai", Effort: "high"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" || res.Model != "gpt-x-2026" || res.Provider != "toai" || res.Usage != (Usage{InputTokens: 5, OutputTokens: 2}) {
		t.Errorf("res = %+v stream %q", res, out.String())
	}
	if gotAuth != "Bearer oai-key" || !strings.Contains(gotBody, `"reasoning_effort":"high"`) || !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("auth=%q body=%s", gotAuth, gotBody)
	}

	_, err = apiRunner{}.Run(context.Background(), Request{Prompt: "boom", Provider: "toai"}, io.Discard)
	var he *httpError
	if !errors.As(err, &he) || Classify("api", err, false).Kind != KindTransient {
		t.Errorf("502 = %v", err)
	}
}

// TestAPIRunnerPassthroughDelegatesToClaude proves api no longer refuses outright on a passthrough
// provider: it must hand the same request to claudeRunner (marked via res.Fallback), not fail instantly
// with the old "needs a key" message. The delegated claude call itself fails in this sandbox (no
// subscription login), but that failure must come from claude, not from api's own passthrough guard.
func TestAPIRunnerPassthroughDelegatesToClaude(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	res, err := apiRunner{}.Run(context.Background(), Request{Prompt: "x", Provider: "subscription"}, io.Discard)
	if res.Fallback != "api" {
		t.Errorf("res.Fallback = %q, want %q (delegation marker)", res.Fallback, "api")
	}
	if err != nil && strings.Contains(err.Error(), "needs a key") {
		t.Errorf("err = %v, want delegation to claude instead of the old instant refusal", err)
	}
}

// TestAPIRunnerPassthroughDelegationSuccessReportsClaudeAgent proves that on a successful delegated
// run (a fake claude binary standing in for the real one), the envelope names "claude" as the agent
// that actually ran, with "api" recorded as the fallback that got it there — never the reverse.
func TestAPIRunnerPassthroughDelegationSuccessReportsClaudeAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"session_id\":\"s1\"}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := apiRunner{}.Run(context.Background(), Request{Prompt: "x", Provider: "subscription"}, io.Discard)
	if err != nil {
		t.Fatalf("delegated run with a working fake claude binary must succeed: %v", err)
	}
	if res.Agent != "claude" {
		t.Errorf("res.Agent = %q, want %q (the runner that actually ran)", res.Agent, "claude")
	}
	if res.Fallback != "api" {
		t.Errorf("res.Fallback = %q, want %q (the runner that dispatched the delegation)", res.Fallback, "api")
	}
}

// TestAPIRunnerPassthroughDelegationErrorCarriesClaudeMessage proves that when the delegated claude
// run itself fails (no binary/login in this sandbox), api still marks the attempt (res.Fallback,
// res.FallbackReason) and surfaces claude's own error verbatim, not a masked or generic one — and that
// an early claude failure (before claude ever sets res.Provider/res.Model) still leaves api's own
// resolved provider/model on the result, never blank.
func TestAPIRunnerPassthroughDelegationErrorCarriesClaudeMessage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	req := Request{Prompt: "x", Provider: "subscription"}

	_, directErr := claudeRunner{}.Run(context.Background(), req, io.Discard)
	if directErr == nil {
		t.Fatal("claudeRunner.Run must fail in this sandbox (no claude binary/login) for this test to prove anything")
	}

	res, err := apiRunner{}.Run(context.Background(), req, io.Discard)
	if res.Fallback != "api" || res.FallbackReason == "" {
		t.Errorf("res = %+v, want Fallback=%q and a non-empty FallbackReason even on a failed delegation", res, "api")
	}
	if err == nil || err.Error() != directErr.Error() {
		t.Errorf("err = %v, want claude's own error verbatim: %v", err, directErr)
	}
	if res.Provider != "subscription" || res.Model == "" {
		t.Errorf("res = %+v, want api's resolved provider/model kept even though claude failed before setting its own", res)
	}
}

// TestAPIRunnerPassthroughBadAttachStillDelegates proves attachInline moved to AFTER the passthrough
// branch: a bad Attach path must not make api fail before delegating — the error, when one occurs, must
// come from claudeRunner's own attachInline call, not from api short-circuiting on the bad path itself.
func TestAPIRunnerPassthroughBadAttachStillDelegates(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	req := Request{Prompt: "x", Provider: "subscription", Attach: []string{filepath.Join(t.TempDir(), "does-not-exist")}}

	_, directErr := claudeRunner{}.Run(context.Background(), req, io.Discard)
	if directErr == nil {
		t.Fatal("claudeRunner.Run must fail on a bad Attach path for this test to prove anything")
	}

	res, err := apiRunner{}.Run(context.Background(), req, io.Discard)
	if res.Fallback != "api" {
		t.Errorf("res.Fallback = %q, want %q: delegation must still happen despite the bad Attach path", res.Fallback, "api")
	}
	if err == nil || err.Error() != directErr.Error() {
		t.Errorf("err = %v, want claude's own attachInline error verbatim: %v", err, directErr)
	}
}

// Available is request-agnostic: a missing claude binary must not block an explicit keyed --provider.
func TestAPIRunnerAvailableIgnoresClaudeBinary(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // isolate from any claude install under the real $HOME/.local/share/claude
	if (claudeRunner{}).Available() == nil {
		t.Fatal("claudeRunner.Available() must fail in this sandbox (no claude binary) for this test to prove anything")
	}
	if err := (apiRunner{}).Available(); err != nil {
		t.Errorf("apiRunner{}.Available() = %v, want nil when the registry loads", err)
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
