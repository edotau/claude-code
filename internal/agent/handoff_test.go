package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// stubHandoff points the hand-off seams at a temp dir and a recorded markExhausted.
func stubHandoff(t *testing.T) (string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pinGitHubProfile(t, "edotau", "")
	var marked []string
	oDir, oLoad, oMark := handoffDir, loadState, markExhausted
	handoffDir = func(string) string { return dir }
	loadState = func() router.State { return router.State{} }
	markExhausted = func(acct string, _, _ time.Time, _ string) error { marked = append(marked, acct); return nil }
	t.Cleanup(func() { handoffDir, loadState, markExhausted = oDir, oLoad, oMark })
	return dir, &marked
}

var errQuotaMidTask = errors.New("send prompt: session error: 402 You have exceeded your monthly quota of premium requests")

func quotaReq() (RunOptions, Result, copilotsdk.Result) {
	req := RunOptions{Prompt: "Refactor foo; token ghp_" + strings.Repeat("a", 36), Model: "opus", WorkDir: "/tmp/repo"}
	res := Result{Agent: "copilot", Provider: "personal", Model: "claude-opus-4.5"}
	return req, res, copilotsdk.Result{Text: "Step 1 done.", SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}
}

func TestCopilotQuotaHandoffWritesHandoffNoRerun(t *testing.T) {
	dir, marked := stubHandoff(t)
	req, res, partial := quotaReq()
	out, err := copilotQuotaHandoff(req, res, partial, errQuotaMidTask, nil)
	if !errors.Is(err, errQuotaMidTask) || Classify("copilot", err, false).Kind != KindQuota {
		t.Fatalf("err = %v; want the KindQuota cause", err)
	}
	if out.Provider != res.Provider || out.Answer != "" || out.SessionID != partial.SessionID {
		t.Fatalf("result = %+v; want the original provider and partial session without a rerun answer", out)
	}
	if len(*marked) != 1 || (*marked)[0] != "github·edotau" {
		t.Errorf("marked = %v; want github·edotau", *marked)
	}
	want := filepath.Join(dir, time.Now().Format("2006-01-02")+"-copilot-0a1b2c3d.md")
	if out.Handoff != want {
		t.Fatalf("handoff = %q, want %q", out.Handoff, want)
	}
	body, err := os.ReadFile(out.Handoff)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Refactor foo", "Step 1 done.", "copilot --resume " + partial.SessionID,
		"account: github·edotau", "402", "REDACTED"} {
		if !strings.Contains(string(body), text) {
			t.Errorf("handoff lacks %q", text)
		}
	}
	if strings.Contains(string(body), "ghp_"+strings.Repeat("a", 36)) {
		t.Error("handoff holds the raw token")
	}
}

// "<profile>:<tier>" resolves like --provider <profile> --model <tier>; anything else is left alone.
func TestResolveTargetProfileTierRewrite(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5","haiku":"claude-haiku-4-5"}}}}}`)
	_, want, err := resolveTarget(RunOptions{Provider: "dev", Model: "opus"})
	if err != nil || want.Provider == nil || want.Provider.Name != "dev" {
		t.Fatalf("baseline --provider dev --model opus = %+v, %v", want, err)
	}

	t.Run("dev:opus equals --provider dev --model opus", func(t *testing.T) {
		_, got, err := resolveTarget(RunOptions{Model: "dev:opus"})
		if err != nil || got.Provider == nil || got.Provider.Name != "dev" || got.Model != want.Model {
			t.Errorf("dev:opus = %+v (%v), want provider dev model %q", got, err, want.Model)
		}
	})
	t.Run("full id keeps its prior behaviour", func(t *testing.T) {
		_, viaSpec, err1 := resolveTarget(RunOptions{Model: "dev:claude-opus-5-5"})
		_, viaFlags, err2 := resolveTarget(RunOptions{Provider: "dev", Model: "claude-opus-5-5"})
		if err1 != nil || err2 != nil || viaSpec.Provider == nil || viaSpec.Provider.Name != "dev" ||
			viaSpec.Model != viaFlags.Model {
			t.Errorf("spec %+v (%v) vs flags %+v (%v)", viaSpec, err1, viaFlags, err2)
		}
	})
	t.Run("unknown row or non-tier is not rewritten", func(t *testing.T) {
		_, bad, err := resolveTarget(RunOptions{Model: "unknownrow:opus"})
		if err == nil && bad.Provider != nil && bad.Provider.Name == "unknownrow" {
			t.Errorf("unknownrow:opus rewritten: %+v", bad)
		}
		_, nt, _ := resolveTarget(RunOptions{Model: "dev:notatier"})
		if nt.Provider != nil && nt.Provider.Name == "dev" && nt.Model == want.Model {
			t.Errorf("dev:notatier rewritten to dev's opus tier: %+v", nt)
		}
	})
	t.Run("the explicit backend is still seen", func(t *testing.T) {
		req := RunOptions{Model: "dev:opus"}
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
func (h *handoffRunner) Run(context.Context, RunOptions, Sink) (Result, error) {
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
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{first(), second}, "", "sonnet"), RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Answer != "from leg 2" || res.Handoff != "/tmp/x.md" {
			t.Fatalf("res=%+v err=%v; want leg 2's answer with handoff /tmp/x.md", res, err)
		}
	})

	t.Run("leg 2 fails with another error", func(t *testing.T) {
		boom := errors.New("boom: unrelated failure")
		second := &handoffRunner{name: "claude", err: boom}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{first(), second}, "", "sonnet"), RunOptions{Prompt: "p"}, Discard, nil)
		if err == nil || !errors.Is(err, boom) || res.Handoff != "/tmp/x.md" {
			t.Fatalf("res=%+v err=%v; want leg 2's error with handoff /tmp/x.md", res, err)
		}
	})

	t.Run("a later leg's own handoff wins", func(t *testing.T) {
		second := &handoffRunner{name: "claude", res: Result{Answer: "ok", Handoff: "/tmp/y.md"}}
		res, _ := RunWithFallback(context.Background(), chainOf([]Runner{first(), second}, "", "sonnet"), RunOptions{Prompt: "p"}, Discard, nil)
		if res.Handoff != "/tmp/y.md" {
			t.Errorf("handoff = %q, want the later leg's /tmp/y.md", res.Handoff)
		}
	})
}

// tok builds a GitHub-shaped token for prefix, so no real secret sits in the source.
func tok(prefix string) string { return prefix + strings.Repeat("a1", 18) }

// The hand-off file never holds a seeded raw token, GitHub-shaped or a plain resolved credential.
func TestCopilotHandoffRedactsEveryToken(t *testing.T) {
	_, _ = stubHandoff(t)
	const plain = "plainsecretvalue-not-shaped-123"
	var all []string
	for _, p := range []string{"ghu_", "gho_", "ghs_", "ghr_", "ghp_", "github_pat_"} {
		all = append(all, tok(p))
	}
	req, res, partial := quotaReq()
	req.Prompt = "Task with " + strings.Join(all, " ") + " and " + plain
	partial.Text = "partial with " + all[2] + " and " + plain
	cause := errors.New("402 quota; token " + plain)
	out, err := copilotQuotaHandoff(req, res, partial, cause, []string{plain})
	if !errors.Is(err, cause) || Classify("copilot", err, false).Kind != KindQuota {
		t.Fatalf("err = %v; want the KindQuota cause", err)
	}
	body, err := os.ReadFile(out.Handoff)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(all, plain) {
		if strings.Contains(string(body), secret) {
			t.Errorf("handoff file holds %q", secret)
		}
	}
	if !strings.Contains(string(body), "REDACTED") {
		t.Error("handoff file should carry the redacted task and partial answer")
	}
}

// The runner writes redacted context and leaves resumption to the next fallback leg.
func TestCopilotRunnerHandoffRedacted(t *testing.T) {
	_, _ = stubHandoff(t)
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	secret := tok("ghs_")
	calls := stubAsk(t, func(n int) (copilotsdk.Result, error) {
		if n != 1 {
			t.Fatal("quota handoff must not rerun the SDK")
		}
		return copilotsdk.Result{Text: "half " + secret, SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}, errQuotaMidTask
	})
	res, err := copilotRunner{}.Run(context.Background(), RunOptions{Prompt: "do it " + secret, Model: "sonnet", Provider: "personal", Direct: true}, Discard)
	if !errors.Is(err, errQuotaMidTask) || Classify("copilot", err, false).Kind != KindQuota || *calls != 1 {
		t.Fatalf("res=%+v err=%v calls=%d; want quota and one SDK call", res, err, *calls)
	}
	body, err := os.ReadFile(res.Handoff)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) || !strings.Contains(string(body), "REDACTED") {
		t.Errorf("handoff file must redact the raw token: %q", body)
	}
	for _, text := range []string{"do it ", "half "} {
		if !strings.Contains(string(body), text) {
			t.Errorf("handoff lacks %q", text)
		}
	}
}

func TestResumeSlot(t *testing.T) {
	for in, want := range map[string]string{"sonnet": "sonnet", "dev:haiku": "haiku", "claude-sonnet-5-5": "sonnet", "gpt-5": ""} {
		if got := resumeSlot(in); got != want {
			t.Errorf("resumeSlot(%q) = %q, want %q", in, got, want)
		}
	}
}

// The hand-off preserves the resolved model and any exposed session for the next leg.
func TestCopilotHandoffKeepsModelAndSession(t *testing.T) {
	_, _ = stubHandoff(t)
	for name, session := range map[string]string{"with session": "0a1b2c3d-4e5f-6789-abcd-ef0123456789", "no session": ""} {
		t.Run(name, func(t *testing.T) {
			req, res, partial := quotaReq()
			req.Model, res.Model, partial.SessionID, res.SessionID = "sonnet", "claude-sonnet-5-5", session, ""
			out, err := copilotQuotaHandoff(req, res, partial, errQuotaMidTask, nil)
			if !errors.Is(err, errQuotaMidTask) {
				t.Fatal(err)
			}
			if out.Model != res.Model || out.SessionID != session {
				t.Fatalf("result = %+v; want model %q and session %q", out, res.Model, session)
			}
			body, err := os.ReadFile(out.Handoff)
			if err != nil {
				t.Fatal(err)
			}
			wantSession := "session: none exposed"
			if session != "" {
				wantSession = "session: " + session + " (`copilot --resume " + session + "`)"
			}
			for _, text := range []string{"model: " + res.Model, wantSession, "Step 1 done."} {
				if !strings.Contains(string(body), text) {
					t.Errorf("handoff lacks %q", text)
				}
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
	tg, _, _, err := vendorTargetNote(RunOptions{Prompt: "p", Provider: "personal", Model: "sonnet"}, "copilot")
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
			if _, err := RunWithFallback(context.Background(), chainOf([]Runner{first, second}, "", "sonnet"), RunOptions{Prompt: "BARE-TASK"}, Discard, nil); err != nil {
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

// (7) Hand-offs with the same date and session id get distinct files and none is truncated.
func TestCopilotHandoffsNeverOverwrite(t *testing.T) {
	dir, _ := stubHandoff(t)
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

// A typed [1m] id is stripped before it is respelled for the fallback provider: no upstream accepts the marker.
func TestVendorTargetNoteFallbackStripsOneM(t *testing.T) {
	ghLogin(t, "alice", 99)
	t.Setenv("DEV_TEST_KEY", "k")
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}}}}}`)
	tg, _, _, err := vendorTargetNote(RunOptions{Prompt: "p", Model: "claude-sonnet-5-5[1m]"}, "copilot")
	if err != nil || tg.Provider == nil || tg.Provider.Name != "dev" {
		t.Fatalf("target = %+v err=%v, want the dev fallback", tg, err)
	}
	if strings.Contains(tg.Model, "[1m]") {
		t.Errorf("model %q still carries [1m]", tg.Model)
	}
}
