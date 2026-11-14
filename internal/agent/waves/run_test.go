package waves

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

type runFixture struct {
	gateFail, verifyFail, cancelVerify bool
	o                                  Options
	h                                  runHooks
	out                                bytes.Buffer
	calls                              [][]Spec
	fail, outside, mismatch            bool
	gates                              int
	cancel                             context.CancelFunc
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := verifyRepo(t, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})
	plan := filepath.Join(dir, "toy.jsonl")
	verifyWrite(t, dir, "toy.jsonl", `{"id":"1.1","wave":1,"agent":"codex","owns":["a.txt"],"task":"first","verify":"check first"}
{"id":"1.2","wave":1,"agent":"codex","owns":["b.txt"],"task":"second","verify":"check second"}
{"id":"1.V","wave":1,"agent":"in-context"}
{"wave":1,"gate":"gate"}
{"id":"2.1","wave":2,"agent":"codex","owns":["c.txt"],"task":"third"}
`)
	f := &runFixture{o: Options{Plan: plan, Dir: dir}}
	f.o.Out, f.o.Err = &f.out, &f.out
	f.h = runHooks{launch: f.launch, check: CheckEnv{Diff: f.diff, Exec: f.exec, ModTime: verifyModTimes(50)}}
	return f
}

func (f *runFixture) launch(_ context.Context, _ string, specs []Spec, o LaunchOptions) ([]Envelope, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	f.calls = append(f.calls, specs)
	var out []Envelope
	for _, s := range specs {
		status := "ok"
		if f.fail && s.ID == "1.2" {
			status = "error"
		}
		out = append(out, Envelope{TaskID: o.Slug + "-" + s.ID, DispatchID: s.ID, Status: status, Report: "report-" + s.ID})
	}
	if f.cancel != nil {
		f.cancel()
	}
	return out, nil
}

func (f *runFixture) diff(o agent.DiffOptions) int {
	rep := agent.DiffReport{}
	if o.Verify != "" {
		rep = verifyWindow(0, 30)
		rep.Verify.ID = o.Verify
		rep.Verify.Mismatch = f.mismatch
		if f.outside && o.Verify == "1.1" {
			rep = verifyWindow(40, 60, "b.txt")
		}
	}
	if err := json.NewEncoder(o.Out).Encode(rep); err != nil {
		return 2
	}
	return 0
}

func (f *runFixture) exec(_ context.Context, _, cmd string, _ []string, _ io.Writer) (int, error) {
	f.gates++
	if f.cancelVerify && f.cancel != nil {
		f.cancel()
		return -1, context.Canceled
	}
	if (f.gateFail && cmd == "gate") || (f.verifyFail && cmd == "check first") {
		return 1, nil
	}
	return 0, nil
}

func TestRunEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		fail, outside, mismatch bool
		code                    int
	}{
		{name: "pass"}, {name: "leaf", fail: true, code: 1}, {name: "outside", outside: true, code: 3},
		{name: "routing", mismatch: true, code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.fail, f.outside, f.mismatch = tc.fail, tc.outside, tc.mismatch
			if code := runWith(t.Context(), f.o, f.h); code != tc.code {
				t.Fatalf("exit %d want %d: %s", code, tc.code, &f.out)
			}
			want := 2
			if tc.code != 0 {
				want = 1
			}
			if len(f.calls) != want {
				t.Fatalf("launched later wave: %+v", f.calls)
			}
			if tc.outside && (!strings.Contains(f.out.String(), "b.txt") || !strings.Contains(f.out.String(), "1.1")) {
				t.Fatalf("missing attribution: %s", &f.out)
			}
			if tc.code != 0 && !strings.Contains(f.out.String(), "next:") {
				t.Fatal("missing rerun")
			}
		})
	}
}

func TestRunResume(t *testing.T) {
	f := newRunFixture(t)
	f.fail = true
	if code := runWith(t.Context(), f.o, f.h); code != 1 {
		t.Fatal(code, &f.out)
	}
	f.fail = false
	f.calls = nil
	f.out.Reset()
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatal(code, &f.out)
	}
	if len(f.calls) != 2 || len(f.calls[0]) != 1 || f.calls[0][0].ID != "1.2" {
		t.Fatalf("resume: %+v", f.calls)
	}
	f.o.Status = true
	f.calls = nil
	if code := runWith(t.Context(), f.o, f.h); code != 0 || len(f.calls) != 0 {
		t.Fatal("status launched", code)
	}
}

func TestRunRefusalAndCancel(t *testing.T) {
	for _, mode := range []string{"owns", "gate", "cancel", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := newRunFixture(t)
			want := 2
			ctx := t.Context()
			switch mode {
			case "owns":
				verifyWrite(t, f.o.Dir, "toy.jsonl", `{"id":"1.1","wave":1,"agent":"codex","owns":["../x"]}`)
			case "gate":
				f.o.Gate = "git commit -m forbidden"
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				f.cancel = cancel
				want = 4
			case "dry-run":
				f.o.DryRun = true
				want = 0
			}
			if code := runWith(ctx, f.o, f.h); code != want {
				t.Fatalf("exit %d want %d: %s", code, want, &f.out)
			}
			if mode != "cancel" && len(f.calls) != 0 {
				t.Fatal("unexpected launch")
			}
		})
	}
}

func TestRunRealPlanDryRun(t *testing.T) {
	f := newRunFixture(t)
	f.o.Plan = "../../../plans/2026-11-13-permissions-settings-toolbox.md"
	f.o.DryRun, f.o.JSON = true, true
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatal(code, &f.out)
	}
	var s State
	if err := json.Unmarshal(f.out.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, w := range s.Waves {
		count += len(w.Packages)
	}
	if len(s.Waves) != 4 || count != 42 || len(f.calls) != 0 {
		t.Fatal(fmt.Sprint(len(s.Waves), count, f.calls))
	}
}

func TestRunGateAndVerifierPrecedence(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(strconv.FormatBool(task), func(t *testing.T) {
			f := newRunFixture(t)
			f.outside = true
			f.gateFail, f.verifyFail = !task, task
			if code := runWith(t.Context(), f.o, f.h); code != 1 || len(f.calls) != 1 {
				t.Fatal(code, &f.out)
			}
		})
	}
}

func TestRunCancelledVerifier(t *testing.T) {
	f := newRunFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.h.launch = func(ctx context.Context, executable string, specs []Spec, o LaunchOptions) ([]Envelope, error) {
		result, err := f.launch(ctx, executable, specs, o)
		f.cancel = cancel
		return result, err
	}
	f.cancelVerify = true
	if code := runWith(ctx, f.o, f.h); code != 4 {
		t.Fatal(code, &f.out)
	}
}
