package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
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

	hi := `{"answers":{"tier":{"choice":"deep","probabilities":{"fast":0.02,"standard":0.08,"deep":0.90}},"agent":{"choice":"codex","probabilities":{"codex":1}}}}`
	lo := `{"answers":{"tier":{"choice":"deep","probabilities":{"fast":0.10,"standard":0.20,"deep":0.70}},"agent":{"choice":"codex","probabilities":{"codex":1}}}}`
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

	body = `{"answers":{"tier":{"choice":"standard","probabilities":{"standard":1}},"agent":{"choice":"gemini","probabilities":{"gemini":0.92,"claude":0.08}}}}`
	if got := JevAgent("review security boundary in auth"); got != "gemini" {
		t.Fatalf("JevAgent confident gemini = %q, want gemini", got)
	}
	jevMemo.Clear()
	body = `{"answers":{"tier":{"choice":"standard","probabilities":{"standard":1}},"agent":{"choice":"gemini","probabilities":{"gemini":0.70,"claude":0.30}}}}`
	if got := JevAgent("review security boundary in auth"); got != "" {
		t.Fatalf("JevAgent low-conf = %q, want empty", got)
	}
}

func TestPhase11HelperGuard(t *testing.T) {
	if providers.LastResort(loadRegistryWarn()) == nil {
		t.Fatal("fixture needs a helper provider to test the guard")
	}
	var helperHits atomic.Int32
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		helperHits.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"tier\":\"fast\"}"}}]}`)
	}))
	defer helper.Close()
	t.Setenv("HARNESS_JEV_QWEN_URL", helper.URL)
	for _, class := range []string{jev.ClassDown, jev.ClassOff, jev.ClassTimeout, jev.ClassBadResponse, "backoff"} {
		t.Run(class, func(t *testing.T) {
			checkPhase11HelperGuard(t, class)
			if helperHits.Load() != 0 {
				t.Fatalf("helper called on %s", class)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"tier":{"choice":"standard","probabilities":{"standard":0.7,"fast":0.3}},"agent":{"choice":"codex","probabilities":{"codex":1}}}}`)
	}))
	defer srv.Close()
	t.Setenv("HARNESS_JEV_URL", srv.URL)
	j, _ := askJevClassify(context.Background(), "positive helper control")
	if helperHits.Load() != 1 || j.Helper != "fast" {
		t.Fatalf("healthy low-confidence helper never ran: %+v hits=%d", j, helperHits.Load())
	}
}

func checkPhase11HelperGuard(t *testing.T, class string) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch class {
		case jev.ClassTimeout:
			<-release
		case "backoff":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = io.WriteString(w, "malformed")
		}
	}))
	defer srv.Close()
	defer close(release)
	t.Setenv("HARNESS_JEV_URL", srv.URL)
	switch class {
	case jev.ClassDown:
		srv.Close()
	case jev.ClassOff:
		t.Setenv("HARNESS_JEV_URL", "off")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if class == "backoff" {
		_, _ = askJevClassify(ctx, "prime backoff")
		class = jev.ClassDown
	}
	j, _ := askJevClassify(ctx, "helper guard "+class)
	if j.Error != class || j.Helper != "" {
		t.Fatalf("got %+v, want error %s without helper", j, class)
	}
}

const phase11DeepCodex = `{"answers":{"tier":{"choice":"deep","probabilities":{"deep":1}},"agent":{"choice":"codex","probabilities":{"codex":1}}}}`

func phase11Combined(r *http.Request) bool {
	var in struct {
		Questions map[string]json.RawMessage `json:"questions"`
	}
	return json.NewDecoder(r.Body).Decode(&in) == nil && len(in.Questions) == 2
}

func TestPhase11SharedClassification(t *testing.T) {
	jevMemo.Clear()
	t.Cleanup(jevMemo.Clear)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !phase11Combined(r) {
			t.Error("classification did not combine questions")
		}
		_, _ = io.WriteString(w, phase11DeepCodex)
	}))
	defer srv.Close()
	t.Setenv("HARNESS_JEV_URL", srv.URL)
	const brief = "implement phase 1.1 shared classification"
	const id = "phase11-shared"
	startJev(context.Background(), id, brief)
	defer stopJev(id)
	checkPhase11Shared(t, brief)
	v, _ := jevPending.Load(id)
	j := <-v.(*jevCall).done
	if j.Agent != "codex" || j.Final != "deep" || hits.Load() != 1 {
		t.Fatalf("shadow=%+v hits=%d", j, hits.Load())
	}
}

func checkPhase11Shared(t *testing.T, brief string) {
	t.Helper()
	if got := JevAgentCtx(context.Background(), brief); got != "codex" {
		t.Fatalf("agent=%q", got)
	}
	if alias, signal := TaskTierCtx(context.Background(), brief); alias != models.Opus || signal != "jev:deep" {
		t.Fatalf("tier=%s %s", alias, signal)
	}
	if alias, _ := TaskTier(brief); alias != models.Opus || JevAgent(brief) != "codex" {
		t.Fatal("legacy wrappers lost shared classification")
	}
}

func TestPhase11SlowHelperDoesNotDelayAgent(t *testing.T) {
	jevMemo.Clear()
	t.Cleanup(jevMemo.Clear)
	release := make(chan struct{})
	helper := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer helper.Close()
	defer close(release)
	t.Setenv("HARNESS_JEV_QWEN_URL", helper.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"answers":{"tier":{"choice":"standard","probabilities":{"standard":0.5,"fast":0.5}},"agent":{"choice":"codex","probabilities":{"codex":1}}}}`)
	}))
	defer srv.Close()
	t.Setenv("HARNESS_JEV_URL", srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if got := JevAgentCtx(ctx, "slow helper brief"); got != "codex" || time.Since(start) > time.Second {
		t.Fatalf("agent=%q after %s: blocked on the helper", got, time.Since(start))
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer tcancel()
	if j := jevTierAnswer(tctx, "slow helper brief"); j.Error != jev.ClassTimeout {
		t.Fatalf("tier should wait on its own deadline for the helper: %+v", j)
	}
}

func TestPhase11SharedWaiterDeadline(t *testing.T) {
	jevMemo.Clear()
	t.Cleanup(jevMemo.Clear)
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"answers":{"tier":{"choice":"deep","probabilities":{"deep":1}},"agent":{"choice":"codex","probabilities":{"codex":1}}}}`)
	}))
	defer srv.Close()
	t.Setenv("HARNESS_JEV_URL", srv.URL)
	owner := make(chan struct{})
	go func() { defer close(owner); JevAgent("shared waiter deadline") }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := JevAgentCtx(ctx, "shared waiter deadline")
	close(release)
	<-owner
	if got != "" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("waiter ignored deadline: %q, %s", got, time.Since(start))
	}
}
