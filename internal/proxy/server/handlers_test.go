package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestAuthHintByAuthType(t *testing.T) {
	cases := map[string]struct {
		p    providers.Provider
		want string
	}{
		"oauth":   {providers.Provider{Name: "d", Auth: providers.Auth{Type: providers.AuthOAuth}}, "claude-code databricks login --profile d"},
		"preset":  {providers.Provider{Name: "d", Preset: providers.PresetDatabricks}, "claude-code databricks login --profile d"},
		"dbx pat": {providers.Provider{Name: "d", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: providers.AuthPAT}}, "--profile d --auth-type oauth"},
		"key":     {providers.Provider{Name: "k", Auth: providers.Auth{Type: providers.AuthKey, Env: "K_KEY"}}, "claude-code vault set K_KEY"},
		"command": {providers.Provider{Name: "c", Auth: providers.Auth{Type: providers.AuthKey, Command: "op read x"}}, "auth.command"},
		"other":   {providers.Provider{Name: "o", Auth: providers.Auth{Type: providers.AuthCLI}}, "claude-code configure provider --list o"},
	}
	for name, c := range cases {
		if got := authHint(&c.p); !strings.Contains(got, c.want) {
			t.Errorf("%s: authHint = %q, want %q", name, got, c.want)
		}
	}
}

func TestPeekBodyLeavesWholeBodyReadable(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("0123456789"))}
	if got := peekBody(resp, 4); string(got) != "0123" {
		t.Fatalf("peek = %q, want 0123", got)
	}
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "0123456789" {
		t.Errorf("body after peek = %q, want the whole body", rest)
	}
}

func TestHeadTailBufferBoundsMemoryKeepsBothEnds(t *testing.T) {
	const start = `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":1}}}`
	const end = `{"type":"message_delta","usage":{"output_tokens":9}}`
	var b headTailBuffer
	b.Write([]byte(start))
	for range 40 {
		b.Write(bytes.Repeat([]byte("x"), 64<<10))
	}
	b.Write([]byte(end))
	if got := len(b.Bytes()); got > 2*tailCap {
		t.Errorf("retained %d bytes, want <= %d", got, 2*tailCap)
	}
	if u := scrapeAnthropicUsage(b.Bytes()); u.input != 7 || u.output != 9 {
		t.Errorf("usage = %+v, want input 7 output 9 (max, cumulative) from head and tail", u)
	}
}

func TestScrapeLastUsageSubtractsCachedForBothDialects(t *testing.T) {
	chat := scrapeChatUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":40}}}`))
	if chat.input != 60 || chat.output != 5 || chat.cacheRead != 40 {
		t.Errorf("chat = %+v, want 60/5/40", chat)
	}
	resp := scrapeUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":200}}}`))
	if resp.input != 100 || resp.cacheRead != 0 {
		t.Errorf("responses = %+v, cached > input must not be subtracted", resp)
	}
}

func TestExhaustedSharesSendErrorReply(t *testing.T) {
	p := &providers.Provider{Name: "up1", Preset: providers.PresetDatabricks}
	ladder := []providers.Target{{Provider: p, Model: "m"}}
	cases := map[string]struct {
		err    error
		status int
		body   string
	}{
		"auth":  {&fallbackError{error: errors.New("denied"), auth: true}, http.StatusUnauthorized, "proxy: up1: authentication failed; run `claude-code databricks login --profile up1`"},
		"other": {errors.New("boom"), http.StatusBadGateway, "proxy: up1: boom"},
	}
	for name, c := range cases {
		rec := httptest.NewRecorder()
		name2, model, n := exhausted(rec, ladder, c.err)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.body) || name2 != "up1" || model != "m" || n != 1 {
			t.Errorf("%s: status %d body %q tuple %s/%s/%d", name, rec.Code, rec.Body.String(), name2, model, n)
		}
	}
}
