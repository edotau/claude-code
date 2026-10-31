package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
)

func TestApplyCaps(t *testing.T) {
	req := RunOptions{Prompt: "p", Model: "m", Provider: "openai", Effort: "high", WorkDir: "/tmp", SessionID: "s", Tools: true}
	got, dropped, _ := applyCaps(Caps{Model: Native, Provider: Native}, req)
	want := []string{"--effort=high", "--workdir=/tmp", "--session=s", "--tools"}
	if !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %q, want %q", dropped, want)
	}
	if got.Effort != "" || got.WorkDir != "" || got.SessionID != "" || got.Tools || got.Model != "m" || got.Provider != "openai" {
		t.Errorf("request not cleared to caps: %+v", got)
	}
	if _, d, _ := applyCaps(Caps{}, RunOptions{Prompt: "p"}); d != nil {
		t.Errorf("unset fields must not be reported, got %q", d)
	}
}

// fakeRunner fails with errs[attempt] and records the request it saw.
type fakeRunner struct {
	name        string // defaults to "fake"
	caps        Caps
	errs        []error
	write       string
	emptyAnswer bool // forces Result.Answer to "" instead of the default "a"
	seen        []RunOptions
}

func (f *fakeRunner) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}
func (f *fakeRunner) Caps() Caps       { return f.caps }
func (f *fakeRunner) Available() error { return nil }
func (f *fakeRunner) Run(_ context.Context, req RunOptions, w Sink) (Result, error) {
	f.seen = append(f.seen, req)
	w.Emit(Event{Type: EventText, Text: f.write})
	err := f.errs[len(f.seen)-1]
	ans := "a"
	if f.emptyAnswer {
		ans = ""
	}
	return Result{Answer: ans}, err
}

func TestRunDropsAndProgressVeto(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{nil}}
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p", Model: "m", Effort: "max"}, Discard, nil)
	if err != nil || res.Agent != "fake" || !reflect.DeepEqual(res.Dropped, []string{"--effort=max"}) || f.seen[0].Effort != "" {
		t.Fatalf("res=%+v err=%v seen=%+v", res, err, f.seen)
	}

	f = &fakeRunner{write: "partial", errs: []error{errors.New("HTTP 503 overloaded"), nil}}
	var out strings.Builder
	_, err = Run(context.Background(), f, RunOptions{Prompt: "p"}, NewTextSink(&out), nil)
	var e *Error
	if !errors.As(err, &e) || !e.Progressed || len(f.seen) != 1 {
		t.Fatalf("streamed output must veto retry: err=%v attempts=%d", err, len(f.seen))
	}
}

// TestRunAgentFallsBackToRunnerNameWhenUnset proves the cmp.Or in Run() only fills Agent from the
// runner's own name when the runner itself left it blank — the non-passthrough/non-delegating path
// (here named "api" to match the real runner) still reports Agent "api", not an empty envelope field.
func TestRunAgentFallsBackToRunnerNameWhenUnset(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := &fakeRunner{name: "api", errs: []error{nil}}
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || res.Agent != "api" {
		t.Fatalf("res.Agent = %q, err = %v, want %q and nil err", res.Agent, err, "api")
	}
}

func TestRubricPrompt(t *testing.T) {
	file := t.TempDir() + "/rubric.md"
	if err := writeFile(file, "RULE: no globals"); err != nil {
		t.Fatal(err)
	}
	p, err := RubricPrompt(file, "review x.go")
	if err != nil || !strings.Contains(p, "RULE: no globals") || !strings.Contains(p, "TASK: review x.go") {
		t.Fatalf("prompt=%q err=%v", p, err)
	}
	if _, err := RubricPrompt(file, strings.Repeat("x", MaxPromptBytes)); err == nil {
		t.Error("oversize prompt must be an error, never truncated")
	}
}

func TestChildDeadlineKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd, tail := childCmd(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"}, nil, "", "", nil, false)
	start := time.Now()
	err := childErr(ctx, "sh", cmd.Run(), tail)
	if time.Since(start) > 3*time.Second || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("grandchild outlived the deadline: took %v, err %v", time.Since(start), err)
	}
}

// childCmd sets $PWD alongside cmd.Dir so a child that reads $PWD (not getcwd()) still sees dir; an
// empty dir (no --workdir) must leave PWD unset so the child inherits the caller's own cwd.
func TestChildCmdSetsPWD(t *testing.T) {
	dir := t.TempDir()
	cmd, _ := childCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, dir, "", nil, false)
	if !slices.Contains(cmd.Env, "PWD="+dir) {
		t.Errorf("cmd.Env = %q, want PWD=%s", cmd.Env, dir)
	}

	// dir == "" (no --workdir): childCmd must append nothing past the scrubbed environ, no PWD.
	scrubbed, _ := proc.Scrub(os.Environ(), nil)
	cmd, _ = childCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, false)
	if n, want := len(cmd.Env), len(scrubbed); n != want {
		t.Errorf("cmd.Env has %d entries, want %d (scrubbed os.Environ(), no PWD appended)", n, want)
	}
}

// A vendor CLI child never sees HARNESS_VAULT_KEY (it would open the vault outside the harness's own
// process); the claude runner's own child keeps it since hooks need it to read the vault.
func TestChildCmdScrubsVaultKeyForVendorOnly(t *testing.T) {
	t.Setenv("HARNESS_VAULT_KEY", "secret-value")
	hasKey := func(env []string) bool {
		return slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "HARNESS_VAULT_KEY=") })
	}
	cmd, _ := childCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, true)
	if hasKey(cmd.Env) {
		t.Errorf("vendor child env leaked HARNESS_VAULT_KEY: %v", cmd.Env)
	}
	cmd, _ = childCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, false)
	if !hasKey(cmd.Env) {
		t.Errorf("claude's own child must keep HARNESS_VAULT_KEY, env=%v", cmd.Env)
	}
}

func TestSessionFromBanner(t *testing.T) {
	banner := "OpenAI Codex v0.154.0\n--------\nmodel: m\nsession id: 01a0aa74-a4c4\n--------\nuser\nsession id: not-this\n"
	if got := sessionFromBanner(banner); got != "01a0aa74-a4c4" {
		t.Errorf("sessionFromBanner = %q, want the banner id", got)
	}
	var h headBuffer
	h.Write([]byte(banner))
	h.Write(make([]byte, 8<<10))
	if len(h.String()) != 4<<10 || sessionFromBanner(h.String()) != "01a0aa74-a4c4" {
		t.Errorf("headBuffer kept %d bytes", len(h.String()))
	}
}

// A hard-quota 429 (this key cannot use the model) demotes the slot for the retry instead of waiting it out.
func TestRunDemotesSlotOnHardQuota(t *testing.T) {
	fastDefaultBackoff(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hard := errors.New("HTTP 429: You exceeded your current quota. Quota exceeded for metric: requests, limit: 0, model: big")
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{hard, nil}}
	var notes []string
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, Discard, func(s string) { notes = append(notes, s) })
	if err != nil || len(f.seen) != 2 || f.seen[0].Model != "" || f.seen[1].Model != "sonnet" {
		t.Fatalf("err=%v seen=%+v res=%+v", err, f.seen, res)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "quota-dead") {
		t.Errorf("demotion must be announced: %v", notes)
	}

	// A transient 429 keeps the model: nothing to demote.
	f = &fakeRunner{caps: Caps{Model: Native}, errs: []error{errors.New("HTTP 429 rate limited, try again in 1s"), nil}}
	if _, err := Run(context.Background(), f, RunOptions{Prompt: "p", Model: "opus"}, Discard, nil); err != nil || f.seen[1].Model != "opus" {
		t.Fatalf("transient 429 must retry the same slot: err=%v seen=%+v", err, f.seen)
	}
}

// A 402 (account exhausted: no credits/plan window left) is KindQuota and IsQuota-true but ModelDenied-false:
// a lower slot on the same account is just as exhausted, so Run must not demote and must return the error.
func TestRunQuotaAccountExhaustedDoesNotDemote(t *testing.T) {
	fastDefaultBackoff(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	acctExhausted := errors.New("HTTP 402 payment required: you have exceeded your monthly quota")
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{acctExhausted}}
	var notes []string
	_, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, Discard, func(s string) { notes = append(notes, s) })
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindQuota {
		t.Fatalf("err = %v, want a KindQuota error", err)
	}
	if len(f.seen) != 1 || f.seen[0].Model != "" {
		t.Fatalf("seen = %+v, want one attempt with the model never demoted off \"\"", f.seen)
	}
	if len(notes) != 0 {
		t.Errorf("account exhaustion must not announce a slot demotion: %v", notes)
	}

	// A body carrying BOTH the 402/payment-required account marker AND a model-denied marker
	// ("insufficient_quota") is still account-exhausted: hardAccountRE vetoes ModelDenied, so Run must not
	// demote off a dead account on the strength of the model-denied marker alone.
	both := errors.New("402 payment required: insufficient_quota")
	f = &fakeRunner{caps: Caps{Model: Native}, errs: []error{both}}
	var bothNotes []string
	_, err = Run(context.Background(), f, RunOptions{Prompt: "p"}, Discard, func(s string) { bothNotes = append(bothNotes, s) })
	if !errors.As(err, &e) || e.Kind != KindQuota {
		t.Fatalf("err = %v, want a KindQuota error", err)
	}
	if len(f.seen) != 1 || f.seen[0].Model != "" {
		t.Fatalf("seen = %+v, want one attempt with the model never demoted off \"\"", f.seen)
	}
	if len(bothNotes) != 0 {
		t.Errorf("402+insufficient_quota must not announce a slot demotion: %v", bothNotes)
	}
}

// TestCodexUnavailable tables the fallback classifier: only an unreachable codex (missing binary, no
// serving provider, or an exec failure with no answer) should fall back; everything else must not.
func TestDispatchUnavailable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	genericErr := errors.New(`exec: "codex": executable file not found in $PATH`)
	deadline := &Error{Kind: KindFatal, Err: context.DeadlineExceeded}
	canceled := &Error{Kind: KindFatal, Err: context.Canceled}
	tooLarge := &Error{Kind: KindFatal, Err: fmt.Errorf("prompt: %w", ErrPromptTooLarge)}
	progressed := &Error{Kind: KindTransient, Progressed: true, Err: genericErr}

	cases := []struct {
		name string
		r    Runner
		req  RunOptions
		res  Result
		err  error
		want bool
	}{
		{"nil error", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, nil, false},
		{"claude ends the chain", &fakeRunner{name: "claude"}, RunOptions{}, Result{}, genericErr, false},
		{"any vendor dispatch", &fakeRunner{name: "gemini"}, RunOptions{}, Result{}, genericErr, true},
		{"non-empty answer", &fakeRunner{name: "codex"}, RunOptions{}, Result{Answer: "partial"}, genericErr, false},
		{"deadline exceeded", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, deadline, false},
		{"canceled", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, canceled, false},
		{"prompt too large", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, tooLarge, false},
		{"progressed", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, progressed, false},
		{"wrapped progressed", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, fmt.Errorf("runner: %w", progressed), false},
		{"not progressed", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, &Error{Kind: KindFatal, Err: genericErr}, true},
		{"binary missing", &fakeRunner{name: "codex"}, RunOptions{}, Result{}, genericErr, true},
		{"claude model slot stays eligible", &fakeRunner{name: "codex"}, RunOptions{Model: "opus"}, Result{}, genericErr, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dispatchUnavailable(c.r, c.res, c.err); got != c.want {
				t.Errorf("dispatchUnavailable() = %v, want %v", got, c.want)
			}
		})
	}
}

// chainOf is a routed chain over rs: the first dispatch is the wanted runner keeping the caller's model, each later
// dispatch runs models[i] (its cell).
func chainOf(rs []Runner, models ...string) Chain {
	c := Chain{Want: rs[0].Name()}
	for i, r := range rs {
		disp := Dispatch{Agent: r, keepCaller: i == 0}
		if i < len(models) {
			disp.Model = models[i]
		}
		c.Dispatches = append(c.Dispatches, disp)
	}
	return c
}

// TestRunWithFallback wires the classifier into Run: codex failing with no answer must rerun on the
// fallback runner with Fallback set, while a successful or excluded-class codex run must not.
func TestRunWithFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	unavailable := errors.New(`exec: "codex": executable file not found in $PATH`)

	t.Run("falls back and reports it", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{unavailable}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Fallback != "codex" || res.Agent != "claude" || res.Answer != "a" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if !strings.HasPrefix(res.FallbackReason, "codex: ") || !strings.Contains(res.FallbackReason, "executable file not found") ||
			strings.Contains(res.FallbackReason, "\n") {
			t.Fatalf("FallbackReason = %q, want the last line of the codex error", res.FallbackReason)
		}
		if len(claude.seen) != 1 || claude.seen[0].Prompt != "p" {
			t.Fatalf("fallback did not see the original request: %+v", claude.seen)
		}
	})

	t.Run("each dispatch runs its own cell and the first answer wins", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{unavailable}, emptyAnswer: true}
		gemini := &fakeRunner{name: "gemini", caps: Caps{Model: Native}, errs: []error{unavailable}, emptyAnswer: true}
		opencode := &fakeRunner{name: "opencode", caps: Caps{Model: Native}, errs: []error{nil}}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		req := RunOptions{Prompt: "p", Model: "gpt-6.1-sol"}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, gemini, opencode, claude}, "", "gemini-4-argon", "kimi-k3", "opus"), req, Discard, nil)
		if err != nil || res.Agent != "opencode" || res.Fallback != "codex" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%d", res, err, len(claude.seen))
		}
		if !strings.Contains(res.FallbackReason, "codex: ") || !strings.Contains(res.FallbackReason, "; gemini: ") {
			t.Errorf("FallbackReason = %q, want one entry per failed dispatch", res.FallbackReason)
		}
		if codex.seen[0].Model != "gpt-6.1-sol" || gemini.seen[0].Model != "gemini-4-argon" || opencode.seen[0].Model != "kimi-k3" {
			t.Errorf("models = %q / %q / %q, want the caller's then each cell's", codex.seen[0].Model, gemini.seen[0].Model, opencode.seen[0].Model)
		}
	})

	t.Run("the tier is kept on the claude dispatch", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{unavailable}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", caps: Caps{Model: Native}, errs: []error{nil}}
		if _, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, claude}, "", "opus"), RunOptions{Prompt: "p", Model: "claude-opus-5.5"}, Discard, nil); err != nil {
			t.Fatal(err)
		}
		if claude.seen[0].Model != "opus" {
			t.Errorf("claude dispatch ran %q, want the opus cell", claude.seen[0].Model)
		}
	})

	t.Run("gate skips make zero attempts and are reported", func(t *testing.T) {
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		c := Chain{Want: "copilot", Skipped: []string{"copilot: github·u exhausted until 2026-11-01"},
			Dispatches: []Dispatch{{Agent: claude, Model: "sonnet"}}}
		res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Fallback != "copilot" || !strings.Contains(res.FallbackReason, "exhausted until 2026-11-01") {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if _, err := RunWithFallback(context.Background(), Chain{Want: "copilot", Skipped: c.Skipped}, RunOptions{Prompt: "p"}, Discard, nil); err == nil {
			t.Error("an empty chain must fail with the skips")
		}
	})

	t.Run("a quota failure marks the account and moves on", func(t *testing.T) {
		quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
		copilot := &fakeRunner{name: "copilot", errs: []error{quota}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		c := chainOf([]Runner{copilot, claude}, "", "sonnet")
		c.Dispatches[0].Account, c.Dispatches[0].Billing = "github·u", "credits"
		res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Agent != "claude" || len(copilot.seen) != 1 {
			t.Fatalf("res=%+v err=%v copilot attempts=%d (quota is never retried)", res, err, len(copilot.seen))
		}
		if e := loadState()["github·u"]; e.Status(time.Now()) != "exhausted" || e.ExhaustedUntil.Day() != 1 {
			t.Errorf("state = %+v, want exhausted until the 1st", e)
		}
	})

	t.Run("an empty chain means no fallback", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{unavailable}, emptyAnswer: true}
		res, err := RunWithFallback(context.Background(), Single(codex), RunOptions{Prompt: "p"}, Discard, nil)
		if err == nil || res.Fallback != "" || res.Agent != "codex" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("success needs no fallback", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{nil}}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Fallback != "" || res.Agent != "codex" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
		}
	})

	t.Run("prompt too large is not retried on the fallback", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{fmt.Errorf("p: %w", ErrPromptTooLarge)}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
		if err == nil || res.Fallback != "" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
		}
	})

	t.Run("explicit non-claude model is never overridden", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{unavailable}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		req := RunOptions{Prompt: "p", Model: "openai:gpt-4"}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, claude}), req, Discard, nil)
		if err == nil || res.Fallback != "" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
		}
	})
}

// TestRouteChainSkipsExhaustedAndRetriesExpired: an exhausted pinned copilot gets no dispatch; once expired it leads again.
func TestRouteChainSkipsExhaustedAndRetriesExpired(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pinGitHubProfile(t, "work-user", "")
	ghHosts(t, "work-user")
	all := router.PickOpts{Configured: func(string, router.Cell) error { return nil }}
	tb, err := router.Load()
	if err != nil {
		t.Fatal(err)
	}
	acct := tb.Rows["copilot"]["sonnet"].Account
	if err := router.MarkExhausted(acct, time.Now().Add(time.Hour), time.Now(), "402"); err != nil {
		t.Fatal(err)
	}
	c, _, err := RouteChain(context.Background(), "copilot", "sonnet", router.FallbackAll, false, false, all)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range c.Dispatches {
		if l.Agent.Name() == "copilot" {
			t.Fatalf("exhausted copilot got a dispatch: %+v", c.Dispatches)
		}
	}
	if last := c.Dispatches[len(c.Dispatches)-1]; last.Agent.Name() != "claude" || last.Model != "sonnet" {
		t.Errorf("claude dispatch = %+v, want sonnet", last)
	}
	if len(c.Skipped) == 0 || !strings.Contains(c.Skipped[0], "exhausted") {
		t.Errorf("skips = %v", c.Skipped)
	}
	if err := router.Update(func(s router.State) { e := s[acct]; e.ExhaustedUntil = time.Now().Add(-time.Minute); s[acct] = e }); err != nil {
		t.Fatal(err)
	}
	c, _, _ = RouteChain(context.Background(), "copilot", "sonnet", router.FallbackAll, false, false, all)
	if c.Dispatches[0].Agent.Name() != "copilot" || c.Dispatches[0].Account != "github·work-user" {
		t.Errorf("expired entry must be retried on the pinned account: %+v", c.Dispatches[0])
	}
}

// TestRunWithFallback_TimeoutAndCancelNotRetried proves the two context-based exclusions against a real
// expiring context, since Run's own tools-on special-case only fires on an actual ctx.Err().
func TestRunWithFallback_TimeoutAndCancelNotRetried(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	claude := &fakeRunner{name: "claude", errs: []error{nil}}

	codex := &codexHang{}
	_, err := RunWithFallback(context.Background(), chainOf([]Runner{codex, claude}),
		RunOptions{Prompt: "p", Tools: true, Timeout: 20 * time.Millisecond}, NewTextSink(io.Discard), nil)
	if err == nil || len(claude.seen) != 0 {
		t.Fatalf("tools-on timeout must not fall back: err=%v claude.seen=%+v", err, claude.seen)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	codex = &codexHang{}
	_, err = RunWithFallback(ctx, chainOf([]Runner{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
	if err == nil || len(claude.seen) != 0 {
		t.Fatalf("a canceled caller context must not fall back: err=%v claude.seen=%+v", err, claude.seen)
	}
}

// TestRunTimeoutEndsKindFatalRegardlessOfTools: Run() no longer special-cases a tools-on timeout (that
// branch was removed as redundant); Do's own AttemptTimeout-is-terminal path is the only one left, and it
// must classify a hung call to KindFatal wrapping context.DeadlineExceeded the same whether Tools was on.
func TestRunTimeoutEndsKindFatalRegardlessOfTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tools := range []bool{true, false} {
		t.Run(fmt.Sprintf("Tools=%v", tools), func(t *testing.T) {
			hang := &codexHang{}
			_, err := Run(context.Background(), hang, RunOptions{Prompt: "p", Tools: tools, Timeout: 20 * time.Millisecond}, Discard, nil)
			var e *Error
			if !errors.As(err, &e) || e.Kind != KindFatal || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Tools=%v: err=%v, want KindFatal wrapping DeadlineExceeded", tools, err)
			}
		})
	}
}

// codexHang blocks until its context is done, named "codex" so RunWithFallback's class check applies.
type codexHang struct{ calls int }

func (h *codexHang) Name() string     { return "codex" }
func (h *codexHang) Caps() Caps       { return Caps{Tools: Native} }
func (h *codexHang) Available() error { return nil }
func (h *codexHang) Run(ctx context.Context, _ RunOptions, _ Sink) (Result, error) {
	h.calls++
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func TestIsText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, true},
		{"ascii", []byte("hello\n"), true},
		{"multibyte", []byte("héllo 世界 ✓"), true},
		{"nul", []byte("a\x00b"), false},
		{"invalid-utf8", []byte{0xff, 0xfe, 'a'}, false},
		{"truncated-rune", []byte{0xe4, 0xb8}, false},
	}
	for _, c := range cases {
		if got := IsText(c.in); got != c.want {
			t.Errorf("%s: IsText = %v, want %v", c.name, got, c.want)
		}
	}
}

// A binary --attach is rejected with the path named; a text file (multibyte too) is still inlined.
func TestAttachInlineRejectsBinaryKeepsText(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, data := range map[string][]byte{"nul.bin": []byte("ab\x00cd"), "bad.dat": {0xff, 0xfe, 0x80}} {
		p := write(name, data)
		_, err := attachInline("task", []string{p})
		if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "binary") {
			t.Errorf("%s: err = %v, want a binary-file error naming %s", name, err, p)
		}
	}
	good := write("ok.txt", []byte("héllo 世界"))
	got, err := attachInline("task", []string{good})
	if err != nil {
		t.Fatal(err)
	}
	if want := "task\n\n<file path=" + `"` + good + `"` + ">\nhéllo 世界\n</file>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRunStatusAndAttempts(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fastDefaultBackoff(t)
	dl := context.DeadlineExceeded
	cases := []struct {
		name     string
		write    string
		errs     []error
		status   string
		attempts int
	}{
		{"ok", "", []error{nil}, StatusOK, 1},
		{"retried transient then ok", "", []error{errors.New("HTTP 503 overloaded"), nil}, StatusOK, 2},
		{"quota", "", []error{&Error{Kind: KindQuota, Err: errors.New("plan exhausted")}}, StatusQuota, 1},
		{"partial", "half", []error{errors.New("boom")}, StatusPartial, 1},
		{"stalled", "", []error{dl, dl, dl}, StatusStalled, 3},
		{"error", "", []error{errors.New("bad request")}, StatusError, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{write: c.write, errs: c.errs}
			res, _ := Run(context.Background(), f, RunOptions{Prompt: "p"}, Discard, nil)
			if res.Status != c.status || res.Transport == nil || res.Transport.Attempts != c.attempts {
				t.Fatalf("status=%q transport=%+v, want %q attempts=%d", res.Status, res.Transport, c.status, c.attempts)
			}
		})
	}
}

// A gated-out own-login route followed by a same-agent Databricks route is a fallback; the requested route answering is not.
func TestRunWithFallbackSkippedRequestedRoute(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Run("own route skipped, same-agent databricks answers", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{nil}}
		c := Chain{Want: "codex", Skipped: []string{"codex: chatgpt exhausted"},
			Dispatches: []Dispatch{{Agent: codex, Account: "databricks·codex", keepCaller: true, otherRoute: true}}}
		res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Agent != "codex" || res.Fallback == "" ||
			!strings.Contains(res.FallbackReason, "chatgpt exhausted") {
			t.Fatalf("res=%+v err=%v, want Fallback set with the skip reason", res, err)
		}
	})
	t.Run("gemini answering itself stays null", func(t *testing.T) {
		gemini := &fakeRunner{name: "gemini", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), chainOf([]Runner{gemini}), RunOptions{Prompt: "p"}, Discard, nil)
		if err != nil || res.Fallback != "" || res.FallbackReason != "" {
			t.Fatalf("res=%+v err=%v, want no fallback", res, err)
		}
	})
}

// ghHosts makes user the only gh login: a scratch hosts.yml under GH_CONFIG_DIR.
func ghHosts(t *testing.T, user string) {
	t.Helper()
	dir := t.TempDir()
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
}
