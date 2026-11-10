package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
)

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
func (f *fakeRunner) Run(_ context.Context, req RunOptions, w wire.Sink) (wire.Result, error) {
	f.seen = append(f.seen, req)
	w.Emit(wire.Event{Type: wire.EventText, Text: f.write})
	err := f.errs[len(f.seen)-1]
	ans := "a"
	if f.emptyAnswer {
		ans = ""
	}
	return wire.Result{Answer: ans}, err
}

// toolRunner reports a tool call, no text, then a transient failure: the tool may have had effects.
type toolRunner struct{ fakeRunner }

func (f *toolRunner) Run(_ context.Context, req RunOptions, w wire.Sink) (wire.Result, error) {
	f.seen = append(f.seen, req)
	_ = w.Emit(wire.ToolEvent("Bash", "t1"))
	return wire.Result{}, io.ErrUnexpectedEOF
}

// codexHang blocks until its context is done, named "codex" so RunWithFallback's class check applies.
type codexHang struct {
	stubEngine
	calls int
}

func newCodexHang() *codexHang {
	return &codexHang{name: "codex", caps: Caps{Tools: Native}}
}

func (h *codexHang) Run(ctx context.Context, _ RunOptions, _ wire.Sink) (wire.Result, error) {
	h.calls++
	<-ctx.Done()
	return wire.Result{}, ctx.Err()
}

// A flag the agent lacks is dropped from the request and named in Dropped, whichever flag it is.
func TestRunDropsFlagsAgentLacks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name string
		req  RunOptions
		want []string
	}{
		{"effort", RunOptions{Prompt: "p", Model: "m", Effort: "max"}, []string{"--effort=max"}},
		{"fleet", RunOptions{Prompt: "p", Fleet: true}, []string{"--fleet"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{nil}}
			res, err := Run(context.Background(), f, c.req, wire.Discard, nil)
			if err != nil || res.Agent != "fake" || !reflect.DeepEqual(res.Dropped, c.want) || f.seen[0].Effort != "" || f.seen[0].Fleet {
				t.Fatalf("res=%+v err=%v seen=%+v", res, err, f.seen)
			}
		})
	}
}

// Streamed text or a tool call that may have had effects vetoes the retry: one attempt, Progressed set.
func TestRunProgressVetoesRetry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	text := &fakeRunner{write: "partial", errs: []error{errors.New("HTTP 503 overloaded"), nil}}
	tool := &toolRunner{}
	cases := []struct {
		name string
		r    Engine
		seen func() int
	}{
		{"streamed output", text, func() int { return len(text.seen) }},
		{"tool call", tool, func() int { return len(tool.seen) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			_, err := Run(context.Background(), c.r, RunOptions{Prompt: "p"}, wire.NewTextSink(&out), nil)
			var e *retry.Error
			if !errors.As(err, &e) || !e.Progressed || c.seen() != 1 {
				t.Fatalf("progress must veto retry: err=%v attempts=%d", err, c.seen())
			}
		})
	}
}

// TestRunAgentFallsBackToRunnerNameWhenUnset proves the cmp.Or in Run() only fills Agent from the
// agent's own name when the agent itself left it blank — the non-passthrough/non-delegating path
// (here named "api" to match the real agent) still reports Agent "api", not an empty envelope field.
func TestRunAgentFallsBackToRunnerNameWhenUnset(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := &fakeRunner{name: "api", errs: []error{nil}}
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, nil)
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

// A hard-quota 429 (this key cannot use the model) demotes the alias for the retry instead of waiting it out.
func TestRunDemotesSlotOnHardQuota(t *testing.T) {
	fastDefaultBackoff(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hard := errors.New("HTTP 429: You exceeded your current quota. Quota exceeded for metric: requests, limit: 0, model: big")
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{hard, nil}}
	var notes []string
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, func(s string) { notes = append(notes, s) })
	if err != nil || len(f.seen) != 2 || f.seen[0].Model != "" || f.seen[1].Model != "sonnet" {
		t.Fatalf("err=%v seen=%+v res=%+v", err, f.seen, res)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "quota-dead") {
		t.Errorf("demotion must be announced: %v", notes)
	}

	// A transient 429 keeps the model: nothing to demote.
	f = &fakeRunner{caps: Caps{Model: Native}, errs: []error{errors.New("HTTP 429 rate limited, try again in 1s"), nil}}
	if _, err := Run(context.Background(), f, RunOptions{Prompt: "p", Model: "opus"}, wire.Discard, nil); err != nil || f.seen[1].Model != "opus" {
		t.Fatalf("transient 429 must retry the same alias: err=%v seen=%+v", err, f.seen)
	}
}

// A 402 (account exhausted: no credits/plan window left) is KindQuota and IsQuota-true but ModelDenied-false:
// a lower alias on the same account is just as exhausted, so Run must not demote and must return the error.
func assertQuotaNoDemote(t *testing.T, acctErr error) {
	t.Helper()
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{acctErr}}
	var notes []string
	_, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, func(s string) { notes = append(notes, s) })
	var e *retry.Error
	if !errors.As(err, &e) || e.Kind != retry.KindQuota {
		t.Fatalf("err = %v, want a KindQuota error", err)
	}
	if len(f.seen) != 1 || f.seen[0].Model != "" {
		t.Fatalf("seen = %+v, want one attempt with the model never demoted off \"\"", f.seen)
	}
	if len(notes) != 0 {
		t.Errorf("account exhaustion must not announce an alias demotion: %v", notes)
	}
}

func TestRunQuotaAccountExhaustedDoesNotDemote(t *testing.T) {
	fastDefaultBackoff(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	assertQuotaNoDemote(t, errors.New("HTTP 402 payment required: you have exceeded your monthly quota"))

	// A body carrying BOTH the 402/payment-required account marker AND a model-denied marker
	// ("insufficient_quota") is still account-exhausted: hardAccountRE vetoes ModelDenied, so Run must not
	// demote off a dead account on the strength of the model-denied marker alone.
	assertQuotaNoDemote(t, errors.New("402 payment required: insufficient_quota"))
}

// TestRunTimeoutEndsKindFatalRegardlessOfTools: Run() no longer special-cases a tools-on timeout (that
// branch was removed as redundant); Do's own AttemptTimeout-is-terminal path is the only one left, and it
// must classify a hung call to KindFatal wrapping context.DeadlineExceeded the same whether Tools was on.
func TestRunTimeoutEndsKindFatalRegardlessOfTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tools := range []bool{true, false} {
		t.Run(fmt.Sprintf("Tools=%v", tools), func(t *testing.T) {
			hang := newCodexHang()
			_, err := Run(context.Background(), hang, RunOptions{Prompt: "p", Tools: tools, Timeout: 20 * time.Millisecond}, wire.Discard, nil)
			var e *retry.Error
			if !errors.As(err, &e) || e.Kind != retry.KindFatal || !errors.Is(err, context.DeadlineExceeded) || hang.calls != 1 {
				t.Fatalf("Tools=%v: calls=%d err=%v, want KindFatal wrapping DeadlineExceeded after one call", tools, hang.calls, err)
			}
		})
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
		{"ok", "", []error{nil}, wire.StatusOK, 1},
		{"retried transient then ok", "", []error{errors.New("HTTP 503 overloaded"), nil}, wire.StatusOK, 2},
		{"quota", "", []error{&retry.Error{Kind: retry.KindQuota, Err: errors.New("plan exhausted")}}, wire.StatusQuota, 1},
		{"partial", "half", []error{errors.New("boom")}, wire.StatusPartial, 1},
		{"stalled", "", []error{dl, dl, dl}, wire.StatusStalled, 3},
		{"error", "", []error{errors.New("bad request")}, wire.StatusError, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{write: c.write, errs: c.errs}
			res, _ := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, nil)
			if res.Status != c.status || res.Transport == nil || res.Transport.Attempts != c.attempts {
				t.Fatalf("status=%q transport=%+v, want %q attempts=%d", res.Status, res.Transport, c.status, c.attempts)
			}
		})
	}
}
