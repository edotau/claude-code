package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/vendors"
)

// ghRow is a github (Copilot) profile row body without its login field.
const ghRow = `"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
	`"models":{"opus":"auto","sonnet":"auto","haiku":"auto"}`

// ghProfiles is the overlay's "github" entry: one profile per login, named after it, whose login field is it.
func ghProfiles(logins ...string) string {
	rows := make([]string, len(logins))
	for i, l := range logins {
		rows[i] = `"` + l + `":{` + ghRow + `,"login":"` + l + `"}`
	}
	return `"github":{` + strings.Join(rows, ",") + `}`
}

// pinGitHubProfile overlays a github profile for login (plus extra provider entries) and pins it as COPILOT_PROFILE.
func pinGitHubProfile(t *testing.T, login, extra string) {
	t.Helper()
	if extra != "" {
		extra = "," + extra
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{`+ghProfiles(login)+extra+`}}`)
	setPin(t, providers.PinCopilotProfile, login)
}

// ghLogin makes user a gh login (scratch hosts.yml) pinned as the COPILOT_PROFILE's login, with pct% of credits used.
func ghLogin(t *testing.T, user string, pct float64) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir() // a fake gh answers `gh auth token`, so the pinned login yields a token
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho gho_faketokenfaketokenfaketoken\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cfg := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", cfg)
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(cfg, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	pinGitHubProfile(t, user, "")
	prev := vendors.CopilotUsedPct
	vendors.CopilotUsedPct = func(context.Context, string) (float64, bool) { return pct, true }
	t.Cleanup(func() { vendors.CopilotUsedPct = prev })
}

// The GitHub-login provider bills the pinned account's route state; no pin yields no account.
func TestExplicitAccountCopilotLoginIsPinnedUser(t *testing.T) {
	ghLogin(t, "alice", 10)
	acct, _ := explicitAccount(&fakeRunner{name: "copilot"}, Request{Provider: "personal"})
	if acct != "github·alice" {
		t.Errorf("account = %q, want github·alice", acct)
	}
	setPin(t, providers.PinCopilotProfile, "")
	if acct, _ := explicitAccount(&fakeRunner{name: "copilot"}, Request{Provider: "personal"}); acct != "" {
		t.Errorf("no pin: account = %q, want empty", acct)
	}
}

// An exhausted mark on copilot:<pin> does not pre-skip the explicit leg (the gate falls to Databricks), and a
// quota failure on it is recorded on that account.
func TestExplicitChainCopilotNotPreSkippedAndMarks(t *testing.T) {
	ghLogin(t, "alice", 10)
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	cp := &fakeRunner{name: "copilot", errs: []error{quota}, emptyAnswer: true}
	req := Request{Prompt: "p", Provider: "personal"}
	c := ExplicitChain(cp, req)
	if len(c.Legs) != 1 || c.Legs[0].Account != "github·alice" {
		t.Fatalf("chain = %+v, want one leg on github·alice", c)
	}
	if _, err := RunWithFallback(context.Background(), c, req, io.Discard, nil); err == nil {
		t.Fatal("the quota failure must surface")
	}
	if e := loadState()["github·alice"]; e.Status(time.Now()) != router.StateExhausted {
		t.Fatalf("github·alice not marked: %+v", e)
	}
	again := ExplicitChain(cp, req)
	if len(again.Legs) != 1 || len(again.Skipped) != 0 {
		t.Errorf("exhausted GitHub-login leg must stay in the chain: %+v", again)
	}
}

// vendorTargetNote gates an explicit --provider personal (the GitHub login): kept under 95%, refused to Databricks (or an
// error naming no credit-free route) at/over it — never silently another GitHub leg.
func TestVendorTargetNoteExplicitCopilotGated(t *testing.T) {
	req := Request{Prompt: "p", Provider: "personal"}

	ghLogin(t, "alice", 10)
	tg, own, _, err := vendorTargetNote(req, "copilot")
	if err != nil || own || tg.Provider == nil || tg.Provider.Name != "personal" {
		t.Fatalf("below threshold: %v own=%v err=%v, want personal", tg.Provider, own, err)
	}

	for _, pct := range []float64{95, 99} {
		vendors.CopilotUsedPct = func(context.Context, string) (float64, bool) { return pct, true }
		tg, _, _, err := vendorTargetNote(req, "copilot")
		if err == nil && tg.Provider != nil && tg.Provider.Name == "personal" {
			t.Fatalf("pct %v: explicit GitHub login must not be used past the gate", pct)
		}
		if err != nil && !strings.Contains(err.Error(), "no credit-free route") {
			t.Errorf("pct %v: err = %v, want the no credit-free route reason", pct, err)
		}
	}
}

// fakeCopilotBin installs an executable stand-in at BinDir()/copilot so paths.VendorBin resolves.
func fakeCopilotBin(t *testing.T) {
	t.Helper()
	dir := paths.BinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copilot"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubAsk swaps copilotAsk for fn (restored at cleanup) and returns the recorded call count.
func stubAsk(t *testing.T, fn func(n int) (copilotsdk.Result, error)) *int {
	t.Helper()
	prev, n := copilotAsk, 0
	copilotAsk = func(context.Context, copilotsdk.Options, string) (copilotsdk.Result, error) {
		n++
		return fn(n)
	}
	t.Cleanup(func() { copilotAsk = prev })
	return &n
}

// An exit-0 empty answer from the SDK is a KindFatal emptyAnswer("copilot"), not a success.
func TestCopilotRunnerEmptyAnswerIsFatal(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	stubAsk(t, func(int) (copilotsdk.Result, error) { return copilotsdk.Result{Text: "  \n", SessionID: "s1"}, nil })
	res, err := copilotRunner{}.Run(context.Background(), Request{Prompt: "p", Provider: "personal", Direct: true}, io.Discard)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindFatal || e.Agent != "copilot" || !strings.Contains(e.Err.Error(), "empty answer") {
		t.Fatalf("err = %v, want KindFatal emptyAnswer(copilot)", err)
	}
	if res.SessionID != "s1" {
		t.Errorf("session id must survive the empty answer, got %q", res.SessionID)
	}
}

// A 402 marks the pinned GitHub account and writes a hand-off without rerunning the SDK.
func TestCopilotRunnerQuotaTriggersHandoff(t *testing.T) {
	dir, marked := stubHandoff(t)
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	calls := stubAsk(t, func(n int) (copilotsdk.Result, error) {
		if n != 1 {
			t.Fatal("quota handoff must not rerun the SDK")
		}
		return copilotsdk.Result{Text: "Step 1 done.", SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}, errQuotaMidTask
	})
	res, err := copilotRunner{}.Run(context.Background(), Request{Prompt: "Refactor foo", Model: "opus", Provider: "personal", Direct: true}, io.Discard)
	if !errors.Is(err, errQuotaMidTask) || Classify("copilot", err, false).Kind != KindQuota {
		t.Fatalf("res=%+v err=%v; want the KindQuota cause", res, err)
	}
	if *calls != 1 {
		t.Errorf("SDK calls = %d, want 1", *calls)
	}
	if len(*marked) != 1 || (*marked)[0] != "github·alice" {
		t.Errorf("marked = %v, want github·alice", *marked)
	}
	if filepath.Dir(res.Handoff) != dir {
		t.Fatalf("handoff = %q, want a file under %s", res.Handoff, dir)
	}
	if _, err := os.Stat(res.Handoff); err != nil {
		t.Fatal(err)
	}
}

// setPin seeds (value != "") or clears a vault pin; pins are vault-only, never read from env.
func setPin(t *testing.T, key, value string) {
	t.Helper()
	if err := providers.SetPins(map[string]string{key: value}); err != nil {
		t.Fatal(err)
	}
}

// Partial copilot text followed by a transient error is not retried: one SDK call, the partial answer and session kept.
func TestCopilotPartialAnswerKeptOnTransientError(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	calls := stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "half an answer", SessionID: "sess-9"}, errors.New("send prompt: connection reset")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := Run(ctx, copilotRunner{}, Request{Prompt: "p", Provider: "personal", Direct: true}, io.Discard, nil)
	if err == nil {
		t.Fatal("err = nil, want the SDK failure")
	}
	if *calls != 1 {
		t.Errorf("copilotAsk calls = %d, want 1: output was already produced, a retry would re-run the task", *calls)
	}
	if res.Handoff != "" {
		t.Fatalf("Handoff = %q: this must be the NON-quota path", res.Handoff)
	}
	if res.Answer != "half an answer" {
		t.Errorf("Answer = %q, want the partial text \"half an answer\"", res.Answer)
	}
	if res.SessionID != "sess-9" {
		t.Errorf("SessionID = %q, want \"sess-9\"", res.SessionID)
	}
}

// Copilot reports no token usage, so a successful run's envelope has usage null and fallback null.
func TestCopilotResultJSONUsageNull(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "hello", SessionID: "sess-1", Model: "m"}, nil
	})
	res, err := Run(context.Background(), copilotRunner{}, Request{Prompt: "p", Provider: "personal", Direct: true}, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(NewEnvelope(res, nil, "id-1"))
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["usage"]; !ok || v != nil {
		t.Errorf("usage = %v (present=%v), want explicit null: copilot reports none", v, ok)
	}
	if v, ok := m["fallback"]; !ok || v != nil {
		t.Errorf("fallback = %v, want null when the requested agent answered", m["fallback"])
	}
}
