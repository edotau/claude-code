package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestApplyCaps(t *testing.T) {
	req := Request{Prompt: "p", Model: "m", Provider: "openai", Effort: "high", WorkDir: "/tmp", SessionID: "s", Tools: true}
	got, dropped := applyCaps(Caps{Model: true, Provider: true}, req)
	want := []string{"--effort=high", "--workdir=/tmp", "--session=s", "--tools"}
	if !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %q, want %q", dropped, want)
	}
	if got.Effort != "" || got.WorkDir != "" || got.SessionID != "" || got.Tools || got.Model != "m" || got.Provider != "openai" {
		t.Errorf("request not cleared to caps: %+v", got)
	}
	if _, d := applyCaps(Caps{}, Request{Prompt: "p"}); d != nil {
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
	seen        []Request
}

func (f *fakeRunner) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}
func (f *fakeRunner) Caps() Caps       { return f.caps }
func (f *fakeRunner) Available() error { return nil }
func (f *fakeRunner) Run(_ context.Context, req Request, w io.Writer) (Result, error) {
	f.seen = append(f.seen, req)
	io.WriteString(w, f.write)
	err := f.errs[len(f.seen)-1]
	ans := "a"
	if f.emptyAnswer {
		ans = ""
	}
	return Result{Answer: ans}, err
}

func TestRunDropsAndProgressVeto(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := &fakeRunner{caps: Caps{Model: true}, errs: []error{nil}}
	res, err := Run(context.Background(), f, Request{Prompt: "p", Model: "m", Effort: "max"}, io.Discard, nil)
	if err != nil || res.Agent != "fake" || !reflect.DeepEqual(res.Dropped, []string{"--effort=max"}) || f.seen[0].Effort != "" {
		t.Fatalf("res=%+v err=%v seen=%+v", res, err, f.seen)
	}

	f = &fakeRunner{write: "partial", errs: []error{errors.New("HTTP 503 overloaded"), nil}}
	var out strings.Builder
	_, err = Run(context.Background(), f, Request{Prompt: "p"}, &out, nil)
	var e *Error
	if !errors.As(err, &e) || !e.Progressed || len(f.seen) != 1 {
		t.Fatalf("streamed output must veto retry: err=%v attempts=%d", err, len(f.seen))
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
	cmd, tail := childCmd(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"}, nil, "", "")
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
	cmd, _ := childCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, dir, "")
	if !slices.Contains(cmd.Env, "PWD="+dir) {
		t.Errorf("cmd.Env = %q, want PWD=%s", cmd.Env, dir)
	}

	// dir == "" (no --workdir): childCmd must append nothing past the inherited os.Environ() PWD.
	cmd, _ = childCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "")
	if n, want := len(cmd.Env), len(os.Environ()); n != want {
		t.Errorf("cmd.Env has %d entries, want %d (os.Environ(), no PWD appended)", n, want)
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hard := errors.New("HTTP 429: You exceeded your current quota. Quota exceeded for metric: requests, limit: 0, model: big")
	f := &fakeRunner{caps: Caps{Model: true}, errs: []error{hard, nil}}
	var notes []string
	res, err := Run(context.Background(), f, Request{Prompt: "p"}, io.Discard, func(s string) { notes = append(notes, s) })
	if err != nil || len(f.seen) != 2 || f.seen[0].Model != "" || f.seen[1].Model != "sonnet" {
		t.Fatalf("err=%v seen=%+v res=%+v", err, f.seen, res)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "quota-dead") {
		t.Errorf("demotion must be announced: %v", notes)
	}

	// A transient 429 keeps the model: nothing to demote.
	f = &fakeRunner{caps: Caps{Model: true}, errs: []error{errors.New("HTTP 429 rate limited, try again in 1s"), nil}}
	if _, err := Run(context.Background(), f, Request{Prompt: "p", Model: "opus"}, io.Discard, nil); err != nil || f.seen[1].Model != "opus" {
		t.Fatalf("transient 429 must retry the same slot: err=%v seen=%+v", err, f.seen)
	}
}

// TestCodexUnavailable tables the fallback classifier: only an unreachable codex (missing binary, no
// serving provider, or an exec failure with no answer) should fall back; everything else must not.
func TestLegUnavailable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	genericErr := errors.New(`exec: "codex": executable file not found in $PATH`)
	deadline := &Error{Kind: KindFatal, Err: context.DeadlineExceeded}
	canceled := &Error{Kind: KindFatal, Err: context.Canceled}
	tooLarge := &Error{Kind: KindFatal, Err: fmt.Errorf("prompt: %w", ErrPromptTooLarge)}
	progressed := &Error{Kind: KindTransient, Progressed: true, Err: genericErr}

	cases := []struct {
		name string
		r    Runner
		req  Request
		res  Result
		err  error
		want bool
	}{
		{"nil error", &fakeRunner{name: "codex"}, Request{}, Result{}, nil, false},
		{"claude ends the chain", &fakeRunner{name: "claude"}, Request{}, Result{}, genericErr, false},
		{"any vendor leg", &fakeRunner{name: "gemini"}, Request{}, Result{}, genericErr, true},
		{"non-empty answer", &fakeRunner{name: "codex"}, Request{}, Result{Answer: "partial"}, genericErr, false},
		{"deadline exceeded", &fakeRunner{name: "codex"}, Request{}, Result{}, deadline, false},
		{"canceled", &fakeRunner{name: "codex"}, Request{}, Result{}, canceled, false},
		{"prompt too large", &fakeRunner{name: "codex"}, Request{}, Result{}, tooLarge, false},
		{"progressed", &fakeRunner{name: "codex"}, Request{}, Result{}, progressed, false},
		{"wrapped progressed", &fakeRunner{name: "codex"}, Request{}, Result{}, fmt.Errorf("runner: %w", progressed), false},
		{"not progressed", &fakeRunner{name: "codex"}, Request{}, Result{}, &Error{Kind: KindFatal, Err: genericErr}, true},
		{"binary missing", &fakeRunner{name: "codex"}, Request{}, Result{}, genericErr, true},
		{"claude model slot stays eligible", &fakeRunner{name: "codex"}, Request{Model: "opus"}, Result{}, genericErr, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := legUnavailable(c.r, c.res, c.err); got != c.want {
				t.Errorf("legUnavailable() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRunWithFallback wires the classifier into Run: codex failing with no answer must rerun on the
// fallback runner with Fallback set, while a successful or excluded-class codex run must not.
func TestRunWithFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	unavailable := errors.New(`exec: "codex": executable file not found in $PATH`)

	t.Run("falls back and reports it", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{unavailable}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), codex, []Runner{claude}, Request{Prompt: "p"}, io.Discard, nil)
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

	t.Run("vendor legs come before claude and the first answer wins", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{unavailable}, emptyAnswer: true}
		gemini := &fakeRunner{name: "gemini", caps: Caps{Model: true}, errs: []error{unavailable}, emptyAnswer: true}
		opencode := &fakeRunner{name: "opencode", caps: Caps{Model: true}, errs: []error{nil}}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		req := Request{Prompt: "p", Model: "gpt-6-astra"}
		res, err := RunWithFallback(context.Background(), codex, []Runner{gemini, opencode, claude}, req, io.Discard, nil)
		if err != nil || res.Agent != "opencode" || res.Fallback != "codex" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%d", res, err, len(claude.seen))
		}
		if !strings.Contains(res.FallbackReason, "codex: ") || !strings.Contains(res.FallbackReason, "; gemini: ") {
			t.Errorf("FallbackReason = %q, want one entry per skipped leg", res.FallbackReason)
		}
		if gemini.seen[0].Model != "" || opencode.seen[0].Model != "" {
			t.Errorf("a vendor leg must take its own default model, saw %q / %q", gemini.seen[0].Model, opencode.seen[0].Model)
		}
	})

	t.Run("an empty chain means no fallback", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{unavailable}, emptyAnswer: true}
		res, err := RunWithFallback(context.Background(), codex, nil, Request{Prompt: "p"}, io.Discard, nil)
		if err == nil || res.Fallback != "" || res.Agent != "codex" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("success needs no fallback", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{nil}}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), codex, []Runner{claude}, Request{Prompt: "p"}, io.Discard, nil)
		if err != nil || res.Fallback != "" || res.Agent != "codex" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
		}
	})

	t.Run("prompt too large is not retried on the fallback", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", errs: []error{fmt.Errorf("p: %w", ErrPromptTooLarge)}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		res, err := RunWithFallback(context.Background(), codex, []Runner{claude}, Request{Prompt: "p"}, io.Discard, nil)
		if err == nil || res.Fallback != "" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
		}
	})

	t.Run("explicit non-claude model is never overridden", func(t *testing.T) {
		codex := &fakeRunner{name: "codex", caps: Caps{Model: true}, errs: []error{unavailable}, emptyAnswer: true}
		claude := &fakeRunner{name: "claude", errs: []error{nil}}
		req := Request{Prompt: "p", Model: "openai:gpt-4"}
		res, err := RunWithFallback(context.Background(), codex, []Runner{claude}, req, io.Discard, nil)
		if err == nil || res.Fallback != "" || len(claude.seen) != 0 {
			t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
		}
	})

	t.Run("model mapping onto the fallback", func(t *testing.T) {
		mapping := []struct{ in, want string }{
			{"", "sonnet"},
			{"opus", "opus"},
			{"claude-sonnet-4-5", "claude-sonnet-4-5"},
			{"gpt-5", "sonnet"},
		}
		for _, m := range mapping {
			codex := &fakeRunner{name: "codex", caps: Caps{Model: true}, errs: []error{unavailable}, emptyAnswer: true}
			claude := &fakeRunner{name: "claude", caps: Caps{Model: true}, errs: []error{nil}}
			req := Request{Prompt: "p", Model: m.in}
			if _, err := RunWithFallback(context.Background(), codex, []Runner{claude}, req, io.Discard, nil); err != nil {
				t.Fatalf("model=%q err=%v", m.in, err)
			}
			if len(claude.seen) != 1 || claude.seen[0].Model != m.want {
				t.Errorf("model=%q: fallback saw Model=%q, want %q", m.in, claude.seen[0].Model, m.want)
			}
		}
	})
}

// TestRunWithFallback_TimeoutAndCancelNotRetried proves the two context-based exclusions against a real
// expiring context, since Run's own tools-on special-case only fires on an actual ctx.Err().
func TestRunWithFallback_TimeoutAndCancelNotRetried(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	claude := &fakeRunner{name: "claude", errs: []error{nil}}

	codex := &codexHang{}
	_, err := RunWithFallback(context.Background(), codex, []Runner{claude},
		Request{Prompt: "p", Tools: true, Timeout: 20 * time.Millisecond}, io.Discard, nil)
	if err == nil || len(claude.seen) != 0 {
		t.Fatalf("tools-on timeout must not fall back: err=%v claude.seen=%+v", err, claude.seen)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	codex = &codexHang{}
	_, err = RunWithFallback(ctx, codex, []Runner{claude}, Request{Prompt: "p"}, io.Discard, nil)
	if err == nil || len(claude.seen) != 0 {
		t.Fatalf("a canceled caller context must not fall back: err=%v claude.seen=%+v", err, claude.seen)
	}
}

// codexHang blocks until its context is done, named "codex" so RunWithFallback's class check applies.
type codexHang struct{ calls int }

func (h *codexHang) Name() string     { return "codex" }
func (h *codexHang) Caps() Caps       { return Caps{Tools: true} }
func (h *codexHang) Available() error { return nil }
func (h *codexHang) Run(ctx context.Context, _ Request, _ io.Writer) (Result, error) {
	h.calls++
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// TestFallbackChain: vendors only in roster order without the requester, claude last only under "all".
func TestFallbackChain(t *testing.T) {
	names := func(rs []Runner) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name())
		}
		return out
	}
	codex := &fakeRunner{name: "codex"}
	all := names(FallbackChain(codex, FallbackAll))
	if len(all) == 0 || all[len(all)-1] != "claude" || slices.Contains(all, "codex") || slices.Contains(all, "api") {
		t.Errorf("all = %v, want installed vendors (no codex/api) then claude", all)
	}
	if v := names(FallbackChain(codex, FallbackVendors)); slices.Contains(v, "claude") {
		t.Errorf("vendors = %v, must never include claude", v)
	}
	if n := FallbackChain(codex, FallbackNone); n != nil {
		t.Errorf("none = %v, want nil", names(n))
	}
	if c := FallbackChain(&fakeRunner{name: "claude"}, FallbackAll); c != nil {
		t.Errorf("claude itself never falls back, got %v", names(c))
	}
}
