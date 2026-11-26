package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	if CanOneShotHTTP(cliProv, "m1", RunOptions{Prompt: "hi"}) {
		t.Fatal("CLI-auth provider must not use one-shot HTTP")
	}
	if CanOneShotHTTP(httpProv, "m1", RunOptions{Prompt: "hi", Tools: true}) {
		t.Fatal("tools=true must not use one-shot HTTP")
	}
}

func TestOneShotHTTPDialects(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		sse      string
		wantAns  string
		wantOut  int
	}{
		{
			name:     "openai-chat",
			protocol: providers.ProtocolOpenAI,
			sse:      "data: {\"model\":\"m-chat\",\"error\":null,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n",
			wantAns:  "ok", wantOut: 2,
		},
		{
			name:     "openai-responses",
			protocol: providers.ProtocolResponses,
			sse:      "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m-resp\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n",
			wantAns:  "done", wantOut: 3,
		},
		{
			name:     "anthropic",
			protocol: providers.ProtocolAnthropic,
			sse:      "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m-anth\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"yes\"}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\ndata: {\"type\":\"message_stop\"}\n\n",
			wantAns:  "yes", wantOut: 6,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.sse)
			}))
			defer srv.Close()
			p := &providers.Provider{Name: "testp", Protocol: tc.protocol, BaseURL: srv.URL,
				Routes: map[string]string{tc.protocol: srv.URL}, Auth: providers.Auth{Type: providers.AuthNone}}
			var out bytes.Buffer
			res, err := OneShotHTTP(context.Background(), providers.Target{Provider: p, Model: "m1"},
				RunOptions{Prompt: "ping", Effort: "low"}, wire.NewTextSink(&out))
			if err != nil {
				t.Fatalf("OneShotHTTP(%s) err = %v", tc.name, err)
			}
			if res.Answer != tc.wantAns || out.String() != tc.wantAns || res.Usage.OutputTokens != tc.wantOut {
				t.Fatalf("got answer=%q stream=%q out=%d, want %q / %d", res.Answer, out.String(), res.Usage.OutputTokens, tc.wantAns, tc.wantOut)
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
