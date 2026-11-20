package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

func TestSwapped(t *testing.T) {
	for _, c := range []struct {
		asked, served string
		want          bool
	}{
		{"claude-opus-5-5", "databricks-claude-opus-5-5", false},
		{"claude-opus-5-5", "claude-opus-5-5", false},
		{"", "databricks-claude-opus-5-5", false},
		{"claude-fable-5-2", "databricks-claude-opus-5-5", true},
	} {
		if got := swapped(c.asked, c.served); got != c.want {
			t.Errorf("swapped(%q, %q) = %v, want %v", c.asked, c.served, got, c.want)
		}
	}
}

func BenchmarkSwapped(b *testing.B) {
	for range b.N {
		swapped("claude-opus-5-5", "databricks-claude-opus-5-5")
	}
}

// waitLog polls for sub: the request line is written just after the reply.
func waitLog(log *lockedBuf, sub string) {
	for i := 0; i < 100 && !strings.Contains(log.String(), sub); i++ {
		time.Sleep(20 * time.Millisecond)
	}
}

func swapRig(t *testing.T) (srvURL string, log *lockedBuf) {
	t.Helper()
	useGate(t)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":4,"output_tokens":1}}`)
	})
	t.Setenv("DEV_TOKEN", "tok")
	t.Setenv("SUB_KEY", "k")
	log = &lockedBuf{}
	srv := setupRouter(t, clientTokenOverlay(up.URL), func(r *Router) { r.log = log })
	return srv.URL, log
}

func TestSwappedRequestRecordsRequestedAndLogsReq(t *testing.T) {
	url, log := swapRig(t)
	if resp := post(t, url+"/p/dev/v1/messages", `{"model":"claude-fable-5-2","max_tokens":5}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	got := waitLedger(t)
	if len(got) != 1 || got[0].Model != "databricks-claude-opus-5-5" || got[0].Requested != "claude-fable-5-2" {
		t.Fatalf("ledger = %+v, want model opus-5-5 requested claude-fable-5-2", got)
	}
	waitLog(log, " asked=")
	out := log.String()
	for _, want := range []string{" asked=claude-fable-5-2", "proxy: dev asked for databricks-claude-fable-5-2, serving databricks-claude-opus-5-5"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestEquivalentIDRecordsNothingExtra(t *testing.T) {
	url, log := swapRig(t)
	if resp := post(t, url+"/p/dev/v1/messages", `{"model":"claude-opus-5-5[1m]","max_tokens":5}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	got := waitLedger(t)
	if len(got) != 1 || got[0].Model != "databricks-claude-opus-5-5" || got[0].Requested != "" {
		t.Fatalf("ledger = %+v, want no requested", got)
	}
	waitLog(log, " 200 ")
	if out := log.String(); strings.Contains(out, "asked=") || strings.Contains(out, "asked for") {
		t.Errorf("log carries swap text for an equivalent id:\n%s", out)
	}
}

func TestNoteSwapLogsOncePerMinutePerTriple(t *testing.T) {
	log := &lockedBuf{}
	now := time.Unix(1_700_000_000, 0)
	r := &Router{log: log, now: func() time.Time { return now }}
	for range 5 {
		r.noteSwap("dev", "a", "b", "why")
	}
	r.noteSwap("dev", "a", "c", "why")
	if n := strings.Count(log.String(), "\n"); n != 2 {
		t.Fatalf("%d lines, want 2:\n%s", n, log.String())
	}
	now = now.Add(swapLogEvery)
	r.noteSwap("dev", "a", "b", "why")
	if n := strings.Count(log.String(), "\n"); n != 3 {
		t.Fatalf("%d lines after a minute, want 3:\n%s", n, log.String())
	}
}

func TestDeadDemotionLogsSwap(t *testing.T) {
	useGate(t)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":4,"output_tokens":1}}`)
	})
	t.Setenv("DEV_TOKEN", "tok")
	t.Setenv("SUB_KEY", "k")
	log := &lockedBuf{}
	overlay := strings.Replace(clientTokenOverlay(up.URL), `"models":{"opus":"databricks-claude-opus-5-5"}`,
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}`, 1)
	srv := setupRouter(t, overlay, func(r *Router) {
		r.log = log
		reg, err := r.registry()
		if err != nil {
			t.Fatal(err)
		}
		r.markDead(providers.Target{Provider: reg.Row("dev"), Model: "databricks-claude-opus-5-5"})
		t.Cleanup(r.bg.Wait)
	})
	if resp := post(t, srv.URL+"/p/dev/v1/messages", `{"model":"databricks-claude-opus-5-5","max_tokens":5}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	waitLog(log, " asked=")
	want := "proxy: dev asked for databricks-claude-opus-5-5, serving databricks-claude-sonnet-5-5 (quota-dead for this key)"
	if out := log.String(); !strings.Contains(out, want) || !strings.Contains(out, " asked=databricks-claude-opus-5-5") {
		t.Errorf("log lacks the demotion line and req= token:\n%s", out)
	}
}

func TestLedgerModelLabelsModelessPassthrough(t *testing.T) {
	for _, c := range []struct{ model, path, want string }{
		{"gpt-x", "/p/pt/openai/v1/chat/completions", "gpt-x"},
		{"", "/p/pt/openai/v1/embeddings", "(passthrough:embeddings)"},
		{"", "/p/pt/openai/chat/completions", "(passthrough:chat/completions)"},
		{"", "/p/pt/openai/my-endpoint/invocations", "my-endpoint"},
		{"", "/p/pt/responses/v1/responses", "(passthrough:responses)"},
		{"", "/p/pt/openai/a b/c d", "(passthrough:unknown)"},
	} {
		req, _ := http.NewRequest(http.MethodPost, "http://x"+c.path, nil)
		if got := ledgerModel(req, c.model); got != c.want {
			t.Errorf("ledgerModel(%q, %q) = %q, want %q", c.model, c.path, got, c.want)
		}
	}
}

func TestPassthroughLedgerRowCarriesLabel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[],"usage":{"prompt_tokens":9,"total_tokens":9}}`)
	})
	t.Setenv("PT_KEY", "k")
	srv := setup(t, ptOverlay(up.URL))
	if resp := post(t, srv.URL+"/p/pt/openai/embeddings", `{"input":"x"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	got := waitLedger(t)
	if len(got) != 1 || got[0].Model != "(passthrough:embeddings)" || got[0].Input != 9 {
		t.Fatalf("ledger = %+v, want label model", got)
	}
}
