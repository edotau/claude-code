package proxy

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/usageledger"
)

func TestMeteredExcludesOnlyCLIPassthrough(t *testing.T) {
	for _, tc := range []struct {
		a    providers.Auth
		want bool
	}{
		{providers.Auth{Type: providers.AuthOAuth}, true},
		{providers.Auth{Type: providers.AuthM2M}, true},
		{providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey}, true},
		{providers.Auth{Type: providers.AuthCLI}, false},
	} {
		if got := metered(&providers.Provider{Auth: tc.a}); got != tc.want {
			t.Errorf("metered(%+v) = %v, want %v", tc.a, got, tc.want)
		}
	}
}

// A non-api-key (pat) Databricks-style row's relayed request lands in the ledger with profile, provider and session.
func TestRelayedRequestWritesLedgerForNonKeyAuth(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2}}`)
	})
	t.Setenv("DATABRICKS_PROD_PAT", "tok")
	row := fmt.Sprintf(`{"provider":"databricks","kind":"anthropic","base_url":%q,"auth":{"type":"pat"},`+
		`"models":{"opus":"m-opus","haiku":"m-haiku"}}`, up.URL)
	srv := setup(t, `{"default":"prod","providers":{"databricks":{"prod":`+row+`}}}`)
	resp := post(t, srv.URL+"/p/prod/v1/messages", `{"model":"m-haiku"}`,
		map[string]string{SessionHeader: "sess-1", AgentHeader: "code-worker"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	var got []usageledger.Entry
	for i := 0; i < 100 && len(got) == 0; i++ { // the append lands just after the relay returns
		if got, _ = usageledger.Read(); len(got) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(got) != 1 {
		t.Fatalf("ledger = %+v, want 1 entry", got)
	}
	e := got[0]
	if e.Profile != "prod" || e.Provider != "databricks" || e.Session != "sess-1" || e.Agent != "code-worker" || e.Input != 7 || e.Output != 3 || e.CacheRead != 2 {
		t.Errorf("entry = %+v", e)
	}
	if in, out, cr := usageledger.TotalsBySession("sess-1"); in != 7 || out != 3 || cr != 2 {
		t.Errorf("totals = %d %d %d", in, out, cr)
	}
}

func TestDeadKeyCanonicalizesOneMarker(t *testing.T) {
	p := &providers.Provider{Name: "p"}
	a := deadKey(providers.Target{Provider: p, Model: "claude-opus-4-7[1m]"})
	b := deadKey(providers.Target{Provider: p, Model: "claude-opus-4-7"})
	if a != b {
		t.Errorf("deadKey marked %q != unmarked %q", a, b)
	}
}

func TestScrapeUsage(t *testing.T) {
	if u := scrapeAnthropicUsage([]byte(`data: {"message":{"usage":{"input_tokens":5}}}` + "\n" + `{"usage":{"output_tokens":9}}`)); u.input != 5 || u.output != 9 {
		t.Errorf("anthropic = %+v", u)
	}
	if u := scrapeChatUsage([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`)); u.input != 6 || u.output != 2 || u.cacheRead != 4 {
		t.Errorf("chat = %+v", u)
	}
}

// Session falls back to X-Claude-Code-Session-Id; absent headers leave empty fields, not a failure.
func TestLedgerAttributionHeaders(t *testing.T) {
	for _, tc := range []struct {
		name           string
		hdr            map[string]string
		session, agent string
	}{
		{"both", map[string]string{SessionHeader: "s1", AgentHeader: "claude"}, "s1", "claude"},
		{"session-id fallback", map[string]string{"X-Claude-Code-Session-Id": "s2"}, "s2", ""},
		{"session wins over session-id", map[string]string{SessionHeader: "s1", "X-Claude-Code-Session-Id": "s2"}, "s1", ""},
		{"neither", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			})
			t.Setenv("DATABRICKS_PROD_PAT", "tok")
			row := fmt.Sprintf(`{"provider":"databricks","kind":"anthropic","base_url":%q,"auth":{"type":"pat"},`+
				`"models":{"opus":"m-opus","haiku":"m-haiku"}}`, up.URL)
			srv := setup(t, `{"default":"prod","providers":{"databricks":{"prod":`+row+`}}}`)
			resp := post(t, srv.URL+"/p/prod/v1/messages", `{"model":"m-haiku"}`, tc.hdr)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
			}
			var got []usageledger.Entry
			for i := 0; i < 100 && len(got) == 0; i++ {
				if got, _ = usageledger.Read(); len(got) == 0 {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if len(got) != 1 || got[0].Session != tc.session || got[0].Agent != tc.agent {
				t.Errorf("ledger = %+v, want session %q agent %q", got, tc.session, tc.agent)
			}
		})
	}
}
