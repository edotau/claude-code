package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRouteLine(t *testing.T) {
	r := agent.ModelRoute{Agent: "copilot", Account: "github·business", Model: "gpt-6-astra", Source: "prefix"}
	if got := r.Line(); got != "copilot · github·business · gpt-6-astra (prefix)" {
		t.Errorf("routeLine = %q", got)
	}
}

func TestGatewayResolveUsage(t *testing.T) {
	if code := cmdGatewayResolve([]string{"claude", "--live"}); code != 2 {
		t.Errorf("--live without --all-profiles exit = %d; want 2", code)
	}
}

// --fallback outside router.FallbackModes is a usage error like the sibling verbs' check, never treated as "not none".
func TestGatewayResolveRejectsUnknownFallback(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdGatewayResolve([]string{"opencode", "--model", "dev:x", "--fallback", "bogus"})
	})
	if code != 2 || !strings.Contains(stderr, `--fallback "bogus" unknown (none|pin|agents|all)`) {
		t.Errorf("exit %d, stderr %q; want exit 2 naming the closed set", code, stderr)
	}
}

// An auto reason in the middle of the note leaves the notes around it joined by one "; ", never an empty part.
func TestPrintRouteAutoReasonMidNote(t *testing.T) {
	choice := &agent.AutoChoice{Tier: "standard", Criterion: "value", Signal: "standard", Model: "m", Fallback: "no ranking"}
	r := agent.ModelRoute{Agent: "opencode", Account: "a", Model: "m", Source: "flag", Note: "first; " + choice.Reason() + "; detail"}
	_, out, _ := hosttest.CaptureOutput(t, func() int { return printRoute(r, choice, "text") })
	if !strings.Contains(out, "note: first; detail\n") {
		t.Errorf("printRoute = %q; want note \"first; detail\"", out)
	}
}

// An untyped resolve reports the hop RunQuery picks (the label's resolver), its note kept; an error is returned.
func TestResolveFirstHop(t *testing.T) {
	base, err := os.ReadFile("../providers/defaults.json")
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
	old := resolveRunQuery
	t.Cleanup(func() { resolveRunQuery = old })
	resolveRunQuery = stub
	r, _, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, "fix the typo")
	if err != nil || got.Agent != "codex" || hint != "haiku" || r.Agent != "claude" || r.Model != "claude-sonnet-5-5" || r.Account != "anthropic·ultra" || !strings.Contains(r.Note, "gated out") {
		t.Errorf("resolveFirstHop = %+v, %v (query %+v); want the stubbed claude sonnet hop on anthropic·ultra with its note", r, err, got)
	}
	resolveRunQuery = func(context.Context, *providers.Registry, agent.ModelQuery, string) (agent.ModelQuery, string, error) {
		return agent.ModelQuery{}, "", errors.New("no route runs codex")
	}
	if _, _, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, ""); err == nil {
		t.Error("resolveFirstHop swallowed the ladder error")
	}
}

// --task resolves an untyped model to the brief's tier, the same pick the run and the label make.
func TestGatewayResolveTaskTier(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	dir := t.TempDir()
	out := map[string]string{}
	for name, brief := range map[string]string{"fast": "Fix the typo in a.go.", "deep": "Audit the safety-scan rules."} {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(brief), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return cmdGatewayResolve([]string{"opencode", "--profile", "dev", "--task", f})
		})
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, stderr)
		}
		out[name] = stdout
	}
	if out["fast"] == out["deep"] {
		t.Errorf("a typo fix and an audit resolve the same: %q", out["fast"])
	}
}

// An untyped tier pick prints the concrete model and one reason line; json carries at most five ranked candidates.
func TestGatewayResolveAuto(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	s := router.Served{Profile: "dev", FetchedAt: time.Now()}
	for _, id := range []string{"opus-5-5", "sonnet-5-5", "haiku-5-5", "opus-5", "sonnet-5", "sonnet-4-6", "opus-4-8"} {
		s.Models = append(s.Models, router.ServedModel{ID: "databricks-claude-" + id, Task: "llm/v1/chat", Ready: "READY"})
	}
	if err := router.SaveServed(s); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "brief")
	if err := os.WriteFile(f, []byte("Add a --json flag to agents status in a.go and b.go."), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"claude", "--profile", "dev", "--task", f}
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdGatewayResolve(args) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || strings.Contains(lines[0], " auto ") || !strings.HasPrefix(lines[1], "auto: standard (") {
		t.Fatalf("exit %d: %q (%s), want the route line then one auto: reason line", code, out, stderr)
	}
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdGatewayResolve(append(args, "--format", "json")) })
	var doc struct {
		Auto struct{ Ranked []json.RawMessage } `json:"auto"`
	}
	if err := json.Unmarshal([]byte(out), &doc); code != 0 || err != nil || len(doc.Auto.Ranked) == 0 || len(doc.Auto.Ranked) > 5 {
		t.Errorf("json exit %d (%v): %d ranked, want 1..5: %s", code, err, len(doc.Auto.Ranked), out)
	}
}

// A model dev's fresh listing lacks prints "served: no"; under --fallback none it exits 2 naming the served ones.
func TestGatewayResolveUnservedModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-gemini-3-8-flash", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		extra    string
		code     int
		out, err string
	}{
		{"", 0, "served: no", ""},
		{"none", 2, "", "databricks-gemini-3-8-flash"},
	} {
		args := []string{"gemini", "--profile", "dev", "--model", "gemini-4-argon-high"}
		if tc.extra != "" {
			args = append(args, "--fallback", tc.extra)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdGatewayResolve(args) })
		if code != tc.code || !strings.Contains(stdout, tc.out) || !strings.Contains(stderr, tc.err) {
			t.Errorf("--fallback %q: exit %d, stdout %q, stderr %q; want exit %d with %q / %q", tc.extra, code, stdout, stderr, tc.code, tc.out, tc.err)
		}
	}
}

// An unverified --all-profiles row prints served: unverified once, on its route line.
func TestResolveAllUnverifiedOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"auth":{"type":"oauth"},
		"host":"https://dev.cloud.databricks.com","preset":"databricks"}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := resolveAll(context.Background(), reg, agent.ModelQuery{Agent: "opencode", Model: "opus"}, false)
	_, out, _ := hosttest.CaptureOutput(t, func() int { return printRows(rows, "text") })
	if n := strings.Count(out, "unverified"); !strings.Contains(out, "dev") || n != 1 {
		t.Errorf("printRows = %q; want the dev row with served: unverified once (got %d)", out, n)
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

// The pool table sorts agent, row, tier, omits a pair the row cannot run and fits 80 columns on a terminal.
func TestPoolTable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := poolRows(context.Background(), reg, []string{"fast", "deep"}, false, "", time.Now())
	seen := map[string]poolRow{}
	var keys []string
	for _, r := range rows {
		seen[r.Agent+" "+r.Row+" "+r.Tier] = r
		keys = append(keys, r.Agent+" "+r.Row+" "+map[string]string{"fast": "0", "deep": "1"}[r.Tier])
	}
	if !slices.IsSorted(keys) {
		t.Errorf("rows not sorted by agent, row, tier: %q", keys)
	}
	_, copilot := seen["copilot github·copilot fast"]
	if _, codex := seen["codex github·copilot fast"]; !copilot || codex {
		t.Errorf("copilot row %v, codex row %v on the copilot login; want only copilot: %v", copilot, codex,
			slices.Collect(maps.Keys(seen)))
	}
	claude := seen["claude databricks·dev deep"]
	if got := [3]any{claude.Size, claude.Next != "", claude.Note}; got != [3]any{0, true, "does not rotate"} {
		t.Errorf("claude on dev = %+v; want its tier model and \"does not rotate\"", claude)
	}
	var out bytes.Buffer
	renderPoolTable(&out, rows, 80, time.Now())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(rows)+1 {
		t.Errorf("%d lines for %d rows; want a header plus one per row", len(lines), len(rows))
	}
	for _, l := range lines {
		if n := textutil.DisplayWidth(l); n > 80 {
			t.Errorf("line is %d columns, want at most 80: %q", n, l)
		}
	}
}
