package agent

import (
	"context"
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

// ghLogin makes user a gh login (scratch hosts.yml) pinned as COPILOT_GITHUB_USER, with pct% of credits used.
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
	setPin(t, providers.PinCopilotProfile, user)
	prev := vendors.CopilotUsedPct
	vendors.CopilotUsedPct = func(context.Context, string) (float64, bool) { return pct, true }
	t.Cleanup(func() { vendors.CopilotUsedPct = prev })
}

// The GitHub-login provider bills the pinned account's route state; no pin yields no account.
func TestExplicitAccountCopilotLoginIsPinnedUser(t *testing.T) {
	ghLogin(t, "alice", 10)
	acct, _ := explicitAccount(&fakeRunner{name: "copilot"}, Request{Provider: "github-copilot"})
	if acct != "github·alice" {
		t.Errorf("account = %q, want github·alice", acct)
	}
	setPin(t, providers.PinCopilotProfile, "")
	if acct, _ := explicitAccount(&fakeRunner{name: "copilot"}, Request{Provider: "github-copilot"}); acct != "" {
		t.Errorf("no pin: account = %q, want empty", acct)
	}
}

// An exhausted mark on copilot:<pin> does not pre-skip the explicit leg (the gate falls to Databricks), and a
// quota failure on it is recorded on that account.
func TestExplicitChainCopilotNotPreSkippedAndMarks(t *testing.T) {
	ghLogin(t, "alice", 10)
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	cp := &fakeRunner{name: "copilot", errs: []error{quota}, emptyAnswer: true}
	req := Request{Prompt: "p", Provider: "github-copilot"}
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

// vendorTargetNote gates an explicit --provider github-copilot: kept under 95%, refused to Databricks (or an
// error naming no credit-free route) at/over it — never silently another GitHub leg.
func TestVendorTargetNoteExplicitCopilotGated(t *testing.T) {
	req := Request{Prompt: "p", Provider: "github-copilot"}

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
	res, err := copilotRunner{}.Run(context.Background(), Request{Prompt: "p", Provider: "github-copilot", Direct: true}, io.Discard)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindFatal || e.Agent != "copilot" || !strings.Contains(e.Err.Error(), "empty answer") {
		t.Fatalf("err = %v, want KindFatal emptyAnswer(copilot)", err)
	}
	if res.SessionID != "s1" {
		t.Errorf("session id must survive the empty answer, got %q", res.SessionID)
	}
}

// A 402 on the GitHub-login target runs the hand-off path: it marks copilot:<pin>, writes the hand-off and resumes once on Databricks.
func TestCopilotRunnerQuotaTriggersHandoff(t *testing.T) {
	dir, marked := stubHandoff(t, func(*providers.Registry, string) (providers.Target, string, error) {
		return providers.Target{Provider: &providers.Provider{Name: "dev"}, Model: "databricks-claude-opus-5-5"}, "using dev", nil
	})
	ghLogin(t, "alice", 10) // after stubHandoff: pins alice as the workable gh login
	t.Setenv("DEV_TEST_KEY", "k")
	vaulttest.UseKeyring(t)
	setPin(t, providers.PinCopilotProfile, "alice") // UseKeyring relocates the vault; reseed the pin
	fakeCopilotBin(t)                               // after the vault helper, which may relocate the config dir
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"dev":{"kind":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5"}}}}`)
	calls := stubAsk(t, func(n int) (copilotsdk.Result, error) {
		if n == 1 {
			return copilotsdk.Result{Text: "Step 1 done.", SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}, errQuotaMidTask
		}
		return copilotsdk.Result{Text: "finished on dev"}, nil
	})
	var out strings.Builder
	res, err := copilotRunner{}.Run(context.Background(), Request{Prompt: "Refactor foo", Model: "opus", Provider: "github-copilot", Direct: true}, &out)
	if err != nil || res.Answer != "finished on dev" || res.Provider != "dev" {
		t.Fatalf("res=%+v err=%v; want the resumed answer from dev", res, err)
	}
	if *calls != 2 {
		t.Errorf("SDK calls = %d, want 2 (GitHub leg, then one resume)", *calls)
	}
	if len(*marked) != 1 || (*marked)[0] != "github·alice" {
		t.Errorf("marked = %v, want github·alice", *marked)
	}
	if !strings.HasPrefix(res.Handoff, dir) {
		t.Errorf("handoff = %q, want a file under %s", res.Handoff, dir)
	}
}

// setPin seeds (value != "") or clears a vault pin; pins are vault-only, never read from env.
func setPin(t *testing.T, key, value string) {
	t.Helper()
	if err := providers.SetPins(map[string]string{key: value}); err != nil {
		t.Fatal(err)
	}
}
