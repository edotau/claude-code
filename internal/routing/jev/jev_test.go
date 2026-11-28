package jev

import (
	_ "github.com/edotau/claude-code/internal/platform/nonet"

	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "jev-backoff")
	backoffPath = func() string { return filepath.Join(dir, "marker") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// isolate gives a test a fresh marker path and a controllable clock.
func isolate(t *testing.T) *time.Time {
	t.Helper()
	oldP, oldN := backoffPath, backoffNow
	p := filepath.Join(t.TempDir(), "marker")
	now := time.Unix(1000, 0)
	backoffPath, backoffNow = func() string { return p }, func() time.Time { return now }
	t.Cleanup(func() { backoffPath, backoffNow = oldP, oldN })
	return &now
}

func serve(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	t.Setenv(urlEnv, s.URL)
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func TestJevGood(t *testing.T) {
	isolate(t)
	serve(t, reply(`{"answers":{"tier":{"type":"choice","choice":"deep","probabilities":{"fast":0.1,"standard":0.2,"deep":0.7},"confidence":0.7}}}`))
	r, err := Tier(context.Background(), "audit it")
	if err != nil || r.Pick != "deep" || r.Confidence != 0.7 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestJevDown(t *testing.T) {
	isolate(t)
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close() // the test's own port, now closed: nothing else is ever dialed
	t.Setenv(urlEnv, s.URL)
	if _, err := Tier(context.Background(), "x"); Class(err) != ClassDown {
		t.Fatalf("class %q, err %v", Class(err), err)
	}
}

func TestJevTimeout(t *testing.T) {
	isolate(t)
	release := make(chan struct{})
	serve(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Tier(ctx, "x"); Class(err) != ClassTimeout {
		t.Fatalf("class %q, err %v", Class(err), err)
	}
}

func TestJevMalformed(t *testing.T) {
	isolate(t)
	for _, body := range []string{`nope`, `{"answers":{"tier":{"choice":"huge","probabilities":{"huge":1}}}}`,
		`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":1.5}}}}`,
		`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":0.2,"standard":0.3,"deep":0.5}}}}`,
		`{"answers":{"tier":{"choice":"deep","probabilities":{"fast":0.5,"standard":0.5,"deep":0.6}}}}`,
		`{"answers":{"tier":{"choice":"deep","probabilities":{"fast":0.1,"standard":0.1,"deep":0.3}}}}`,
		`{"answers":{}}`} {
		serve(t, reply(body))
		if _, err := Tier(context.Background(), "x"); Class(err) != ClassBadResponse {
			t.Errorf("%s: class %q, err %v", body, Class(err), err)
		}
	}
}

func TestJevOff(t *testing.T) {
	isolate(t)
	t.Setenv(urlEnv, "off")
	if _, err := Tier(context.Background(), "x"); Class(err) != ClassOff {
		t.Fatalf("class %q", Class(err))
	}
}

func TestJevNeeds(t *testing.T) {
	isolate(t)
	serve(t, reply(`{"answers":{"needs":{"type":"choice","choice":"yes","probabilities":{"yes":0.9,"no":0.1}}}}`))
	r, err := Needs(context.Background(), "write the parser", "test the parser")
	if err != nil || r.Pick != "yes" || r.Confidence != 0.9 {
		t.Fatalf("got %+v, %v", r, err)
	}
	serve(t, reply(`{"answers":{"needs":{"choice":"maybe","probabilities":{"maybe":1}}}}`))
	if _, err := Needs(context.Background(), "a", "b"); Class(err) != ClassBadResponse {
		t.Fatalf("err %v", err)
	}
}

func TestJevAgent(t *testing.T) {
	isolate(t)
	serve(t, reply(`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":0.9,"standard":0.1}},"agent":{"choice":"pi","probabilities":{"pi":0.8,"claude":0.2}}}}`))
	r, err := Agent(context.Background(), "rename x")
	if err != nil || r.Pick != "pi" || r.Confidence != 0.8 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestJevBadAgent(t *testing.T) {
	isolate(t)
	serve(t, reply(`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":0.9}},"agent":{"choice":42}}}`))
	if _, err := Agent(context.Background(), "x"); Class(err) != ClassBadResponse {
		t.Fatalf("class %q, err %v", Class(err), err)
	}
}

func TestJevHelper(t *testing.T) {
	isolate(t)
	s := httptest.NewServer(reply(`{"choices":[{"message":{"content":"{\"tier\":\"fast\"}"}}]}`))
	t.Cleanup(s.Close)
	t.Setenv(qwenEnv, s.URL)
	if got, err := Helper(context.Background(), "http://unused", "m", "rename x"); err != nil || got != "fast" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestJevBackoff(t *testing.T) {
	now := isolate(t)
	hits, status := 0, 500
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		reply(`{"answers":{"tier":{"choice":"deep","probabilities":{"deep":0.7,"standard":0.3}}}}`)(w, r)
	})
	target := os.Getenv(urlEnv)
	ctx := context.Background()
	if _, err := Tier(ctx, "x"); Class(err) != ClassBadResponse || !backedOff(target) {
		t.Fatalf("5xx: %v backedOff=%v", err, backedOff(target))
	}
	if backedOff("http://127.0.0.1:1") {
		t.Fatalf("different target should not be backed off")
	}
	if _, err := Tier(ctx, "x"); Class(err) != ClassDown || hits != 1 {
		t.Fatalf("active backoff: %v hits=%d", err, hits)
	}
	*now = now.Add(backoffTTL + time.Second)
	status = 200
	if r, err := Tier(ctx, "x"); err != nil || r.Pick != "deep" || hits != 2 || backedOff(target) {
		t.Fatalf("recovery: %+v %v hits=%d", r, err, hits)
	}
	if _, err := os.Stat(backoffPath()); !os.IsNotExist(err) {
		t.Fatalf("marker not cleared: %v", err)
	}
}

func TestJevBackoffDownWrites(t *testing.T) {
	isolate(t)
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close()
	t.Setenv(urlEnv, s.URL)
	if _, err := Tier(context.Background(), "x"); Class(err) != ClassDown || !backedOff(s.URL) {
		t.Fatalf("down: %v backedOff=%v", err, backedOff(s.URL))
	}
}

func TestJevBackoffIgnoresCallerCancel(t *testing.T) {
	isolate(t)
	release := make(chan struct{})
	serve(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Tier(ctx, "x"); Class(err) != ClassTimeout {
		t.Fatalf("expected timeout, got %v", err)
	}
	if backedOff(os.Getenv(urlEnv)) {
		t.Fatalf("caller deadline must not set shared backoff")
	}
}

const phase11Answers = `{"answers":{"tier":{"choice":"fast","probabilities":{"fast":0.9,"deep":0.1}},"agent":{"choice":"pi","probabilities":{"pi":0.95,"codex":0.05}}}}`

// phase11RequestOK reports whether r is the one combined tier+agent request for "rename x".
func phase11RequestOK(r *http.Request) bool {
	var in struct {
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Questions) != 2 {
		return false
	}
	return string(in.Questions["agent"]) == agentQuestion && len(in.Questions["tier"]) != 0 &&
		in.State == "rename x" && r.URL.Path == "/v1/systemone"
}

func TestPhase11ClassifyOneRequest(t *testing.T) {
	isolate(t)
	var hits atomic.Int32
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !phase11RequestOK(r) {
			t.Errorf("unexpected request at %s", r.URL.Path)
		}
		reply(phase11Answers)(w, r)
	})
	tier, agent, err := Classify(context.Background(), "rename x")
	if err != nil || hits.Load() != 1 || tier.Pick != "fast" || tier.Confidence != 0.9 ||
		agent.Pick != "pi" || agent.Confidence != 0.95 || tier.Ms != agent.Ms {
		t.Fatalf("tier=%+v agent=%+v err=%v hits=%d", tier, agent, err, hits.Load())
	}
}

func TestPhase11ProbeAddrIPv6(t *testing.T) {
	for in, want := range map[string]string{
		"http://[::1]":         "[::1]:80",
		"https://[::1]":        "[::1]:443",
		"http://[::1]:11401":   "[::1]:11401",
		"http://127.0.0.1:9":   "127.0.0.1:9",
		"http://localhost":     "localhost:80",
		"https://example.test": "example.test:443",
		"nohost":               "",
	} {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPhase11PendingLocalBackoffSurvivesLockFailure(t *testing.T) {
	isolate(t)
	const target, other = "http://pending.test", "http://other.test"
	p := backoffPath()
	backoffMemory.Lock()
	clear(backoffMemory.pendingLocal)
	backoffMemory.Unlock()
	if err := os.WriteFile(p, []byte(`{"`+other+`":{"until":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ran, err := paths.TryFileLock(p+".lock", time.Second, func() error {
		noteOutcome(target, ErrDown, 0) // lock is held elsewhere: this update stays local
		return nil
	})
	if !ran || err != nil {
		t.Fatalf("lock: ran=%v err=%v", ran, err)
	}
	if !backedOff(target) {
		t.Fatal("older disk map discarded newer unsaved local backoff")
	}
	noteOutcome(other, ErrDown, 0) // persisted now: pending is cleared and the disk keeps both
	backoffMemory.Lock()
	clear(backoffMemory.states)
	backoffMemory.Unlock()
	if !backedOff(target) || !backedOff(other) {
		t.Fatal("persisted merge lost a target")
	}
}

func TestPhase11SlowBackoffShared(t *testing.T) {
	now := isolate(t)
	var hits atomic.Int32
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(slowThreshold + 5*time.Millisecond)
		reply(phase11Answers)(w, r)
	})
	target := os.Getenv(urlEnv)
	for i := range 2 {
		if _, _, err := Classify(context.Background(), "rename x"); err != nil {
			t.Fatal(err)
		}
		backoffMemory.Lock()
		clear(backoffMemory.states) // emulate a fresh hook process between live calls
		backoffMemory.Unlock()
		if got := backedOff(target); got != (i == 1) {
			t.Fatalf("after call %d: backedOff=%v", i+1, got)
		}
	}
	if _, _, err := Classify(context.Background(), "rename y"); Class(err) != ClassDown || hits.Load() != 2 {
		t.Fatalf("backoff: %v hits=%d", err, hits.Load())
	}
	*now = now.Add(backoffTTL + time.Second)
	if backedOff(target) {
		t.Fatal("slow backoff did not expire")
	}
}

func TestPhase11SlowStreakResetAndTargets(t *testing.T) {
	isolate(t)
	const classifier, helper = "classifier", "helper"
	slow := slowThreshold + time.Nanosecond
	noteOutcome(classifier, nil, slow)
	noteOutcome(classifier, nil, time.Millisecond)
	noteOutcome(classifier, nil, slow)
	if backedOff(classifier) {
		t.Fatal("fast success did not reset the streak")
	}
	noteOutcome(classifier, nil, slow)
	noteOutcome(helper, nil, slow)
	if !backedOff(classifier) || backedOff(helper) {
		t.Fatal("targets share health incorrectly")
	}
	noteOutcome(helper, nil, slow)
	if !backedOff(classifier) || !backedOff(helper) {
		t.Fatal("helper slow state did not persist alongside classifier backoff")
	}
	noteOutcome(helper, nil, time.Millisecond)
	if !backedOff(classifier) || backedOff(helper) {
		t.Fatal("helper recovery cleared classifier backoff")
	}
}

func TestPhase11HelperBackoff(t *testing.T) {
	now := isolate(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv(qwenEnv, srv.URL)
	if _, err := Helper(context.Background(), "unused", "m", "x"); Class(err) != ClassBadResponse {
		t.Fatal(err)
	}
	if _, err := Helper(context.Background(), "unused", "m", "x"); Class(err) != ClassDown || hits.Load() != 1 {
		t.Fatalf("helper did not back off: %v hits=%d", err, hits.Load())
	}
	*now = now.Add(backoffTTL + time.Second)
	if _, err := Helper(context.Background(), "unused", "m", "x"); Class(err) != ClassBadResponse || hits.Load() != 2 {
		t.Fatalf("helper did not recover: %v hits=%d", err, hits.Load())
	}
}

func TestPhase11BackoffMemoryFallback(t *testing.T) {
	isolate(t)
	if err := os.Mkdir(backoffPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	const target = "unwritable-marker"
	noteOutcome(target, nil, slowThreshold+time.Nanosecond)
	if backedOff(target) {
		t.Fatal("first slow call backed off")
	}
	noteOutcome(target, nil, slowThreshold+time.Nanosecond)
	if !backedOff(target) {
		t.Fatal("in-process backoff lost when marker could not be written")
	}
}

func TestJevProbeDownSeedsBackoff(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Setenv(urlEnv, srv.URL)
	if err := Probe(context.Background()); err != nil {
		t.Fatalf("Probe on live server = %v, want nil", err)
	}
	srv.Close()
	if err := Probe(context.Background()); err != ErrDown {
		t.Fatalf("Probe on closed server = %v, want ErrDown", err)
	}
	if !backedOff(srv.URL) {
		t.Fatal("Probe down did not seed backoff")
	}
}
