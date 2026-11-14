package waves

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// statefp is a fake fingerprinter over an in-memory file map; a missing key is absent.
func statefp(files map[string]string) FingerprintFunc {
	return func(path string) (string, error) {
		if h, ok := files[path]; ok {
			return h, nil
		}
		return FingerprintAbsent, nil
	}
}

func stateLeaf(id, status, verdict string, files map[string]string) LeafState {
	own := make([]string, 0, len(files))
	for f := range files {
		own = append(own, f)
	}
	slices.Sort(own)
	return LeafState{ID: id, Agent: "codex", Status: status, Verdict: verdict, Owns: own, Fingerprints: files}
}

func stateWave(status string, leaves ...LeafState) State {
	return State{Base: "abc"}.WithWave(WaveState{N: 2, Status: status, Leaves: leaves})
}

func stateActions(t *testing.T, r DecideResult) map[string]DecideAction {
	t.Helper()
	out := map[string]DecideAction{}
	for _, l := range r.Leaves {
		out[l.ID] = l.Action
	}
	return out
}

type stateResumeCase struct {
	name  string
	state State
	fresh bool
	wave  DecideAction
	leaf  map[string]DecideAction
}

func stateResumeCases(ok func(id, f string) LeafState) []stateResumeCase {
	keepRelaunch := map[string]DecideAction{"p-2.1": DecideKeep, "p-2.2": DecideRelaunch}
	launchAll := map[string]DecideAction{"p-2.1": DecideLaunch, "p-2.2": DecideLaunch}
	both := []LeafState{ok("p-2.1", "a.go"), ok("p-2.2", "b.go")}
	bad := func(status, verdict string, files map[string]string) State {
		return stateWave(WaveStateFailed, ok("p-2.1", "a.go"), stateLeaf("p-2.2", status, verdict, files))
	}
	return []stateResumeCase{
		{"wave passed is skipped", stateWave(WaveStatePassed, both...), false, DecideSkip, nil},
		{"ok leaves are kept and verifier re-runs", stateWave(WaveStateFailed, both...), false, DecideVerify,
			map[string]DecideAction{"p-2.1": DecideKeep, "p-2.2": DecideKeep}},
		{"error leaf relaunches alone", bad("error", "", nil), false, DecideRun, keepRelaunch},
		{"timeout leaf relaunches", bad("timeout", "", nil), false, DecideRun, keepRelaunch},
		{"lost leaf (still running) relaunches", bad(WaveStateRunning, "", nil), false, DecideRun, keepRelaunch},
		{"mismatched leaf relaunches", bad(LeafStateOK, LeafStateMismatch, map[string]string{"b.go": "h2"}),
			false, DecideRun, keepRelaunch},
		{"unrecorded leaf launches", stateWave(WaveStateRunning, ok("p-2.1", "a.go")), false, DecideRun,
			map[string]DecideAction{"p-2.1": DecideKeep, "p-2.2": DecideLaunch}},
		{"no state launches everything", State{}, false, DecideRun, launchAll},
		{"fresh discards a passed wave", stateWave(WaveStatePassed, both...), true, DecideRun, launchAll},
	}
}

func checkStateResume(t *testing.T, tc stateResumeCase, ids []string, fp FingerprintFunc) {
	t.Helper()
	r, err := Decide(tc.state, 2, ids, tc.fresh, fp)
	if err != nil {
		t.Fatal(err)
	}
	if r.Wave != tc.wave || r.Fresh != tc.fresh {
		t.Fatalf("wave action = %s fresh=%v, want %s fresh=%v", r.Wave, r.Fresh, tc.wave, tc.fresh)
	}
	got := stateActions(t, r)
	for id, want := range tc.leaf {
		if got[id] != want {
			t.Errorf("leaf %s = %s, want %s (%v)", id, got[id], want, r.Leaves)
		}
	}
}

func TestStateDecideResumeTable(t *testing.T) {
	disk := map[string]string{"a.go": "h1", "b.go": "h2"}
	ok := func(id, f string) LeafState { return stateLeaf(id, LeafStateOK, "", map[string]string{f: disk[f]}) }
	for _, tc := range stateResumeCases(ok) {
		t.Run(tc.name, func(t *testing.T) { checkStateResume(t, tc, []string{"p-2.1", "p-2.2"}, statefp(disk)) })
	}
}

func TestStateDecideChangedOwnedFileDefeatsKeep(t *testing.T) {
	leaf := stateLeaf("p-2.1", LeafStateOK, "", map[string]string{"a.go": "h1", "gone.go": FingerprintAbsent})
	s := stateWave(WaveStateFailed, leaf)
	for name, disk := range map[string]map[string]string{
		"edited":  {"a.go": "h1-edited"},
		"created": {"a.go": "h1", "gone.go": "now-exists"},
		"removed": {},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Decide(s, 2, []string{"p-2.1"}, false, statefp(disk))
			if err != nil {
				t.Fatal(err)
			}
			if got := stateActions(t, r)["p-2.1"]; got != DecideRelaunch || r.Wave != DecideRun {
				t.Fatalf("leaf %s wave %s, want relaunch/run (%v)", got, r.Wave, r.Leaves)
			}
		})
	}
	same := map[string]string{"a.go": "h1"}
	if r, _ := Decide(s, 2, []string{"p-2.1"}, false, statefp(same)); stateActions(t, r)["p-2.1"] != DecideKeep {
		t.Fatalf("unchanged files must keep the leaf: %v", r.Leaves)
	}
}

func stateRoundTripState(t *testing.T) State {
	t.Helper()
	s, err := StateStart("deadbeef", []string{"x.go"}, statefp(map[string]string{"x.go": "hx"}))
	if err != nil {
		t.Fatal(err)
	}
	leaf := stateLeaf("p-2.1", "ok", "", map[string]string{"a.go": "h1"})
	return s.WithWave(WaveState{N: 3, Status: WaveStatePassed}).
		WithWave(WaveState{N: 2, Status: WaveStateRunning}.WithLeaf(leaf))
}

func TestStateSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waves", "slug", "state.json")
	if err := stateRoundTripState(t).Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode = %v, err %v; want 0600", fi.Mode().Perm(), err)
	}
	got, err := StateLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	checkStateRoundTrip(t, got)
	if _, err := StateLoad(path + ".missing"); !os.IsNotExist(err) {
		t.Fatalf("missing state: err = %v, want not-exist", err)
	}
}

func checkStateRoundTrip(t *testing.T, got State) {
	t.Helper()
	if got.Base != "deadbeef" || got.Dirty["x.go"] != "hx" || len(got.Waves) != 2 || got.Waves[0].N != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if l := got.Waves[0].Leaves[0]; l.Fingerprints["a.go"] != "h1" {
		t.Fatalf("leaf fingerprints lost: %+v", l)
	}
}

func TestStateWithCopiesNeverMutate(t *testing.T) {
	a := State{}.WithWave(WaveState{N: 1}.WithLeaf(LeafState{ID: "x", Status: "error"}))
	b := a.WithWave(WaveState{N: 1}.WithLeaf(LeafState{ID: "x", Status: "ok"}))
	if a.Waves[0].Leaves[0].Status != "error" || b.Waves[0].Leaves[0].Status != "ok" {
		t.Fatalf("WithWave mutated its receiver: a=%+v b=%+v", a, b)
	}
	w := WaveState{N: 1}.WithLeaf(LeafState{ID: "x", Status: "error"})
	w2 := w.WithLeaf(LeafState{ID: "x", Status: "ok"}).WithLeaf(LeafState{ID: "y"})
	if w.Leaves[0].Status != "error" || len(w.Leaves) != 1 || len(w2.Leaves) != 2 || w2.Leaves[0].Status != "ok" {
		t.Fatalf("WithLeaf wrong: w=%+v w2=%+v", w, w2)
	}
}

func TestStateTornWriteLeavesOldStateReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := State{Base: "old"}
	if err := old.Save(path); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves a partial temp sibling; the real file is never touched.
	if err := os.WriteFile(path+".tmp-123", []byte(`{"base":"ne`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := StateLoad(path); err != nil || s.Base != "old" {
		t.Fatalf("after torn temp: %+v, %v", s, err)
	}
	// A save that fails (dir not writable) must leave the old state in place.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := (State{Base: "new"}).Save(path); err == nil {
		t.Fatal("Save into a read-only dir succeeded")
	}
	if s, err := StateLoad(path); err != nil || s.Base != "old" {
		t.Fatalf("after failed save: %+v, %v", s, err)
	}
}

// stateGitRepo makes a repo with a committed tracked.go, then edits it and adds new.go.
func stateGitRepo(t *testing.T) (dir string, run func(args ...string) string, write func(name, body string)) {
	t.Helper()
	dir = t.TempDir()
	run = func(args ...string) string {
		t.Helper()
		pre := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}
		cmd := exec.Command("git", append(pre, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write = func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("tracked.go", "one\n")
	run("add", "tracked.go")
	run("commit", "-q", "-m", "init")
	write("tracked.go", "two\n")
	write("new.go", "fresh\n")
	return dir, run, write
}

func TestStateGitRealRepo(t *testing.T) {
	dir, run, write := stateGitRepo(t)
	base, dirty, err := StateGitHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(dirty)
	if base != run("rev-parse", "HEAD") || !slices.Equal(dirty, []string{"new.go", "tracked.go"}) {
		t.Fatalf("base %s dirty %v", base, dirty)
	}
	fp := FingerprintGit(dir)
	checkStateFingerprints(t, fp, run("hash-object", "new.go"))
	s, err := StateStart(base, dirty, fp)
	if err != nil || len(s.Dirty) != 2 || s.Dirty["new.go"] == "" {
		t.Fatalf("StateStart: %+v %v", s, err)
	}
	checkStateKeptUntilEdited(t, s, fp, func() { write("new.go", "edited after the leaf ended\n") })
}

func checkStateFingerprints(t *testing.T, fp FingerprintFunc, wantNew string) {
	t.Helper()
	if got, err := fp("new.go"); err != nil || got != wantNew {
		t.Fatalf("fingerprint %q err %v, want git hash-object", got, err)
	}
	if got, err := fp("nope.go"); err != nil || got != FingerprintAbsent {
		t.Fatalf("missing file fingerprint %q err %v, want absent", got, err)
	}
}

// checkStateKeptUntilEdited: a leaf that ended with new.go as it is stays kept until edit changes the file.
func checkStateKeptUntilEdited(t *testing.T, s State, fp FingerprintFunc, edit func()) {
	t.Helper()
	fps, err := FingerprintAll(fp, []string{"new.go"})
	if err != nil {
		t.Fatal(err)
	}
	s = s.WithWave(WaveState{N: 1, Status: WaveStateFailed, Leaves: []LeafState{
		{ID: "p-1.1", Status: LeafStateOK, Owns: []string{"new.go"}, Fingerprints: fps}}})
	decide := func() DecideAction {
		r, err := Decide(s, 1, []string{"p-1.1"}, false, fp)
		if err != nil {
			t.Fatal(err)
		}
		return r.Leaves[0].Action
	}
	if a := decide(); a != DecideKeep {
		t.Fatalf("unchanged file: %s, want keep", a)
	}
	edit()
	if a := decide(); a != DecideRelaunch {
		t.Fatalf("edited file: %s, want relaunch", a)
	}
}

func TestFingerprintGitRefusesPathsOutsideRepoAndStateDir(t *testing.T) {
	dir, _, _ := stateGitRepo(t)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(paths.StateDir(), "waves", "state.json")
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp := FingerprintGit(dir)
	for _, p := range []string{outside, "../escape.txt", filepath.Join(dir, "..", "x"), "/etc/hosts"} {
		if got, err := fp(p); err == nil {
			t.Errorf("fingerprint(%q) = %q, want a refusal", p, got)
		}
	}
	for _, p := range []string{"new.go", filepath.Join(dir, "new.go"), stateFile} {
		if got, err := fp(p); err != nil || got == "" {
			t.Errorf("fingerprint(%q) = %q, %v, want a hash", p, got, err)
		}
	}
}
