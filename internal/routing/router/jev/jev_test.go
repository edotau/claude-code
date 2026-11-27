package jev

import (
	_ "github.com/edotau/claude-code/internal/platform/nonet"

	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if got, err := Helper(context.Background(), "rename x"); err != nil || got != "fast" {
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
		reply(`{"answers":{"tier":{"choice":"deep","probabilities":{"deep":0.7}}}}`)(w, r)
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
