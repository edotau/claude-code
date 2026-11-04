package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var errCodexMissing = errors.New(`exec: "codex": executable file not found in $PATH`)

func fallbackConfigDir(t *testing.T) { t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) }

// TestRunWithFallback wires the classifier into Run: codex failing with no answer must rerun on the
// fallback agent with Fallback set, while a successful or excluded-class codex run must not.
func TestRunWithFallbackFallsBackAndReportsIt(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", errs: []error{errCodexMissing}, emptyAnswer: true}
	claude := &fakeRunner{name: "claude", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
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
}

func TestRunWithFallbackEachDispatchRunsItsOwnCell(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{errCodexMissing}, emptyAnswer: true}
	gemini := &fakeRunner{name: "gemini", caps: Caps{Model: Native}, errs: []error{errCodexMissing}, emptyAnswer: true}
	opencode := &fakeRunner{name: "opencode", caps: Caps{Model: Native}, errs: []error{nil}}
	claude := &fakeRunner{name: "claude", errs: []error{nil}}
	req := RunOptions{Prompt: "p", Model: "gpt-6.1-sol"}
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{codex, gemini, opencode, claude}, "", "gemini-4-argon", "kimi-k3", "opus"), req, Discard, nil)
	if err != nil || res.Agent != "opencode" || res.Fallback != "codex" || len(claude.seen) != 0 {
		t.Fatalf("res=%+v err=%v claude.seen=%d", res, err, len(claude.seen))
	}
	if !strings.Contains(res.FallbackReason, "codex: ") || !strings.Contains(res.FallbackReason, "; gemini: ") {
		t.Errorf("FallbackReason = %q, want one entry per failed dispatch", res.FallbackReason)
	}
	if codex.seen[0].Model != "gpt-6.1-sol" || gemini.seen[0].Model != "gemini-4-argon" || opencode.seen[0].Model != "kimi-k3" {
		t.Errorf("models = %q / %q / %q, want the caller's then each cell's", codex.seen[0].Model, gemini.seen[0].Model, opencode.seen[0].Model)
	}
}

func TestRunWithFallbackKeepsAliasOnClaudeDispatch(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{errCodexMissing}, emptyAnswer: true}
	claude := &fakeRunner{name: "claude", caps: Caps{Model: Native}, errs: []error{nil}}
	if _, err := RunWithFallback(context.Background(), chainOf([]Engine{codex, claude}, "", "opus"), RunOptions{Prompt: "p", Model: "claude-opus-5.5"}, Discard, nil); err != nil {
		t.Fatal(err)
	}
	if claude.seen[0].Model != "opus" {
		t.Errorf("claude dispatch ran %q, want the opus cell", claude.seen[0].Model)
	}
}

func TestRunWithFallbackGateSkipsMakeZeroAttempts(t *testing.T) {
	fallbackConfigDir(t)
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
}

func TestRunWithFallbackQuotaMarksAccountAndMovesOn(t *testing.T) {
	fallbackConfigDir(t)
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	copilot := &fakeRunner{name: "copilot", errs: []error{quota}, emptyAnswer: true}
	claude := &fakeRunner{name: "claude", errs: []error{nil}}
	c := chainOf([]Engine{copilot, claude}, "", "sonnet")
	c.Dispatches[0].Account, c.Dispatches[0].Billing = "github·u", "credits"
	res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || res.Agent != "claude" || len(copilot.seen) != 1 {
		t.Fatalf("res=%+v err=%v copilot attempts=%d (quota is never retried)", res, err, len(copilot.seen))
	}
	if e := loadState()["github·u"]; e.Status(time.Now()) != "exhausted" || e.ExhaustedUntil.Day() != 1 {
		t.Errorf("state = %+v, want exhausted until the 1st", e)
	}
}

func TestRunWithFallbackEmptyChainMeansNoFallback(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", errs: []error{errCodexMissing}, emptyAnswer: true}
	res, err := RunWithFallback(context.Background(), Single(codex), RunOptions{Prompt: "p"}, Discard, nil)
	if err == nil || res.Fallback != "" || res.Agent != "codex" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRunWithFallbackSuccessNeedsNoFallback(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", errs: []error{nil}}
	claude := &fakeRunner{name: "claude", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || res.Fallback != "" || res.Agent != "codex" || len(claude.seen) != 0 {
		t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
	}
}

func TestRunWithFallbackPromptTooLargeNotRetried(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", errs: []error{fmt.Errorf("p: %w", ErrPromptTooLarge)}, emptyAnswer: true}
	claude := &fakeRunner{name: "claude", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{codex, claude}), RunOptions{Prompt: "p"}, Discard, nil)
	if err == nil || res.Fallback != "" || len(claude.seen) != 0 {
		t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
	}
}

func TestRunWithFallbackExplicitNonClaudeModelNeverOverridden(t *testing.T) {
	fallbackConfigDir(t)
	codex := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{errCodexMissing}, emptyAnswer: true}
	claude := &fakeRunner{name: "claude", errs: []error{nil}}
	req := RunOptions{Prompt: "p", Model: "openai:gpt-4"}
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{codex, claude}), req, Discard, nil)
	if err == nil || res.Fallback != "" || len(claude.seen) != 0 {
		t.Fatalf("res=%+v err=%v claude.seen=%+v", res, err, claude.seen)
	}
}
