package server

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tokenledger"
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
		io.WriteString(w,
			`{"type":"message","content":[],"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2}}`)
	})
	t.Setenv("DATABRICKS_PROD_PAT", "tok")
	row := fmt.Sprintf(`{"provider":"databricks","protocol":"anthropic","base_url":%q,"auth":{"type":"pat"},`+
		`"models":{"opus":"m-opus","haiku":"m-haiku"}}`, up.URL)
	srv := setup(t, `{"default":"prod","providers":{"databricks":{"prod":`+row+`}}}`)
	resp := post(t, srv.URL+"/p/prod/v1/messages", `{"model":"m-haiku"}`,
		map[string]string{"X-Claude-Code-Session-Id": "sess-1", gateway.AgentHeader: "claude"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	got := waitLedger(t)
	if len(got) != 1 {
		t.Fatalf("ledger = %+v, want 1 entry", got)
	}
	checkNonKeyLedgerEntry(t, got[0])
	if in, out, cr := tokenledger.TotalsBySession("sess-1"); in != 7 || out != 3 || cr != 2 {
		t.Errorf("totals = %d %d %d", in, out, cr)
	}
}

// checkNonKeyLedgerEntry asserts the entry carries the prod profile, databricks provider, session, agent and usage.
func checkNonKeyLedgerEntry(t *testing.T, e tokenledger.Entry) {
	t.Helper()
	if e.Profile != "prod" || e.Provider != "databricks" || e.Session != "sess-1" || e.Agent != "claude" ||
		e.Input != 7 ||
		e.Output != 3 ||
		e.CacheRead != 2 {
		t.Errorf("entry = %+v", e)
	}
}

// waitLedger polls until the ledger has an entry; the append lands just after the relay returns.
func waitLedger(t *testing.T) []tokenledger.Entry {
	t.Helper()
	var got []tokenledger.Entry
	for i := 0; i < 100 && len(got) == 0; i++ {
		if got, _ = tokenledger.Read(); len(got) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return got
}

func TestScrapeUsage(t *testing.T) {
	if u := scrapeAnthropicUsage([]byte(`data: {"message":{"usage":{"input_tokens":5}}}` + "\n" + `{"usage":{"output_tokens":9}}`)); u.input != 5 ||
		u.output != 9 {
		t.Errorf("anthropic = %+v", u)
	}
	if u := scrapeChatUsage([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`)); u.input != 6 ||
		u.output != 2 ||
		u.cacheRead != 4 {
		t.Errorf("chat = %+v", u)
	}
}

// Session comes from X-Claude-Code-Session-Id; absent headers leave empty fields, not a failure.
func TestLedgerAttributionHeaders(t *testing.T) {
	for _, tc := range []struct {
		name           string
		hdr            map[string]string
		session, agent string
	}{
		{"both", map[string]string{"X-Claude-Code-Session-Id": "s1", gateway.AgentHeader: "claude"}, "s1", "claude"},
		{"session-id fallback", map[string]string{"X-Claude-Code-Session-Id": "s2"}, "s2", ""},
		{"neither", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			})
			t.Setenv("DATABRICKS_PROD_PAT", "tok")
			row := fmt.Sprintf(`{"provider":"databricks","protocol":"anthropic","base_url":%q,"auth":{"type":"pat"},`+
				`"models":{"opus":"m-opus","haiku":"m-haiku"}}`, up.URL)
			srv := setup(t, `{"default":"prod","providers":{"databricks":{"prod":`+row+`}}}`)
			resp := post(t, srv.URL+"/p/prod/v1/messages", `{"model":"m-haiku"}`, tc.hdr)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
			}
			var got []tokenledger.Entry
			for i := 0; i < 100 && len(got) == 0; i++ {
				if got, _ = tokenledger.Read(); len(got) == 0 {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if len(got) != 1 || got[0].Session != tc.session || got[0].Agent != tc.agent {
				t.Errorf("ledger = %+v, want session %q agent %q", got, tc.session, tc.agent)
			}
		})
	}
}

// A direct (non-Databricks) row's ledger entry carries its provider, never an empty one.
func TestLedgerDirectProviderNamed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":4,"output_tokens":1}}`)
	})
	t.Setenv("DIRECT_TEST_KEY", "k")
	row := fmt.Sprintf(
		`{"protocol":"anthropic","base_url":%q,"auth":{"type":"key","header":"x-api-key","env":"DIRECT_TEST_KEY"},`+
			`"models":{"opus":"m-opus","haiku":"m-haiku"}}`,
		up.URL,
	)
	srv := setup(t, `{"default":"direct","providers":{"anthropic":{"direct":`+row+`}}}`)
	if resp := post(t, srv.URL+"/p/direct/v1/messages", `{"model":"m-haiku"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	var got []tokenledger.Entry
	for i := 0; i < 100 && len(got) == 0; i++ {
		if got, _ = tokenledger.Read(); len(got) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(got) != 1 || got[0].Provider != "anthropic" || got[0].Profile != "direct" {
		t.Errorf("ledger = %+v, want provider anthropic profile direct", got)
	}
}

func TestScrapeAnthropicUsageCumulativeDeltaNotSummed(t *testing.T) {
	body := `data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":30}}}` + "\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":30}}`
	u := scrapeAnthropicUsage([]byte(body))
	if u.input != 100 || u.output != 50 || u.cacheRead != 30 {
		t.Errorf("usage = %+v, want 100/50/30 (cumulative delta must not be added to start)", u)
	}
}

// An embeddings-style usage object has no completion_tokens; the prompt count must still be recorded.
func TestScrapeChatUsageMissingOutputKeepsInput(t *testing.T) {
	u := scrapeChatUsage([]byte(`{"usage":{"prompt_tokens":8,"total_tokens":8}}`))
	if u.input != 8 || u.output != 0 {
		t.Errorf("usage = %+v, want input 8 output 0", u)
	}
	if u := scrapeChatUsage([]byte(`{"usage":{"prompt_tokens":null,"completion_tokens":3}}`)); u.output != 3 {
		t.Errorf("null prompt_tokens: usage = %+v, want output 3", u)
	}
}

func TestScrapeLastUsageSubtractsCachedForBothDialects(t *testing.T) {
	chat := scrapeChatUsage(
		[]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":40}}}`),
	)
	if chat.input != 60 || chat.output != 5 || chat.cacheRead != 40 {
		t.Errorf("chat = %+v, want 60/5/40", chat)
	}
	resp := scrapeUsage(
		[]byte(`{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":200}}}`),
	)
	if resp.input != 100 || resp.cacheRead != 0 {
		t.Errorf("responses = %+v, cached > input must not be subtracted", resp)
	}
}
