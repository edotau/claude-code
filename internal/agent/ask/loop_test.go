package ask

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// fakeLoop scripts the planner's answers and each part's status; it records every run call.
type fakeLoop struct {
	plans   []loopPlan
	planned int
	status  func(id string, retry bool) string
	runs    []string // "<id>" or "<id>+retry" per part run
}

func (f *fakeLoop) cfg(out *bytes.Buffer, rounds int) loopCfg {
	return loopCfg{Rounds: rounds, Planner: "claude", Out: out, Log: out,
		plan: func(context.Context, string, string) (loopPlan, wire.Usage, error) {
			p := f.plans[min(f.planned, len(f.plans)-1)]
			f.planned++
			return p, wire.Usage{InputTokens: 1}, nil
		},
		run: func(_ context.Context, _ int, parts []loopPart, retry bool, emit func(wire.Envelope)) {
			for _, p := range parts {
				tag := p.ID
				if retry {
					tag += "+retry"
				}
				f.runs = append(f.runs, tag)
				st, ran := wire.StatusOK, "codex"
				if f.status != nil {
					st = f.status(p.ID, retry)
				}
				if retry {
					ran = "claude"
				}
				emit(wire.Envelope{Status: st, TaskID: p.ID, DispatchID: "d-" + tag, Agent: ran, Answer: "ans " + p.ID})
			}
		}}
}

func parts(ids ...string) loopPlan {
	var p loopPlan
	for _, id := range ids {
		p.Parts = append(p.Parts, loopPart{ID: id, Prompt: "do " + id})
	}
	return p
}

var planDone = loopPlan{Done: true, Summary: "all done"}

func loopTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// Two planned rounds then done: both rounds run, exit 0, state holds both.
func TestLoopTwoRoundsThenDone(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{parts("a", "b"), parts("c"), planDone}}
	var out bytes.Buffer
	st := newLoopFile("goal")
	if rc := runLoop(context.Background(), f.cfg(&out, 4), st); rc != 0 {
		t.Fatalf("exit %d, want 0\n%s", rc, out.String())
	}
	got, err := loadLoop(st.ID)
	if err != nil || len(got.Rounds) != 2 || got.Reason != loopDone || got.Summary != "all done" {
		t.Fatalf("state = %+v, %v", got, err)
	}
}

// A failed part is retried once on claude and succeeds; without retryFailed it stays "error" and the loop never ends 0.
func TestLoopFailedPartRetriedOnClaude(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{parts("a"), planDone}, status: func(_ string, retry bool) string {
		if retry {
			return wire.StatusOK
		}
		return wire.StatusStalled
	}}
	var out bytes.Buffer
	st := newLoopFile("goal")
	if rc := runLoop(context.Background(), f.cfg(&out, 4), st); rc != 0 {
		t.Fatalf("exit %d, want 0\n%s", rc, out.String())
	}
	got, _ := loadLoop(st.ID)
	r := got.Rounds[0].Parts[0]
	if strings.Join(f.runs, ",") != "a,a+retry" || !r.Retried || r.Status != wire.StatusOK || len(r.Dispatches) != 2 {
		t.Fatalf("runs %v, result %+v", f.runs, r)
	}
}

// A part that fails on the retry too is needs_main: exit 1 and one line naming it.
func TestLoopPartFailingTwiceNeedsMain(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{parts("a"), planDone}, status: func(string, bool) string { return wire.StatusError }}
	var out bytes.Buffer
	if rc := runLoop(context.Background(), f.cfg(&out, 4), newLoopFile("goal")); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(out.String(), "needs main: a\td-a,d-a+retry\tdo a") {
		t.Fatalf("output lacks the needs-main line:\n%s", out.String())
	}
}

// A typed model keeps a failed part where it is: no claude retry, reported as needs_main.
func TestLoopTypedModelIsNotMoved(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{parts("a"), planDone}, status: func(string, bool) string { return wire.StatusError }}
	var out bytes.Buffer
	cfg := f.cfg(&out, 4)
	cfg.TypedModel = true
	if rc := runLoop(context.Background(), cfg, newLoopFile("goal")); rc != 1 || len(f.runs) != 1 {
		t.Fatalf("exit %d, runs %v: want 1 and a single run", rc, f.runs)
	}
}

// Overlapping owns refuse the round before any part runs.
func TestLoopOverlappingOwnsRefused(t *testing.T) {
	loopTestEnv(t)
	p := parts("a", "b")
	p.Parts[0].Owns, p.Parts[1].Owns = []string{"/r/x"}, []string{"/r/x/y.go"}
	f := &fakeLoop{plans: []loopPlan{p}}
	var out bytes.Buffer
	if rc := runLoop(context.Background(), f.cfg(&out, 4), newLoopFile("goal")); rc != 1 || len(f.runs) != 0 {
		t.Fatalf("exit %d, runs %v: want 1 and none\n%s", rc, f.runs, out.String())
	}
	if !strings.Contains(out.String(), "both own") {
		t.Fatalf("no overlap message:\n%s", out.String())
	}
}

// The round cap exits 4 with state; --loop-resume's state continues from the next round (it fails without loadLoop and the saved rounds).
func TestLoopRoundCapThenResume(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{parts("a"), parts("b"), planDone}}
	var out bytes.Buffer
	st := newLoopFile("goal")
	if rc := runLoop(context.Background(), f.cfg(&out, 1), st); rc != 4 {
		t.Fatalf("exit %d, want 4\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "--loop-resume "+st.ID) {
		t.Fatalf("no resume command:\n%s", out.String())
	}
	saved, err := loadLoop(st.ID)
	if err != nil || len(saved.Rounds) != 1 {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	if rc := runLoop(context.Background(), f.cfg(&out, 2), saved); rc != 0 {
		t.Fatalf("resume exit %d, want 0\n%s", rc, out.String())
	}
	if got, _ := loadLoop(st.ID); len(got.Rounds) != 2 || strings.Join(f.runs, ",") != "a,b" {
		t.Fatalf("rounds %d, runs %v: resume must not re-run round 1", len(got.Rounds), f.runs)
	}
}

// An interrupted round's unfinished parts are re-run on resume, and only those.
func TestLoopResumeRerunsOnlyUnfinishedParts(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{planDone}}
	st := newLoopFile("goal")
	st.Rounds = []loopRound{{N: 1, Parts: []loopResult{
		{Part: loopPart{ID: "a", Prompt: "do a"}, Status: wire.StatusOK},
		{Part: loopPart{ID: "b", Prompt: "do b"}, Status: wire.StatusError, Kind: "canceled"}}}}
	var out bytes.Buffer
	if rc := runLoop(context.Background(), f.cfg(&out, 2), st); rc != 0 || strings.Join(f.runs, ",") != "b" {
		t.Fatalf("exit %d, runs %v: want 0 and only b", rc, f.runs)
	}
}

// Identical plans in two rounds stop the loop instead of spending another round.
func TestLoopIdenticalPlansStopNoProgress(t *testing.T) {
	loopTestEnv(t)
	f := &fakeLoop{plans: []loopPlan{parts("a"), parts("a"), planDone}}
	var out bytes.Buffer
	st := newLoopFile("goal")
	if rc := runLoop(context.Background(), f.cfg(&out, 4), st); rc != 1 || len(f.runs) != 1 {
		t.Fatalf("exit %d, runs %v: want 1 and one run", rc, f.runs)
	}
	if got, _ := loadLoop(st.ID); got.Reason != loopNoProgress {
		t.Fatalf("reason %q", got.Reason)
	}
}

// Inside a dispatch --loop exits 2 before anything runs.
func TestLoopRefusedInsideDispatch(t *testing.T) {
	loopTestEnv(t)
	t.Setenv("HARNESS_DISPATCH_ID", "20261120T000000.000-codex")
	if rc := cmdTo([]string{"--loop", "--dry-run", "x"}, &bytes.Buffer{}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

// --loop --dry-run prints the planner route, the schema and the round-1 prompt, and exits 0.
func TestLoopDryRun(t *testing.T) {
	loopTestEnv(t)
	t.Setenv("HARNESS_DISPATCH_ID", "")
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	var out bytes.Buffer
	if rc := cmdTo([]string{"--loop", "--dry-run", "my goal"}, &out); rc != 0 {
		t.Fatalf("exit %d\n%s", rc, out.String())
	}
	for _, want := range []string{"planner claude", "--- schema ---", "my goal", "round 1 of at most 4"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out.String())
		}
	}
}
