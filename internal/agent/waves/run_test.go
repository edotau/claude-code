package waves

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/inspect"
	"github.com/edotau/claude-code/internal/paths"
)

type runFixture struct {
	gateFail, verifyFail, cancelVerify bool
	o                                  Options
	h                                  runHooks
	out                                bytes.Buffer
	calls                              [][]Spec
	fail, outside, mismatch            bool
	failure                            string
	foreign                            map[string][]CheckViolation // package id -> other packages' paths it wrote alone
	parallel, scoped                   bool                        // 1.1 and 1.2 ran in one window, and 1.1 wrote 1.2's file; 1.1 was refused it
	gates                              int
	cancel                             context.CancelFunc
	unowned                            []string // the union check's changed files outside every owner
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
			status = f.failure
			if status == "" {
				status = "error"
			}
		}
		out = append(out, Envelope{TaskID: o.Slug + "-" + s.ID, DispatchID: s.ID, Status: status, Report: "report-" + s.ID,
			Scoped: f.scoped, Foreign: f.foreign[s.ID]})
	}
	if f.cancel != nil {
		f.cancel()
	}
	return out, nil
}

func (f *runFixture) diff(o inspect.DiffOptions) int {
	rep := inspect.DiffReport{Outside: f.unowned}
	if o.Verify != "" {
		rep = verifyWindow(0, 30)
		rep.Verify.ID = o.Verify
		rep.Verify.Mismatch = f.mismatch
		if f.outside && o.Verify == "1.1" {
			rep = verifyWindow(40, 60, "b.txt")
		}
		if f.parallel {
			rep = verifyWindow(0, 60)
			if o.Verify == "1.1" {
				rep = verifyWindow(0, 60, "b.txt")
			}
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
	f := newRunFixture(t)
	f.failure = "stalled"
	for _, tc := range []struct {
		name                    string
		fail, outside, mismatch bool
		code                    int
	}{
		{name: "pass"}, {name: "leaf", fail: true, code: 1}, {name: "outside", outside: true, code: 3},
		{name: "routing", mismatch: true, code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			f.calls, f.gates = nil, 0
			f.out.Reset()
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
	f.failure = "partial"
	f.fail = true
	if code := runWith(t.Context(), f.o, f.h); code != 1 {
		t.Fatal(code, &f.out)
	}
	if f.gates != 2 {
		t.Fatalf("successful leaf and wave gate were not verified: %d steps", f.gates)
	}
	f.fail = false
	f.calls = nil
	f.out.Reset()
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatal(code, &f.out)
	}
	if !strings.Contains(f.out.String(), "no verify: 2.1 (diff check only)") {
		t.Fatalf("missing verify notice: %s", &f.out)
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

// A resume launches only the failed leaf; it keeps the profile its original position rotated to.
func TestRunResumeKeepsProfileRotation(t *testing.T) {
	f := newRunFixture(t)
	f.o.Profiles = []string{"dev", "qa"}
	f.fail = true
	if code := runWith(t.Context(), f.o, f.h); code != 1 {
		t.Fatal(code, &f.out)
	}
	f.fail, f.calls = false, nil
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatal(code, &f.out)
	}
	if got := f.calls[0][0]; got.ID != "1.2" || got.Profile != "qa" {
		t.Fatalf("resumed leaf = %s on %q, want 1.2 on qa", got.ID, got.Profile)
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
	f.o.Plan = filepath.Join("testdata", "plan.md")
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
	if len(s.Waves) != 4 || count != 6 || len(f.calls) != 0 {
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

func TestRunForeignWriteInOneParallelWindowIsNeverPassed(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(strconv.FormatBool(scoped), func(t *testing.T) {
			f := newRunFixture(t)
			f.parallel, f.scoped = true, scoped
			code := runWith(t.Context(), f.o, f.h)
			if scoped && code != 0 {
				t.Fatalf("scoped leaves cannot write each other's files: exit %d\n%s", code, &f.out)
			}
			if !scoped && (code != 3 || !strings.Contains(f.out.String(), "unattributed: b.txt") ||
				!strings.Contains(f.out.String(), "scope: 1.1,1.2 cannot refuse a foreign write")) {
				t.Fatalf("unscoped parallel write: exit %d, want 3 naming b.txt and the scope notice\n%s", code, &f.out)
			}
		})
	}
}

func TestRunLeafThatWroteAlonePathsOfAnotherPackageFailsTheWave(t *testing.T) {
	f := newRunFixture(t)
	f.foreign = map[string][]CheckViolation{"1.1": {{Package: "1.1", File: "b.txt", Owners: []string{"1.2"}}}}
	if code := runWith(t.Context(), f.o, f.h); code != 3 || !strings.Contains(f.out.String(), "1.1 wrote b.txt (owner 1.2)") {
		t.Fatalf("exit %d, want 3 naming 1.1, b.txt and owner 1.2\n%s", code, &f.out)
	}
}

func TestRunFileDirtiedBeforeTheWaveIsNotOutside(t *testing.T) {
	f := newRunFixture(t)
	f.o.From, f.o.To = 1, 1
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatalf("wave 1: exit %d\n%s", code, &f.out)
	}
	verifyWrite(t, f.o.Dir, "d.txt", "main session edit between waves")
	f.unowned, f.o.From, f.o.To = []string{"d.txt"}, 2, 2
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatalf("a file dirty before wave 2 launched failed it: exit %d\n%s", code, &f.out)
	}
}

// A pre-hash <slug>/state.json of this plan moves to the keyed path; another plan's stays put.
func TestPrepareAdoptsLegacyState(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := filepath.Join(paths.StateDir(), "waves", "my-plan", "state.json")
	if err := (State{Plan: "/elsewhere/my-plan.md"}).Save(old); err != nil {
		t.Fatal(err)
	}
	r := &runner{o: Options{Dir: t.TempDir(), Plan: "/x/my-plan.md"}}
	if err := r.prepare(); err != nil || r.path == old {
		t.Fatalf("foreign legacy state adopted: path %q err %v", r.path, err)
	}
	if err := (State{Plan: "/x/my-plan.md", Base: "abc"}).Save(old); err != nil {
		t.Fatal(err)
	}
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	if s, err := StateLoad(r.path); err != nil || s.Base != "abc" || r.path == old {
		t.Fatalf("legacy state not moved to %q: %+v %v", r.path, s, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy state left behind: %v", err)
	}
}

func TestPrepareKeepsUndatedSlugAndFixesDay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	r := &runner{o: Options{Dir: t.TempDir(), Plan: "/x/my-plan.md"}}
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.path, paths.StateDir()) {
		t.Errorf("state path %q is outside the harness state tree", r.path)
	}
	if r.slug != "my-plan" || !strings.HasPrefix(filepath.Base(r.dir), "my-plan-") || r.day.IsZero() {
		t.Errorf("slug %q path %q day %v", r.slug, r.path, r.day)
	}
	other := &runner{o: Options{Dir: r.o.Dir, Plan: "/y/my-plan.md"}}
	if err := other.prepare(); err != nil || other.path == r.path {
		t.Errorf("same-named plans share state %q (err %v)", r.path, err)
	}
}

// A second run of one plan is refused while the first holds the lock; a copied foreign state is refused.
func TestRunLockAndForeignState(t *testing.T) {
	f := newRunFixture(t)
	r := &runner{o: f.o}
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	unlock, err := r.lock()
	if err != nil {
		t.Fatal(err)
	}
	if code := runWith(t.Context(), f.o, f.h); code != 1 || len(f.calls) != 0 || !strings.Contains(f.out.String(), "another run") {
		t.Fatalf("locked run: exit %d calls %d\n%s", code, len(f.calls), &f.out)
	}
	unlock()
	if err := (State{Plan: "/elsewhere/toy.jsonl"}).Save(r.path); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := runWith(t.Context(), f.o, f.h); code != 1 || len(f.calls) != 0 || !strings.Contains(f.out.String(), "belongs to plan") {
		t.Fatalf("foreign state: exit %d calls %d\n%s", code, len(f.calls), &f.out)
	}
}

// A resume that relaunches 1.1 relaunches its kept dependent 1.2 too, whatever their order in the plan.
func TestRunResumeRelaunchesStaleDependent(t *testing.T) {
	first := `{"id":"1.1","wave":1,"agent":"codex","owns":["a.txt"],"task":"first"}` + "\n"
	second := `{"id":"1.2","wave":1,"agent":"codex","owns":["b.txt"],"task":"second","blocked_by":["1.1"]}` + "\n"
	for name, plan := range map[string]string{"blocker first": first + second, "dependent first": second + first} {
		t.Run(name, func(t *testing.T) { resumeRelaunchesDependent(t, plan) })
	}
}

func resumeRelaunchesDependent(t *testing.T, plan string) {
	f := newRunFixture(t)
	verifyWrite(t, f.o.Dir, "toy.jsonl", plan)
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatal(code, &f.out)
	}
	r := &runner{o: f.o}
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	s, err := StateLoad(r.path)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := s.Wave(1)
	i := slices.IndexFunc(w.Leaves, func(l LeafState) bool { return strings.HasSuffix(l.ID, "1.1") })
	w.Status, w.Leaves[i].Status = WaveStateFailed, "error"
	if err := s.WithWave(w).Save(r.path); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if code := runWith(t.Context(), f.o, f.h); code != 0 {
		t.Fatal(code, &f.out)
	}
	if len(f.calls) != 2 || f.calls[0][0].ID != "1.1" || f.calls[1][0].ID != "1.2" {
		t.Fatalf("resume launched %+v, want 1.1 then its dependent 1.2", f.calls)
	}
}
