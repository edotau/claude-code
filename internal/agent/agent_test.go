package agent

import (
	"context"
	"errors"
	"io"
	"reflect"
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
	caps  Caps
	errs  []error
	write string
	seen  []Request
}

func (f *fakeRunner) Name() string     { return "fake" }
func (f *fakeRunner) Caps() Caps       { return f.caps }
func (f *fakeRunner) Available() error { return nil }
func (f *fakeRunner) Run(_ context.Context, req Request, w io.Writer) (Result, error) {
	f.seen = append(f.seen, req)
	io.WriteString(w, f.write)
	err := f.errs[len(f.seen)-1]
	return Result{Answer: "a"}, err
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
