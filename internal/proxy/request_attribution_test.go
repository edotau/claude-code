package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func okMsg(w http.ResponseWriter, r *http.Request, _ int) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
		return
	}
	io.WriteString(w, `{"type":"message","content":[]}`)
}

func headerHolds(h http.Header, needle string) bool {
	for _, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, needle) {
				return true
			}
		}
	}
	return false
}

// Every upstream hop carries our UA with the client's agent, and never the client's agent/session/UA headers.
func TestUpstreamUserAgentOnEveryRoute(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("PT_KEY", "prov-cred")
	t.Setenv("AN_KEY", "an-cred")
	t.Setenv("HARNESS_REQUEST_TAGS", "")
	srv := setup(t, `{"default":"pt","providers":{`+
		`"openai":{"pt":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"PT_KEY"},`+
		`"routes":{"responses":"`+up.URL+`/resp"},"models":{"opus":"m"}}},`+
		`"anthropic":{"an":`+anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)

	cases := []struct{ name, url, body string }{
		{"anthropic", "/p/an/v1/messages", `{"model":"m","max_tokens":5}`},
		{"chat", "/p/pt/v1/messages", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`},
		{"passthrough-chat", "/p/pt/openai/chat/completions", `{}`},
		{"passthrough-responses", "/p/pt/responses/responses", `{}`},
	}
	for _, agent := range []string{"codex", ""} {
		for _, tc := range cases {
			t.Run(tc.name+"/"+agent, func(t *testing.T) {
				before := len(up.got())
				hdr := map[string]string{"User-Agent": "client-ua/9", SessionHeader: "sess-leak"}
				if agent != "" {
					hdr[AgentHeader] = agent
				}
				resp := ptDo(t, http.MethodPost, srv.URL+tc.url, tc.body, hdr)
				if resp.StatusCode != 200 {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("status %d: %s", resp.StatusCode, b)
				}
				reqs := up.got()
				if len(reqs) != before+1 {
					t.Fatalf("upstream hits %d, want %d", len(reqs), before+1)
				}
				h := reqs[before].hdr
				ua := h.Values("User-Agent")
				if len(ua) != 1 || !strings.HasPrefix(ua[0], "claude-code/") {
					t.Fatalf("User-Agent = %q, want one claude-code/<version> value", ua)
				}
				if agent != "" && !strings.HasSuffix(ua[0], " "+agent) {
					t.Errorf("User-Agent %q lacks agent %q", ua[0], agent)
				}
				if agent == "" && strings.Contains(strings.TrimSpace(ua[0]), " ") {
					t.Errorf("User-Agent %q has an agent part without the header", ua[0])
				}
				for _, k := range []string{AgentHeader, SessionHeader, ClientHeader} {
					if v := h.Get(k); v != "" {
						t.Errorf("%s reached upstream: %q", k, v)
					}
				}
				if headerHolds(h, "client-ua/9") || headerHolds(h, "sess-leak") || headerHolds(h, secret) {
					t.Errorf("client-side value leaked upstream: %v", h)
				}
			})
		}
	}
}

// Request tags reach only an ai-gateway row, and invalid HARNESS_REQUEST_TAGS turns the feature off.
func TestRequestTagsOnlyOnAIGatewayRow(t *testing.T) {
	gw := func(up string) string {
		return `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up +
			`","auth":{"type":"key","env":"DBX_KEY"},"options":{"wire":"ai-gateway"}}`
	}
	const tagMark = "tagmark-7f3a"
	valid := `{"team":"` + tagMark + `"}`
	cases := []struct {
		name, tags string
		gateway    bool
		want       bool
	}{
		{"valid-gateway", valid, true, true},
		{"valid-anthropic-row", valid, false, false},
		{"multiline", "{\n\"team\":\"" + tagMark + "\"}", true, false},
		{"control-char", "{\"team\":\"" + tagMark + "\x01\"}", true, false},
		{"not-object", `["` + tagMark + `"]`, true, false},
		{"too-big", `{"team":"` + tagMark + strings.Repeat("a", 4097) + `"}`, true, false},
		{"unset", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newRawUp(t, okMsg)
			t.Setenv("DBX_KEY", "dbx-token")
			t.Setenv("AN_KEY", "an-cred")
			t.Setenv("HARNESS_REQUEST_TAGS", tc.tags)
			row, path, def := anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`), "/p/an/v1/messages", "an"
			prov := `"anthropic":{"an":` + row + `}`
			if tc.gateway {
				prov, path = `"databricks":{"dbx":`+gw(up.URL)+`}`, "/p/dbx/v1/messages"
				def = "dbx"
			}
			srv := setup(t, `{"default":"`+def+`","providers":{`+prov+`}}`)
			resp := ptDo(t, http.MethodPost, srv.URL+path, `{"model":"system.ai.claude-opus-5-5","max_tokens":5}`, nil)
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d: %s", resp.StatusCode, b)
			}
			reqs := up.got()
			if len(reqs) != 1 {
				t.Fatalf("upstream hits = %d", len(reqs))
			}
			if got := headerHolds(reqs[0].hdr, tagMark); got != tc.want {
				t.Errorf("tags upstream = %v, want %v; headers %v", got, tc.want, reqs[0].hdr)
			}
		})
	}
}

// HARNESS_HEADERS reaches upstream; auth and attribution headers win; bad lines drop with one warning.
func TestHarnessHeadersForwarded(t *testing.T) {
	up := newRawUp(t, okMsg)
	t.Setenv("AN_KEY", "an-cred")
	t.Setenv("HARNESS_REQUEST_TAGS", "")
	t.Setenv("HARNESS_HEADERS", "X-Team: alpha\nx-team: beta\nAuthorization: Bearer evil\nUser-Agent: evil/1\n"+
		"Bad Name: x\nX-Inj: a\rX-Evil: b\n:novalue\nX-Ok:  spaced ")
	srv := setup(t, `{"default":"an","providers":{"anthropic":{"an":`+anthropicProvider(up.URL, "AN_KEY", `{"opus":"m"}`)+`}}}`)
	resp := ptDo(t, http.MethodPost, srv.URL+"/p/an/v1/messages", `{"model":"m","max_tokens":5}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := up.got()[0].hdr
	if got := h.Values("X-Team"); len(got) != 1 || got[0] != "beta" {
		t.Errorf("X-Team = %q, want single beta (case-insensitive replace)", got)
	}
	if h.Get("X-Ok") != "spaced" {
		t.Errorf("X-Ok = %q", h.Get("X-Ok"))
	}
	if headerHolds(h, "evil") || h.Get("X-Evil") != "" || h.Get("X-Inj") != "" {
		t.Errorf("protected or injected header reached upstream: %v", h)
	}
	if !strings.HasPrefix(h.Get("User-Agent"), "claude-code/") {
		t.Errorf("User-Agent overridden: %q", h.Get("User-Agent"))
	}
}

func TestParseHeadersWarnsOnce(t *testing.T) {
	var log strings.Builder
	got := ParseHeaders("A: 1\nbad name: x\nAuthorization: y\nB: v\x00\n", &log)
	if len(got) != 1 || got["A"] != "1" {
		t.Errorf("got %v", got)
	}
	if strings.Count(log.String(), "\n") != 1 {
		t.Errorf("want one warning, got %q", log.String())
	}
}
