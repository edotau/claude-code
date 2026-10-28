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
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// stubHandoff points the hand-off seams at a temp dir and a recorded markExhausted; fb is the fallback stub.
func stubHandoff(t *testing.T, fb func(*providers.Registry, string) (providers.Target, string, error)) (string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pinGitHubProfile(t, "edotau", "")
	var marked []string
	oDir, oFb, oLoad, oMark := handoffDir, copilotFallback, loadState, markExhausted
	handoffDir = func(string) string { return dir }
	copilotFallback = fb
	loadState = func() router.State { return router.State{} }
	markExhausted = func(acct string, _, _ time.Time, _ string) error { marked = append(marked, acct); return nil }
	t.Cleanup(func() { handoffDir, copilotFallback, loadState, markExhausted = oDir, oFb, oLoad, oMark })
	return dir, &marked
}

var errQuotaMidTask = errors.New("send prompt: session error: 402 You have exceeded your monthly quota of premium requests")

func quotaReq() (Request, Result, copilotsdk.Result) {
	req := Request{Prompt: "Refactor foo; token ghp_" + strings.Repeat("a", 36), Model: "opus", WorkDir: "/tmp/repo"}
	res := Result{Agent: "copilot", Provider: "personal", Model: "claude-opus-4.5"}
	return req, res, copilotsdk.Result{Text: "Step 1 done.", SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}
}

func TestCopilotQuotaHandoffResumesOnceOnDatabricks(t *testing.T) {
	dir, marked := stubHandoff(t, func(*providers.Registry, string) (providers.Target, string, error) {
		return providers.Target{Provider: &providers.Provider{Name: "dev"}, Model: "databricks-claude-opus-5-5"},
			"copilot: edotau quota exhausted mid-task — using databricks profile dev", nil
	})
	if Classify("copilot", errQuotaMidTask, false).Kind != KindQuota {
		t.Fatal("stub error must classify as KindQuota")
	}
	req, res, partial := quotaReq()
	var reruns []Request
	rerun := func(_ context.Context, r Request, _ io.Writer) (Result, error) {
		reruns = append(reruns, r)
		return Result{Agent: "copilot", Provider: r.Provider, Answer: "done"}, nil
	}
	out, err := copilotQuotaHandoff(context.Background(), req, res, partial, errQuotaMidTask, nil, rerun, io.Discard)
	if err != nil || out.Answer != "done" || out.Provider != "dev" {
		t.Fatalf("resume = %+v, %v; want dev's answer", out, err)
	}
	if len(reruns) != 1 || reruns[0].Provider != "dev" || reruns[0].Model != "opus" || reruns[0].SessionID != partial.SessionID {
		t.Fatalf("reruns = %+v; want exactly one on dev/opus resuming the same session", reruns)
	}
	if len(*marked) != 1 || (*marked)[0] != "github·edotau" {
		t.Errorf("marked = %v; want github·edotau", *marked)
	}
	want := dir + "/" + time.Now().Format("2006-01-02") + "-copilot-0a1b2c3d.md"
	if out.Handoff != want {
		t.Fatalf("handoff = %q, want %q", out.Handoff, want)
	}
	body, _ := os.ReadFile(want)
	t.Logf("handoff file:\n%s", body)
	for _, s := range []string{"Refactor foo", "Step 1 done.", "copilot --resume 0a1b2c3d", "account: github·edotau", "402"} {
		if !strings.Contains(string(body), s) {
			t.Errorf("handoff lacks %q", s)
		}
	}
	if strings.Contains(string(body), "ghp_aaaa") || !strings.Contains(reruns[0].Prompt, "Step 1 done.") {
		t.Error("handoff must be redacted and ride the resumed prompt")
	}
}

func TestCopilotQuotaHandoffWithoutFallbackReturnsQuota(t *testing.T) {
	_, _ = stubHandoff(t, func(*providers.Registry, string) (providers.Target, string, error) {
		return providers.Target{}, "", errors.New("no credit-free route for copilot")
	})
	req, res, partial := quotaReq()
	rerun := func(context.Context, Request, io.Writer) (Result, error) {
		t.Fatal("no fallback: the leg must not rerun")
		return Result{}, nil
	}
	out, err := copilotQuotaHandoff(context.Background(), req, res, partial, errQuotaMidTask, nil, rerun, io.Discard)
	if !errors.Is(err, errQuotaMidTask) || Classify("copilot", err, false).Kind != KindQuota {
		t.Fatalf("err = %v; want the KindQuota cause", err)
	}
	if out.Handoff == "" || out.SessionID != partial.SessionID {
		t.Fatalf("result = %+v; want the handoff path and the session pointer", out)
	}
	if _, serr := os.Stat(out.Handoff); serr != nil {
		t.Fatalf("handoff file: %v", serr)
	}
}

func TestCopilotQuotaHandoffNeverResumesOnGitHubLogin(t *testing.T) {
	_, _ = stubHandoff(t, func(*providers.Registry, string) (providers.Target, string, error) {
		return providers.Target{Provider: &providers.Provider{Name: "personal", Provider: "github",
			Auth: providers.Auth{Login: "copilot"}}}, "", nil
	})
	req, res, partial := quotaReq()
	rerun := func(context.Context, Request, io.Writer) (Result, error) {
		t.Fatal("a GitHub-login fallback must not rerun")
		return Result{}, nil
	}
	if _, err := copilotQuotaHandoff(context.Background(), req, res, partial, errQuotaMidTask, nil, rerun, io.Discard); err == nil {
		t.Fatal("want the quota error back")
	}
}

// "<profile>:<tier>" resolves like --provider <profile> --model <tier>; anything else is left alone.
func TestResolveTargetProfileTierRewrite(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"kind":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5","haiku":"claude-haiku-4-5"}}}}}`)
	_, want, err := resolveTarget(Request{Provider: "dev", Model: "opus"})
	if err != nil || want.Provider == nil || want.Provider.Name != "dev" {
		t.Fatalf("baseline --provider dev --model opus = %+v, %v", want, err)
	}

	t.Run("dev:opus equals --provider dev --model opus", func(t *testing.T) {
		_, got, err := resolveTarget(Request{Model: "dev:opus"})
		if err != nil || got.Provider == nil || got.Provider.Name != "dev" || got.Model != want.Model {
			t.Errorf("dev:opus = %+v (%v), want provider dev model %q", got, err, want.Model)
		}
	})
	t.Run("full id keeps its prior behaviour", func(t *testing.T) {
		_, viaSpec, err1 := resolveTarget(Request{Model: "dev:claude-opus-5-5"})
		_, viaFlags, err2 := resolveTarget(Request{Provider: "dev", Model: "claude-opus-5-5"})
		if err1 != nil || err2 != nil || viaSpec.Provider == nil || viaSpec.Provider.Name != "dev" ||
			viaSpec.Model != viaFlags.Model {
			t.Errorf("spec %+v (%v) vs flags %+v (%v)", viaSpec, err1, viaFlags, err2)
		}
	})
	t.Run("unknown row or non-tier is not rewritten", func(t *testing.T) {
		_, bad, err := resolveTarget(Request{Model: "unknownrow:opus"})
		if err == nil && bad.Provider != nil && bad.Provider.Name == "unknownrow" {
			t.Errorf("unknownrow:opus rewritten: %+v", bad)
		}
		_, nt, _ := resolveTarget(Request{Model: "dev:notatier"})
		if nt.Provider != nil && nt.Provider.Name == "dev" && nt.Model == want.Model {
			t.Errorf("dev:notatier rewritten to dev's opus tier: %+v", nt)
		}
	})
	t.Run("the explicit backend is still seen", func(t *testing.T) {
		req := Request{Model: "dev:opus"}
		if !ExplicitBackend(req) {
			t.Error("ExplicitBackend(dev:opus) must stay true")
		}
		if acct, _ := explicitAccount(&fakeRunner{name: "claude"}, req); acct != "databricks·dev" {
			t.Errorf("explicitAccount = %q, want databricks·dev", acct)
		}
	})
}

// handoffRunner returns a canned Result and error per call, so a leg can carry a hand-off path.
type handoffRunner struct {
	name string
	res  Result
	err  error
}

func (h *handoffRunner) Name() string     { return h.name }
func (h *handoffRunner) Caps() Caps       { return Caps{} }
func (h *handoffRunner) Available() error { return nil }
func (h *handoffRunner) Run(context.Context, Request, io.Writer) (Result, error) {
	return h.res, h.err
}

// A hand-off written by leg 1 rides the final envelope whichever way the later legs end.
func TestRunWithFallbackCarriesHandoff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	first := func() *handoffRunner {
		return &handoffRunner{name: "copilot", res: Result{Handoff: "/tmp/x.md"}, err: quota}
	}

	t.Run("leg 2 succeeds", func(t *testing.T) {
		second := &handoffRunner{name: "claude", res: Result{Answer: "from leg 2"}}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{first(), second}, "", "sonnet"), Request{Prompt: "p"}, io.Discard, nil)
		if err != nil || res.Answer != "from leg 2" || res.Handoff != "/tmp/x.md" {
			t.Fatalf("res=%+v err=%v; want leg 2's answer with handoff /tmp/x.md", res, err)
		}
	})

	t.Run("leg 2 fails with another error", func(t *testing.T) {
		boom := errors.New("boom: unrelated failure")
		second := &handoffRunner{name: "claude", err: boom}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{first(), second}, "", "sonnet"), Request{Prompt: "p"}, io.Discard, nil)
		if err == nil || !errors.Is(err, boom) || res.Handoff != "/tmp/x.md" {
			t.Fatalf("res=%+v err=%v; want leg 2's error with handoff /tmp/x.md", res, err)
		}
	})

	t.Run("a later leg's own handoff wins", func(t *testing.T) {
		second := &handoffRunner{name: "claude", res: Result{Answer: "ok", Handoff: "/tmp/y.md"}}
		res, _ := RunWithFallback(context.Background(), chainOf([]Runner{first(), second}, "", "sonnet"), Request{Prompt: "p"}, io.Discard, nil)
		if res.Handoff != "/tmp/y.md" {
			t.Errorf("handoff = %q, want the later leg's /tmp/y.md", res.Handoff)
		}
	})
}

// tok builds a GitHub-shaped token for prefix, so no real secret sits in the source.
func tok(prefix string) string { return prefix + strings.Repeat("a1", 18) }

// dbxFallback is a copilotFallback stub resolving to the dev Databricks profile.
func dbxFallback(*providers.Registry, string) (providers.Target, string, error) {
	return providers.Target{Provider: &providers.Provider{Name: "dev"}, Model: "databricks-claude-sonnet-5-5"}, "using dev", nil
}

// (1) The hand-off file and the resume prompt never hold a seeded raw token, GitHub-shaped or a plain resolved credential.
func TestCopilotHandoffRedactsEveryToken(t *testing.T) {
	_, _ = stubHandoff(t, dbxFallback)
	const plain = "plainsecretvalue-not-shaped-123"
	var all []string
	for _, p := range []string{"ghu_", "gho_", "ghs_", "ghr_", "ghp_", "github_pat_"} {
		all = append(all, tok(p))
	}
	req, res, partial := quotaReq()
	req.Prompt = "Task with " + strings.Join(all, " ") + " and " + plain
	partial.Text = "partial with " + all[2] + " and " + plain
	var resumed Request
	rerun := func(_ context.Context, r Request, _ io.Writer) (Result, error) {
		resumed = r
		return Result{Answer: "ok"}, nil
	}
	out, err := copilotQuotaHandoff(context.Background(), req, res, partial,
		errors.New("402 quota; token "+plain), []string{plain}, rerun, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	body, rerr := os.ReadFile(out.Handoff)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, secret := range append(all, plain) {
		if strings.Contains(string(body), secret) {
			t.Errorf("handoff file holds %q", secret)
		}
		if strings.Contains(resumed.Prompt, secret) {
			t.Errorf("resume prompt holds %q", secret)
		}
	}
	if !strings.Contains(resumed.Prompt, "REDACTED") {
		t.Error("resume prompt should carry the redacted handoff")
	}
}

// (1, runner level) The prompt the stubbed SDK receives on the resume is redacted too.
func TestCopilotRunnerResumePromptRedacted(t *testing.T) {
	_, _ = stubHandoff(t, dbxFallback)
	ghLogin(t, "alice", 10)
	t.Setenv("DEV_TEST_KEY", "k")
	vaulttest.UseKeyring(t)
	fakeCopilotBin(t)
	// UseKeyring relocates the vault; reseed the alice profile and its pin alongside dev
	pinGitHubProfile(t, "alice", `"databricks":{"dev":{"kind":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5"}}}`)
	secret := tok("ghs_")
	var prompts []string
	prev := copilotAsk
	copilotAsk = func(_ context.Context, _ copilotsdk.Options, p string) (copilotsdk.Result, error) {
		prompts = append(prompts, p)
		if len(prompts) == 1 {
			return copilotsdk.Result{Text: "half " + secret, SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}, errQuotaMidTask
		}
		return copilotsdk.Result{Text: "done"}, nil
	}
	t.Cleanup(func() { copilotAsk = prev })
	res, err := copilotRunner{}.Run(context.Background(), Request{Prompt: "do it " + secret, Model: "sonnet", Provider: "personal", Direct: true}, io.Discard)
	if err != nil || len(prompts) != 2 {
		t.Fatalf("res=%+v err=%v prompts=%d", res, err, len(prompts))
	}
	if strings.Contains(prompts[1], secret) || !strings.Contains(prompts[1], "HANDOFF") {
		t.Errorf("resume prompt = %q", prompts[1])
	}
	if b, _ := os.ReadFile(res.Handoff); strings.Contains(string(b), secret) {
		t.Error("handoff file holds the raw token")
	}
}

// (2, 3) The resume keeps the requested tier and reuses the session (fresh only when none was exposed).
func TestCopilotHandoffResumeKeepsTierAndSession(t *testing.T) {
	for in, want := range map[string]string{"sonnet": "sonnet", "dev:haiku": "haiku", "claude-sonnet-5-5": "sonnet", "gpt-5": ""} {
		if got := resumeSlot(in); got != want {
			t.Errorf("resumeSlot(%q) = %q, want %q", in, got, want)
		}
	}
	_, _ = stubHandoff(t, dbxFallback)
	for name, session := range map[string]string{"with session": "0a1b2c3d-4e5f-6789-abcd-ef0123456789", "no session": ""} {
		t.Run(name, func(t *testing.T) {
			req, res, partial := quotaReq()
			req.Model, partial.SessionID, res.SessionID = "sonnet", session, ""
			var got Request
			rerun := func(_ context.Context, r Request, _ io.Writer) (Result, error) { got = r; return Result{}, nil }
			if _, err := copilotQuotaHandoff(context.Background(), req, res, partial, errQuotaMidTask, nil, rerun, io.Discard); err != nil {
				t.Fatal(err)
			}
			if got.Model != "sonnet" || got.Provider != "dev" || got.SessionID != session || !strings.Contains(got.Prompt, "Step 1 done.") {
				t.Errorf("resume request = %+v", got)
			}
		})
	}
}

// (2) `--provider personal --model sonnet` over the gate lands on the fallback's sonnet model.
func TestVendorTargetNoteGatedKeepsSonnetTier(t *testing.T) {
	ghLogin(t, "alice", 99)
	t.Setenv("DEV_TEST_KEY", "k")
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}}}}}`)
	tg, _, _, err := vendorTargetNote(Request{Prompt: "p", Provider: "personal", Model: "sonnet"}, "copilot")
	if err != nil || tg.Provider == nil || tg.Provider.Name != "dev" || tg.Model != "databricks-claude-sonnet-5-5" {
		t.Fatalf("target = %+v err=%v, want dev / databricks-claude-sonnet-5-5", tg, err)
	}
}

// (4) A leg that wrote a hand-off hands the next leg the hand-off, not the bare task; an unreadable file changes nothing.
func TestRunWithFallbackNextLegGetsHandoff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	file := filepath.Join(t.TempDir(), "h.md")
	if err := os.WriteFile(file, []byte("PARTIAL-PROGRESS-MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"readable": file, "unreadable": file + ".missing"} {
		t.Run(name, func(t *testing.T) {
			second := &fakeRunner{name: "claude", errs: []error{nil}}
			first := &handoffRunner{name: "copilot", res: Result{Handoff: path}, err: quota}
			if _, err := RunWithFallback(context.Background(), chainOf([]Runner{first, second}, "", "sonnet"), Request{Prompt: "BARE-TASK"}, io.Discard, nil); err != nil {
				t.Fatal(err)
			}
			got := second.seen[0].Prompt
			if name == "readable" && (!strings.Contains(got, "PARTIAL-PROGRESS-MARKER") || strings.Contains(got, "BARE-TASK")) {
				t.Errorf("next leg prompt = %q, want the hand-off instead of the task", got)
			}
			if name == "unreadable" && got != "BARE-TASK" {
				t.Errorf("unreadable hand-off must leave the prompt alone, got %q", got)
			}
		})
	}
}

// (5) A providers.Load failure in the hand-off path keeps the quota cause and the hand-off, calls no fallback, never panics.
func TestCopilotHandoffProvidersLoadFailure(t *testing.T) {
	_, _ = stubHandoff(t, func(*providers.Registry, string) (providers.Target, string, error) {
		t.Fatal("fallback must not run when providers.Load fails")
		return providers.Target{}, "", nil
	})
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	if _, err := providers.Load(); err == nil {
		t.Skip("a corrupt overlay no longer fails providers.Load; pick another failure injection")
	}
	req, res, partial := quotaReq()
	rerun := func(context.Context, Request, io.Writer) (Result, error) {
		t.Fatal("must not rerun")
		return Result{}, nil
	}
	out, err := copilotQuotaHandoff(context.Background(), req, res, partial, errQuotaMidTask, nil, rerun, io.Discard)
	if !errors.Is(err, errQuotaMidTask) || Classify("copilot", err, false).Kind != KindQuota || out.Handoff == "" {
		t.Fatalf("out=%+v err=%v; want KindQuota cause plus the handoff", out, err)
	}
}

// (7) Hand-offs with the same date and session id get distinct files and none is truncated.
func TestCopilotHandoffsNeverOverwrite(t *testing.T) {
	dir, _ := stubHandoff(t, dbxFallback)
	req, res, _ := quotaReq()
	res.SessionID = "0a1b2c3d-4e5f-6789-abcd-ef0123456789"
	seen := map[string]string{}
	for i, task := range []string{"first task body", "second task body", "third task body"} {
		req.Prompt = task
		path, _, err := writeCopilotHandoff(req, res, "partial", "github·alice", errQuotaMidTask, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[path]; dup || filepath.Dir(path) != dir {
			t.Fatalf("round %d: path %q duplicated or outside %s", i, path, dir)
		}
		seen[path] = task
	}
	if len(seen) != 3 {
		t.Fatalf("files = %v", seen)
	}
	for path, task := range seen {
		if b, _ := os.ReadFile(path); !strings.Contains(string(b), task) {
			t.Errorf("%s lost its content: %q", path, b)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".handoff-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
