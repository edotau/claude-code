package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/providers"
)

const testModel = "qwen-test"

func localReg(base string) *providers.Registry {
	row := &providers.Provider{Name: "local", BaseURL: base + "/inference/v1", Concurrency: 1,
		Models: map[string]string{"sonnet": testModel}, Options: providers.Options{LastResort: true}}
	return &providers.Registry{Providers: map[string]*providers.Provider{"local": row}}
}

func resetProbe(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	probeCache.Lock()
	probeCache.e = probeEntry{}
	probeCache.Unlock()
}

func modelsServer(t *testing.T, status int, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLocalUp(t *testing.T) {
	good := `{"data":[{"id":"` + testModel + `"}]}`
	cases := []struct {
		name   string
		status int
		body   string
		up     bool
		down   LocalDown
	}{
		{"up", http.StatusOK, good, true, ""},
		{"bad status", http.StatusServiceUnavailable, good, false, LocalBadStatus},
		{"empty list", http.StatusOK, `{"data":[]}`, false, LocalNoModel},
		{"wrong model", http.StatusOK, `{"data":[{"id":"other"}]}`, false, LocalNoModel},
		{"bad body", http.StatusOK, `not json`, false, LocalBadBody},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetProbe(t)
			srv := modelsServer(t, c.status, c.body, nil)
			p := ProbeLocal(context.Background(), localReg(srv.URL))
			if p.Up() != c.up || p.Down != c.down {
				t.Fatalf("got up=%v down=%q (%s), want up=%v down=%q", p.Up(), p.Down, p.Detail, c.up, c.down)
			}
			if c.up && p.Model != testModel {
				t.Fatalf("model %q", p.Model)
			}
		})
	}
}

func TestLocalUpRefused(t *testing.T) {
	resetProbe(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	reg := localReg(srv.URL)
	srv.Close()
	if up, why := LocalUp(context.Background(), reg); up || why != string(LocalRefused) {
		t.Fatalf("got %v %q", up, why)
	}
}

func TestLocalUpHangPastDeadline(t *testing.T) {
	resetProbe(t)
	localProbeTimeout = 40 * time.Millisecond
	t.Cleanup(func() { localProbeTimeout = defaultProbeTimeout })
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stop }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	start := time.Now()
	p := ProbeLocal(context.Background(), localReg(srv.URL))
	if p.Down != LocalTimeout {
		t.Fatalf("down %q (%s)", p.Down, p.Detail)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("probe blocked %s", d)
	}
}

func TestLocalUpCache(t *testing.T) {
	resetProbe(t)
	var hits atomic.Int32
	srv := modelsServer(t, http.StatusOK, `{"data":[{"id":"`+testModel+`"}]}`, &hits)
	reg := localReg(srv.URL)
	clock := time.Now()
	localNow = func() time.Time { return clock }
	t.Cleanup(func() { localNow = time.Now })
	for range 3 {
		if up, _ := LocalUp(context.Background(), reg); !up {
			t.Fatal("want up")
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("cache missed: %d hits", hits.Load())
	}
	srv.Close()
	if up, _ := LocalUp(context.Background(), reg); !up {
		t.Fatal("within the interval the cached answer stands")
	}
	clock = clock.Add(localProbeTTL + time.Second)
	if up, why := LocalUp(context.Background(), reg); up || why != string(LocalRefused) {
		t.Fatalf("after expiry a failure must replace the success: %v %q", up, why)
	}
	if up, _ := LocalUp(context.Background(), reg); up {
		t.Fatal("a failure is cached, never flipped back to up")
	}
}

func TestLocalUpCancelledNotCached(t *testing.T) {
	resetProbe(t)
	srv := modelsServer(t, http.StatusOK, `{"data":[{"id":"`+testModel+`"}]}`, nil)
	reg := localReg(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p := ProbeLocal(ctx, reg); p.Down != LocalCancelled {
		t.Fatalf("down %q", p.Down)
	}
	if up, _ := LocalUp(context.Background(), reg); !up {
		t.Fatal("a cancelled probe must not poison the cache")
	}
}

func admitEnv(t *testing.T) inflight.Limits {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	localAdmitPoll = 5 * time.Millisecond
	t.Cleanup(func() { localAdmitPoll = defaultAdmitPoll })
	return inflight.Limits{Total: 24, Override: map[string]int{"magnitude·local": 1}}
}

func TestLocalAdmit(t *testing.T) {
	lim := admitEnv(t)
	const acct = "magnitude·local"
	rel, err := admitLocal(context.Background(), 0, acct, lim)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitLocal(context.Background(), 30*time.Millisecond, acct, lim)
	var ae *LocalAdmitError
	if !errors.As(err, &ae) || ae.Outcome != LocalBusy || ae.Running != 1 || ae.Waited < 30*time.Millisecond {
		t.Fatalf("second caller: %+v", err)
	}
	done := make(chan error, 1)
	go func() {
		r, err := admitLocal(context.Background(), 5*time.Second, acct, lim)
		if err == nil {
			r()
		}
		done <- err
	}()
	rel()
	rel() // idempotent
	if err := <-done; err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestLocalAdmitCancel(t *testing.T) {
	lim := admitEnv(t)
	const acct = "magnitude·local"
	rel, err := admitLocal(context.Background(), 0, acct, lim)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = admitLocal(ctx, time.Minute, acct, lim)
	var ae *LocalAdmitError
	if !errors.As(err, &ae) || ae.Outcome != LocalCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if localWaiting.Load() != 0 {
		t.Fatalf("waiting leaked: %d", localWaiting.Load())
	}
}

func TestLocalAdmitStaleHolder(t *testing.T) {
	lim := admitEnv(t)
	const acct = "magnitude·local"
	dir := inflight.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := dir + "/" + inflight.Prefix + strconv.Itoa(1<<22+7) + "-local.1"
	if err := os.WriteFile(dead, []byte(acct), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := admitLocal(context.Background(), 0, acct, lim)
	if err != nil {
		t.Fatalf("a dead owner's marker must not block: %v", err)
	}
	rel()
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(dead))); err == nil {
		t.Fatal("dead marker not pruned")
	}
}

func TestNoteLocalFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, _, _, ok := LastLocalFallback(""); ok {
		t.Fatal("no note yet")
	}
	NoteLocalFallback("", "rate limited", "dispatch")
	at, kind, surface, ok := LastLocalFallback("")
	if !ok || kind != "rate limited" || surface != "dispatch" || time.Since(at) > time.Minute {
		t.Fatalf("got %v %q %q %v", at, kind, surface, ok)
	}
}

func TestLastLocalFallbackFiltersByProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	NoteLocalFallback("dev", "rate limited", "session")
	if _, _, _, ok := LastLocalFallback("dev"); !ok {
		t.Error("dev session must see its own note")
	}
	if _, _, _, ok := LastLocalFallback("qa"); ok {
		t.Error("qa session must not see dev's note")
	}
}
