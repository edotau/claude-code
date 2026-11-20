package resolve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubRefreshListingForTest()
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}

// An untyped resolve reports the hop RunQuery picks (the label's resolver), its note kept; an error is returned.
func TestResolveFirstHop(t *testing.T) {
	base, err := os.ReadFile("../../providers/defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Parse(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got agent.ModelQuery
	hint := ""
	stub := func(_ context.Context, _ *providers.Registry, q agent.ModelQuery, alias string) (agent.ModelQuery, string, error) {
		got, hint = q, alias
		return agent.ModelQuery{Agent: "claude", Model: "ultra:claude-sonnet-5-5"}, "codex is gated out", nil
	}
	old := runQuery
	t.Cleanup(func() { runQuery = old })
	runQuery = stub
	r, _, err := FirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, "fix the typo")
	if err != nil || got.Agent != "codex" || hint != "haiku" {
		t.Fatalf("FirstHop = %v (query %+v, hint %q); want the codex query with the haiku hint", err, got, hint)
	}
	if want := "claude claude-sonnet-5-5 anthropic·ultra"; r.Agent+" "+r.Model+" "+r.Account != want || !strings.Contains(r.Note, "gated out") {
		t.Errorf("FirstHop = %+v; want the stubbed hop %q with its note", r, want)
	}
	runQuery = func(context.Context, *providers.Registry, agent.ModelQuery, string) (agent.ModelQuery, string, error) {
		return agent.ModelQuery{}, "", errors.New("no route runs codex")
	}
	if _, _, err := FirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, ""); err == nil {
		t.Error("FirstHop swallowed the ladder error")
	}
}

func stubGoogleQuota(t *testing.T, fn func(context.Context, string, time.Time) (gemini.ModelQuota, error)) *int {
	t.Helper()
	calls := 0
	old := googleQuotaCheck
	googleQuotaCheck = func(ctx context.Context, m string, now time.Time) (gemini.ModelQuota, error) {
		calls++
		return fn(ctx, m, now)
	}
	t.Cleanup(func() { googleQuotaCheck = old })
	return &calls
}

func TestWithGoogleQuotaNotesPoolAndHold(t *testing.T) {
	until := time.Date(2026, 11, 23, 3, 35, 47, 0, time.UTC)
	low := gemini.QuotaRow{Pool: "Gemini Models", Limit: "Five Hour Limit Remaining", Remaining: 9}
	calls := stubGoogleQuota(t, func(_ context.Context, m string, _ time.Time) (gemini.ModelQuota, error) {
		if m == "gemini-4-argon" {
			return gemini.ModelQuota{Pool: "Gemini 4 Argon", Left: 0, HeldUntil: until}, nil
		}
		return gemini.ModelQuota{Pool: "Gemini Models", Left: 9, Low: &low}, nil
	})
	var warn bytes.Buffer
	r := WithGoogleQuota(context.Background(), agent.ModelRoute{Account: "google·antigravity", Model: "gemini-4-argon", Note: "pinned"}, &warn)
	if want := "held until " + until.Local().Format("2006-01-02 15:04"); !strings.Contains(r.Note, want) || !strings.HasPrefix(r.Note, "pinned; Gemini 4 Argon 0% left") || warn.Len() != 0 {
		t.Errorf("held route: note %q warn %q, want the pool, 0%% and %q", r.Note, warn.String(), want)
	}
	r = WithGoogleQuota(context.Background(), agent.ModelRoute{Account: "google·antigravity", Model: "gemini-4-hydrogen"}, &warn)
	if w := warn.String(); r.Note != "Gemini Models 9% left" || !strings.HasPrefix(w, "warning:") || strings.Count(w, "\n") != 1 {
		t.Errorf("low route: note %q warn %q, want 9%% and a one-line warning", r.Note, w)
	}
	if *calls != 2 {
		t.Errorf("usage read %d times, want 2", *calls)
	}
}

func TestWithGoogleQuotaSkipsOtherRoutes(t *testing.T) {
	calls := stubGoogleQuota(t, func(context.Context, string, time.Time) (gemini.ModelQuota, error) {
		return gemini.ModelQuota{}, errors.New("must not run")
	})
	for _, acct := range []string{"databricks·qa", "anthropic·ultra", ""} {
		var warn bytes.Buffer
		r := WithGoogleQuota(context.Background(), agent.ModelRoute{Account: acct, Model: "gemini-4-hydrogen", Note: "n"}, &warn)
		if r.Note != "n" || warn.Len() != 0 {
			t.Errorf("%q: note %q warn %q, want the route unchanged", acct, r.Note, warn.String())
		}
	}
	if *calls != 0 {
		t.Errorf("usage read %d times on non-Google routes, want 0", *calls)
	}
}

func TestWithGoogleQuotaFailureNeverFailsResolve(t *testing.T) {
	stubGoogleQuota(t, func(context.Context, string, time.Time) (gemini.ModelQuota, error) {
		return gemini.ModelQuota{}, errors.New("agy usage unavailable")
	})
	var warn bytes.Buffer
	r := WithGoogleQuota(context.Background(), agent.ModelRoute{Account: "google·antigravity", Model: "gemini-4-hydrogen"}, &warn)
	if r.Note != "usage unavailable" || warn.Len() != 0 || r.Model != "gemini-4-hydrogen" {
		t.Errorf("route = %+v warn %q, want the route intact and the note %q", r, warn.String(), "usage unavailable")
	}
}
