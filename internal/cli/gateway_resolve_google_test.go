package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gemini"
)

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
	r := withGoogleQuota(context.Background(), agent.ModelRoute{Account: "google·antigravity", Model: "gemini-4-argon", Note: "pinned"}, &warn)
	if want := "held until " + until.Local().Format("2006-01-02 15:04"); !strings.Contains(r.Note, want) || !strings.HasPrefix(r.Note, "pinned; Gemini 4 Argon 0% left") || warn.Len() != 0 {
		t.Errorf("held route: note %q warn %q, want the pool, 0%% and %q", r.Note, warn.String(), want)
	}
	r = withGoogleQuota(context.Background(), agent.ModelRoute{Account: "google·antigravity", Model: "gemini-4-hydrogen"}, &warn)
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
		r := withGoogleQuota(context.Background(), agent.ModelRoute{Account: acct, Model: "gemini-4-hydrogen", Note: "n"}, &warn)
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
	r := withGoogleQuota(context.Background(), agent.ModelRoute{Account: "google·antigravity", Model: "gemini-4-hydrogen"}, &warn)
	if r.Note != "usage unavailable" || warn.Len() != 0 || r.Model != "gemini-4-hydrogen" {
		t.Errorf("route = %+v warn %q, want the route intact and the note %q", r, warn.String(), "usage unavailable")
	}
}
