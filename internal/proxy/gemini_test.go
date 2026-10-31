package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/ledger"
	"github.com/edotau/claude-code/internal/translate"
)

// dbxGeminiSetup is a Databricks serving row whose derived gemini route lands on a recording upstream.
func dbxGeminiSetup(t *testing.T) (*rawUp, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("DBX_KEY", "dbx-token")
	row := `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up.URL +
		`/serving-endpoints/anthropic","auth":{"type":"key","env":"DBX_KEY"}}`
	srv := setup(t, `{"default":"dbx","providers":{"databricks":{"dbx":`+row+`}}}`)
	return up, srv.URL + "/p/dbx/gemini/"
}

// agy's call reaches /serving-endpoints/gemini with the wire model, alt=sse only, fresh token, thoughts-off fixed.
func TestDatabricksGeminiRoute(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	body := `{"contents":[],"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`
	resp := ptDo(t, http.MethodPost, base+"v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse&key=x", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	if want := "/serving-endpoints/gemini/v1beta/models/databricks-gemini-3-8-flash:streamGenerateContent?alt=sse"; g[0].uri != want {
		t.Errorf("uri = %s, want %s", g[0].uri, want)
	}
	if strings.Contains(g[0].body, "includeThoughts") || !strings.Contains(g[0].body, `"thinkingBudget":0`) {
		t.Errorf("body = %s", g[0].body)
	}
	if a := g[0].hdr.Get("Authorization"); a != "Bearer dbx-token" {
		t.Errorf("authorization = %q", a)
	}
}

// Regression: an encoded or literal climb, or any call but (stream)GenerateContent, never reaches the workspace.
func TestDatabricksGeminiRouteRejectsTraversal(t *testing.T) {
	up, base := dbxGeminiSetup(t)
	for _, rest := range []string{
		"v1beta/models/%2e%2e/%2e%2e/api/2.0/secrets:generateContent", "v1beta/../../api/2.0/clusters/list",
		"v1beta/models/m%252e:generateContent", "v1beta//models/m:generateContent", "v1beta/models/m:countTokens",
		"v1beta/models/m%5c:generateContent",
	} {
		resp := ptDo(t, http.MethodPost, base+rest, `{}`, nil)
		if resp.StatusCode < 400 {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

// Regression: every passthrough dialect refuses a dot-segment tail before building the upstream URL.
func TestPassthroughRejectsDotSegments(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, okRaw)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	for _, rest := range []string{"openai/%2e%2e/admin", "gemini/v1beta/%2e%2e/x", "responses/a%252e%252e/b"} {
		if resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/"+rest, `{}`, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", rest, resp.StatusCode)
		}
	}
	if g := up.got(); len(g) != 0 {
		t.Fatalf("upstream reached: %v", g[0].uri)
	}
}

const geminiChat = `{"model":"databricks-gemini-3-8-flash","stream":true,"stream_options":{"include_usage":true},"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","tool_calls":[` +
	`{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},` +
	`{"id":"b","type":"function","thoughtSignature":"real-sig","function":{"name":"f","arguments":"{}"}},` +
	`{"id":"c","type":"function","extra_content":{"google":{"thought_signature":"x"}},"function":{"name":"f","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"a","content":"ok"}]}`

func sendChat(t *testing.T, body string, h func(http.ResponseWriter, *http.Request, int)) (*rawUp, *http.Response) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, h)
	t.Setenv("PT_KEY", "prov-cred")
	srv := setup(t, ptOverlay(up.URL))
	return up, ptDo(t, http.MethodPost, srv.URL+"/p/pt/openai/chat/completions", body, nil)
}

// Passthrough to a Gemini model drops stream_options and signs unsigned assistant tool_calls only.
func TestPassthroughGeminiChatSanitized(t *testing.T) {
	up, resp := sendChat(t, geminiChat, okRaw)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	g := up.got()
	if len(g) != 1 {
		t.Fatalf("upstream hits %d", len(g))
	}
	var sent struct {
		StreamOptions *json.RawMessage `json:"stream_options"`
		Messages      []struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(g[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.StreamOptions != nil || strings.Contains(g[0].body, "stream_options") {
		t.Errorf("stream_options reached upstream: %s", g[0].body)
	}
	calls := sent.Messages[1].ToolCalls
	if string(calls[0]["thoughtSignature"]) != `"skip_thought_signature_validator"` {
		t.Errorf("unsigned call = %v, want skip signature", calls[0])
	}
	if string(calls[1]["thoughtSignature"]) != `"real-sig"` {
		t.Errorf("signed call changed: %v", calls[1])
	}
	if _, has := calls[2]["thoughtSignature"]; has || calls[2]["extra_content"] == nil {
		t.Errorf("extra_content call touched: %v", calls[2])
	}
}

// A non-Gemini chat body is forwarded byte-identical.
func TestPassthroughNonGeminiChatUntouched(t *testing.T) {
	body := strings.Replace(geminiChat, "databricks-gemini-3-8-flash", "databricks-llama", 1)
	up, resp := sendChat(t, body, okRaw)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if g := up.got(); len(g) != 1 || g[0].body != body {
		t.Fatalf("upstream body %+v, want unchanged", g)
	}
}

func ledgerSettled(t *testing.T, want int) []ledger.Entry {
	t.Helper()
	var got []ledger.Entry
	for i := range 100 {
		got, _ = ledger.Read()
		if len(got) >= want && want > 0 {
			return got
		}
		time.Sleep(20 * time.Millisecond)
		if want == 0 && i >= 10 {
			break
		}
	}
	return got
}

// Passthrough chat usage (plain JSON and the SSE closing chunk) lands in the ledger.
func TestPassthroughMetersChatUsage(t *testing.T) {
	for name, tc := range map[string]struct{ ct, out string }{
		"json": {"application/json", `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`},
		"sse": {"text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":4,\"prompt_tokens\":11,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, resp := sendChat(t, `{"model":"m"}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, tc.out)
			})
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(b) != tc.out {
				t.Fatalf("status %d body %q, want relayed unchanged", resp.StatusCode, b)
			}
			got := ledgerSettled(t, 1)
			if len(got) != 1 || got[0].Profile != "pt" || got[0].Model != "m" || got[0].Input != 11 || got[0].Output != 4 {
				t.Fatalf("ledger = %+v, want pt/m 11 in 4 out", got)
			}
		})
	}
}

// A non-200 upstream answer records nothing.
func TestPassthroughNon200NotMetered(t *testing.T) {
	_, resp := sendChat(t, `{"model":"m"}`, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"usage":{"prompt_tokens":9,"completion_tokens":9}}`)
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if got := ledgerSettled(t, 0); len(got) != 0 {
		t.Fatalf("ledger = %+v, want empty", got)
	}
}

// A null tool_calls entry passes through instead of panicking the handler.
func TestFixGeminiChatNullToolCall(t *testing.T) {
	in := `{"model":"databricks-gemini-3-8-flash","messages":[{"role":"assistant","tool_calls":[null,{"id":"a","type":"function"}]}]}`
	out := string(fixGeminiChat([]byte(in)))
	if !strings.Contains(out, "null") || !strings.Contains(out, translate.SkipThoughtSignature) {
		t.Fatalf("null entry or signature lost: %s", out)
	}
}
