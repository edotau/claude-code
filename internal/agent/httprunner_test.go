package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func TestCanOneShotHTTP(t *testing.T) {
	httpProv := &providers.Provider{Name: "p1", Protocol: providers.ProtocolOpenAI, BaseURL: "https://api.example.com/v1", Auth: providers.Auth{Type: providers.AuthNone}}
	cliProv := &providers.Provider{Name: "p2", Protocol: providers.ProtocolOpenAI, BaseURL: "https://api.example.com/v1", Auth: providers.Auth{Type: providers.AuthCLI}}
	if !CanOneShotHTTP(httpProv, "m1", RunOptions{Prompt: "hi"}) {
		t.Fatal("expected HTTP provider with tools=false to allow one-shot HTTP")
	}
	if !CanOneShotHTTP(httpProv, "m1", RunOptions{Prompt: "hi", OutputSchema: `{"type":"object"}`, Attach: []string{"a.txt"}}) {
		t.Fatal("expected OutputSchema and Attach with tools=false to allow one-shot HTTP")
	}
	if CanOneShotHTTP(cliProv, "m1", RunOptions{Prompt: "hi"}) {
		t.Fatal("CLI-auth provider must not use one-shot HTTP")
	}
	if CanOneShotHTTP(httpProv, "m1", RunOptions{Prompt: "hi", Tools: true}) {
		t.Fatal("tools=true must not use one-shot HTTP")
	}
	binFile := filepath.Join(t.TempDir(), "bad.bin")
	if err := os.WriteFile(binFile, []byte{0x00, 0x01}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OneShotHTTP(context.Background(), providers.Target{Provider: httpProv, Model: "m1"},
		RunOptions{Prompt: "hi", Attach: []string{binFile}}, wire.Discard); !errors.Is(err, ErrBadAttach) {
		t.Fatalf("binary attach err = %v, want ErrBadAttach", err)
	}
}

func TestOneShotHTTPDialects(t *testing.T) {
	txtFile := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(txtFile, []byte("file-body-here"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, protocol, sse, wantAns string
		wantOut                      int
	}{
		{"openai-chat", providers.ProtocolOpenAI, "data: {\"model\":\"m-chat\",\"error\":null,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", "ok", 2},
		{"openai-responses", providers.ProtocolResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m-resp\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n", "done", 3},
		{"anthropic", providers.ProtocolAnthropic, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m-anth\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"yes\"}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "yes", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotBody = string(raw)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.sse)
			}))
			defer srv.Close()
			p := &providers.Provider{Name: "testp", Protocol: tc.protocol, BaseURL: srv.URL,
				Routes: map[string]string{tc.protocol: srv.URL}, Auth: providers.Auth{Type: providers.AuthNone}}
			var out bytes.Buffer
			res, err := OneShotHTTP(context.Background(), providers.Target{Provider: p, Model: "m1"},
				RunOptions{Prompt: "ping", Effort: "low", Attach: []string{txtFile}, OutputSchema: `{"type":"object"}`}, wire.NewTextSink(&out))
			if err != nil {
				t.Fatalf("OneShotHTTP(%s) err = %v", tc.name, err)
			}
			if res.Answer != tc.wantAns || out.String() != tc.wantAns || res.Usage.OutputTokens != tc.wantOut {
				t.Fatalf("got answer=%q stream=%q out=%d, want %q / %d", res.Answer, out.String(), res.Usage.OutputTokens, tc.wantAns, tc.wantOut)
			}
			if !strings.Contains(gotBody, "file-body-here") || !strings.Contains(gotBody, `\"type\":\"object\"`) {
				t.Fatalf("body(%s) missing attach or schema: %s", tc.name, gotBody)
			}
		})
	}
}

func TestParseOneShotChatEdgeCases(t *testing.T) {
	if _, _, err := parseOneShotStream(providers.ProtocolOpenAI, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("empty stream must fail")
	}
	const size = 5 << 20
	big := strings.Repeat("b", size)
	stream := ": comment\n" +
		`data: {"model":"m","choices":[{"delta":{"content":"` + big + `"}}]}` + "\n\n" +
		"data: {not json\n" +
		`data: {"choices":[{"delta":{"content":"end"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n" +
		"data: [DONE]\n"
	var out strings.Builder
	usage, model, err := parseOneShotStream(providers.ProtocolOpenAI, strings.NewReader(stream), &out)
	if err != nil || out.String() != big+"end" || model != "m" || usage != (wire.Usage{InputTokens: 2, OutputTokens: 1}) {
		t.Fatalf("err=%v len=%d model=%q usage=%+v", err, out.Len(), model, usage)
	}
}

func TestHTTPRunnerGeminiChatParts(t *testing.T) {
	for _, content := range []string{
		`"hello"`,
		`[{"type":"text","text":"hel"},{"type":"text","text":"lo"}]`,
		`[{"type":"reasoning","text":"private"},{"type":"text","text":"hello"}]`,
	} {
		t.Run(content, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Errorf("path = %q, want existing chat endpoint", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"model\":\"gemini-served\",\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", content)
			}))
			defer srv.Close()
			p := &providers.Provider{Name: "test", Protocol: providers.ProtocolOpenAI, BaseURL: srv.URL,
				Auth: providers.Auth{Type: providers.AuthNone}}
			req := RunOptions{Prompt: "ping", HTTPDirect: true}
			if !CanOneShotHTTP(p, "gemini-requested", req) {
				t.Fatal("this request must already be HTTP-eligible")
			}
			var stream bytes.Buffer
			res, err := OneShotHTTP(t.Context(), providers.Target{Provider: p, Model: "gemini-requested"}, req, wire.NewTextSink(&stream))
			want := wire.Result{Provider: "test", Model: "gemini-served", Reported: "gemini-served", Answer: "hello",
				Usage: wire.Usage{InputTokens: 5, OutputTokens: 2}}
			if err != nil || !reflect.DeepEqual(res, want) || stream.String() != "hello" {
				t.Fatalf("result=%+v stream=%q err=%v, want %+v", res, stream.String(), err, want)
			}
		})
	}
}
