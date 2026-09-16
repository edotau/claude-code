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
	if err := writeFile(filepath.Join(dir, "providers.json"), body); err != nil {
		t.Fatal(err)
	}
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
	if out.String() != "ok" || res.Answer != "ok" || res.Model != "claude-haiku-4-5-20251001" || res.Usage != (Usage{7, 2}) {
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
	if out.String() != "hello" || res.Model != "gpt-x-2026" || res.Provider != "toai" || res.Usage != (Usage{5, 2}) {
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

func TestAPIRunnerPassthroughRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, err := apiRunner{}.Run(context.Background(), Request{Prompt: "x", Provider: "subscription"}, io.Discard)
	if e := Classify("api", err, false); e == nil || e.Kind != KindFatal || !strings.Contains(err.Error(), "passthrough") {
		t.Errorf("err = %v", err)
	}
}
