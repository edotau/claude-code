package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/router/jev"
)

// hangJev serves /v1/systemone only once the client leaves, so a shadow call stays mid-flight until cancel.
func hangJev(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a client leaving only once the body is read
		<-r.Context().Done()
	}))
	// cleanups run last-first: close connections before Close waits on the handler, so a failed check never hangs
	t.Cleanup(srv.Close)
	t.Cleanup(srv.CloseClientConnections)
	t.Cleanup(http.DefaultClient.CloseIdleConnections) // a dial cancelled mid-flight parks its conn, which hangs Close
	t.Setenv("HARNESS_JEV_URL", srv.URL)
}

// settledGoroutines is the process goroutine count once it holds steady across two samples.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	last := runtime.NumGoroutine()
	for range 40 {
		runtime.GC()
		time.Sleep(25 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			return n
		}
		last = n
	}
	t.Fatalf("goroutine count never settled (last %d)", last)
	return last
}

// waitGoroutinesAbove returns once the count tops base, proving a shadow is in flight.
func waitGoroutinesAbove(t *testing.T, base int) {
	t.Helper()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() > base {
			return
		}
	}
	t.Fatalf("goroutines = %d, never rose above baseline %d: the shadow never started", runtime.NumGoroutine(), base)
}

// waitGoroutines polls until the count is back to base; a dial cancelled mid-flight parks its conn and goroutines.
func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		http.DefaultClient.CloseIdleConnections()
		if runtime.NumGoroutine() <= base {
			return
		}
	}
	t.Fatalf("goroutines = %d, want <= %d: the run leaked", runtime.NumGoroutine(), base)
}

// A cancelled dispatch's jev shadow exits with it instead of hanging the local server until its own timeout.
func TestStartJevShadowExitsOnDispatchCancel(t *testing.T) {
	hangJev(t)
	base := settledGoroutines(t)

	ctx, cancel := context.WithCancel(context.Background())
	id := "test-jev-cancel"
	startJev(ctx, id, "review the sync.Map changes in internal/agent")
	t.Cleanup(func() { stopJev(id) })
	waitGoroutinesAbove(t, base) // the shadow and its HTTP call are in flight
	if _, ok := jevPending.Load(id); !ok {
		t.Fatal("startJev kept no pending call")
	}

	cancel()
	waitGoroutines(t, base)
	if j := takeJev(id); j == nil || j.Error != jev.ClassTimeout {
		t.Errorf("takeJev after cancel = %+v, want timeout class", j)
	}
}

// Finishing a record stops its unanswered shadow: the result channel is nobody's reason to keep the HTTP call.
func TestTakeJevStopsAnUnansweredShadow(t *testing.T) {
	hangJev(t)
	base := settledGoroutines(t)

	id := "test-jev-take"
	startJev(context.Background(), id, "summarize the fallback changes")
	waitGoroutinesAbove(t, base)

	j := takeJev(id) // the run ended first: timeout class, and the in-flight call is cancelled
	if j == nil || j.Error != jev.ClassTimeout || j.Keyword != tierName("summarize the fallback changes") {
		t.Errorf("takeJev = %+v, want timeout with keyword %q", j, tierName("summarize the fallback changes"))
	}
	waitGoroutines(t, base)
}

// A background job's parent drops the pending shadow at spawn: the detached child starts one of its own.
func TestStopJevReleasesASpawnedJobsShadow(t *testing.T) {
	hangJev(t)
	base := settledGoroutines(t)

	id := "test-jev-spawn"
	startJev(context.Background(), id, "explain the job recovery path")
	waitGoroutinesAbove(t, base)

	stopJev(id)
	if _, ok := jevPending.Load(id); ok {
		t.Error("stopJev kept the pending entry")
	}
	waitGoroutines(t, base)
	if j := takeJev(id); j != nil {
		t.Errorf("takeJev after stopJev = %+v, want nil", j)
	}
}

func TestJevBriefStripsRecallAndMeta(t *testing.T) {
	in := "<memory-recall>\nsecret old context\n</memory-recall>\nTier: deep\nWorkdir: /tmp/x\nHARD CAP: 5\nFix typo in `README.md`\nInvestigate race under concurrent writes\n\n## Leaf rules\nNever commit"
	want := "Fix typo in `README.md`\nInvestigate race under concurrent writes"
	if got := jevBrief(in); got != want {
		t.Fatalf("jevBrief = %q, want %q", got, want)
	}
}

func TestTaskTierUsesConfidentJev(t *testing.T) {
	var body, qwenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	t.Setenv("HARNESS_JEV_URL", srv.URL)
	qsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(qwenBody)) }))
	t.Cleanup(qsrv.Close)

	hi := `{"answers":{"tier":{"choice":"deep","probabilities":{"fast":0.02,"standard":0.08,"deep":0.90}}}}`
	lo := `{"answers":{"tier":{"choice":"deep","probabilities":{"fast":0.10,"standard":0.20,"deep":0.70}}}}`
	for _, tc := range []struct {
		jev, qwen, brief, wantAlias, wantSig string
	}{
		{hi, "", "update the config loader", models.Opus, "jev:deep"},
		{hi, "", "Tier: fast\nupdate the config loader", models.Haiku, "Tier: fast"},
		{lo, "", "update the config loader", models.Sonnet, "standard"},
		{lo, `{"choices":[{"message":{"content":"{\"reason\":\"Scoped\",\"tier\":\"deep\"}"}}]}`, "update the config loader", models.Opus, "jev-qwen:deep"},
		{lo, `{"choices":[{"message":{"content":"{\"reason\":\"Trivial\",\"tier\":\"fast\"}"}}]}`, "design the multi-file architecture for auth", models.Opus, "deep:auth"},
	} {
		body, qwenBody = tc.jev, tc.qwen
		jevMemo.Clear()
		if tc.qwen != "" {
			t.Setenv("HARNESS_JEV_QWEN_URL", qsrv.URL)
		}
		if got, sig := TaskTier(tc.brief); got != tc.wantAlias || sig != tc.wantSig {
			t.Fatalf("TaskTier(%q): got (%q, %q), want (%q, %q)", tc.brief, got, sig, tc.wantAlias, tc.wantSig)
		}
	}
}

func TestJevShadowFinalHelperSafety(t *testing.T) {
	if got := (JevShadow{Pick: "standard", Confidence: 0.60, Helper: "deep", Keyword: "standard"}).final(); got != "deep" {
		t.Fatalf("helper deep = %q, want deep", got)
	}
	if got := (JevShadow{Pick: "standard", Confidence: 0.60, Helper: "fast", Keyword: "deep"}).final(); got != "deep" {
		t.Fatalf("helper fast on deep keyword = %q, want deep", got)
	}
}

func TestJevAgentPicksConfidentKnownVendor(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	t.Setenv("HARNESS_JEV_URL", srv.URL)

	body = `{"answers":{"tier":{"choice":"standard","probabilities":{"standard":0.9}},"agent":{"choice":"gemini","probabilities":{"gemini":0.92}}}}`
	if got := JevAgent("review security boundary in auth"); got != "gemini" {
		t.Fatalf("JevAgent confident gemini = %q, want gemini", got)
	}
	jevMemo.Clear()
	body = `{"answers":{"tier":{"choice":"standard","probabilities":{"standard":0.9}},"agent":{"choice":"gemini","probabilities":{"gemini":0.70}}}}`
	if got := JevAgent("review security boundary in auth"); got != "" {
		t.Fatalf("JevAgent low-conf = %q, want empty", got)
	}
}
