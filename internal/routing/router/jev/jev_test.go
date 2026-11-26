package jev

import (
	_ "github.com/edotau/claude-code/internal/platform/nonet"

	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
	serve(t, reply(`{"answers":{"tier":{"type":"choice","choice":"deep","probabilities":{"fast":0.1,"standard":0.2,"deep":0.7},"confidence":0.7}}}`))
	r, err := Tier(context.Background(), "audit it")
	if err != nil || r.Pick != "deep" || r.Confidence != 0.7 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestJevDown(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close() // the test's own port, now closed: nothing else is ever dialed
	t.Setenv(urlEnv, s.URL)
	if _, err := Tier(context.Background(), "x"); Class(err) != ClassDown {
		t.Fatalf("class %q, err %v", Class(err), err)
	}
}

func TestJevTimeout(t *testing.T) {
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
	for _, body := range []string{`nope`, `{"answers":{"tier":{"choice":"huge","probabilities":{"huge":1}}}}`,
		`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":1.5}}}}`, `{"answers":{}}`} {
		serve(t, reply(body))
		if _, err := Tier(context.Background(), "x"); Class(err) != ClassBadResponse {
			t.Errorf("%s: class %q, err %v", body, Class(err), err)
		}
	}
}

func TestJevOff(t *testing.T) {
	t.Setenv(urlEnv, "off")
	if _, err := Tier(context.Background(), "x"); Class(err) != ClassOff {
		t.Fatalf("class %q", Class(err))
	}
}

func TestJevNeeds(t *testing.T) {
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
	serve(t, reply(`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":0.9,"standard":0.1}},"agent":{"choice":"pi","probabilities":{"pi":0.8,"claude":0.2}}}}`))
	r, err := Tier(context.Background(), "rename x")
	if err != nil || r.Agent != "pi" || r.AgentConfidence != 0.8 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestJevBadAgentKeepsTier(t *testing.T) {
	serve(t, reply(`{"answers":{"tier":{"choice":"fast","probabilities":{"fast":0.9}},"agent":{"choice":42}}}`))
	r, err := Tier(context.Background(), "x")
	if err != nil || r.Pick != "fast" || r.Agent != "" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestJevHelper(t *testing.T) {
	s := httptest.NewServer(reply(`{"choices":[{"message":{"content":"{\"tier\":\"fast\"}"}}]}`))
	t.Cleanup(s.Close)
	t.Setenv(qwenEnv, s.URL)
	if got, err := Helper(context.Background(), "rename x"); err != nil || got != "fast" {
		t.Fatalf("got %q, %v", got, err)
	}
}
