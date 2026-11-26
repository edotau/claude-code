package plans

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
)

const fillPlan, fillKey = "2026-11-26-fill", fillPlan + " Phase 1.1"

// autofillSpawned is one faked spawn: its args, its batch line and whether the plan held the claim at that moment.
type autofillSpawned struct {
	args    []string
	line    map[string]any
	claimed bool
}

// autofillFixture writes one ready task (leaf, brief text; "" brief = needs a brief) and fakes spawn, health and spend.
func autofillFixture(t *testing.T, leaf, brief string, health DispatchState) (AutofillInput, *[]autofillSpawned) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(AutofillEnv, "")
	root, dir := hosttest.Repo(t), t.TempDir()
	plan := filepath.Join(dir, fillPlan+".md")
	// a bare id: Ready keys it "Phase 1.1" and the claim must find "1.1"
	line := "- [ ] 1.1 One — leaf: " + leaf + " — verify: go test ./a — owns: a/a.go"
	if brief != "" {
		path := filepath.Join(dir, "brief.md")
		writeFile(t, path, brief)
		line += " — brief: " + path
	}
	writeFile(t, plan, line+"\n")
	var spawned []autofillSpawned
	oldSpawn, oldBin, oldHealth := autofillSpawn, autofillBin, DispatchHealth
	t.Cleanup(func() {
		autofillSpawn, autofillBin, DispatchHealth = oldSpawn, oldBin, oldHealth
	})
	autofillBin = func() (string, error) { return "/bin/true", nil }
	DispatchHealth = func(TaskKey) DispatchState { return health }
	autofillSpawn = func(_ string, args []string, _ string, _ *os.File) int {
		p, _ := Parse(plan)
		s := autofillSpawned{args: args, claimed: len(p.Tasks) == 1 && strings.Contains(p.Tasks[0].Claim, AutofillOrigin)}
		b, _ := os.ReadFile(args[slices.Index(args, "--batch")+1])
		_ = json.Unmarshal(b, &s.line)
		spawned = append(spawned, s)
		return os.Getpid() // a live pid: the run counts as going until a test says otherwise
	}
	return AutofillInput{Root: root, Plans: []string{plan}, Slots: 2}, &spawned
}

// matches is a spawn claimed first whose line is leaf's autofill line for fillKey and whose only setting is model.
func (s autofillSpawned) matches(leaf, model string) bool {
	id, _ := s.line["id"].(string)
	ov, _ := s.line["overlay"].(map[string]any)
	ok := s.claimed && strings.HasPrefix(id, AutofillOrigin+"-") && len(ov) == 1 && ov["model"] == model &&
		!slices.Contains(s.args, "--overlay") && !slices.Contains(s.args, "--model")
	for k, v := range map[string]any{"plan_task": fillKey, "prompt": "do the thing\n", "agent": leaf} {
		ok = ok && s.line[k] == v
	}
	return ok
}

// one ready task goes out once, claimed first, at its family's sonnet tier, as --overlay only, origin autofill.
func TestAutofillSendsOnceClaimedFirst(t *testing.T) {
	for leaf, model := range map[string]string{"claude": "sonnet", "codex": "terra"} {
		in, spawned := autofillFixture(t, leaf, "do the thing\n", DispatchState{})
		if res := Autofill(in); len(res.Sent) != 1 || len(*spawned) != 1 {
			t.Fatalf("%s: sent %v, spawns %d, want one", leaf, res.Sent, len(*spawned))
		}
		if s := (*spawned)[0]; !s.matches(leaf, model) {
			t.Errorf("%s: claimed %v, args %v, line %v; want a claimed %s autofill line at %s", leaf, s.claimed, s.args, s.line, fillKey, model)
		}
		if again := Autofill(in); len(again.Sent) != 0 || len(*spawned) != 1 {
			t.Errorf("%s: a second trigger sent %v again", leaf, again.Sent)
		}
	}
}

// every hold sends nothing: two failed attempts, no brief, an over-tier ask, the spend cap and the off key.
func TestAutofillHolds(t *testing.T) {
	for _, tc := range []struct {
		name, leaf, brief, off, want string
		health                       DispatchState
	}{
		{"attempts", "claude", "x\n", "", "held " + fillKey + ": 2 failed attempts", DispatchState{Attempts: 2}},
		{"needs brief", "claude", "", "", "needs a brief", DispatchState{}},
		{"over tier leaf", "claude opus", "x\n", "", "asks for opus", DispatchState{}},
		{"over tier family", "gemini gemini-4-pro", "x\n", "", "asks for gemini-4-pro", DispatchState{}},
		{"over tier brief", "claude", "Tier: claude opus\nx\n", "", "asks for opus", DispatchState{}},
		{"off", "claude", "x\n", "off", "", DispatchState{}},
	} {
		in, spawned := autofillFixture(t, tc.leaf, tc.brief, tc.health)
		t.Setenv(AutofillEnv, tc.off)
		res := Autofill(in)
		if len(res.Sent) != 0 || len(*spawned) != 0 {
			t.Errorf("%s: sent %v", tc.name, res.Sent)
		}
		if got := strings.Join(res.Lines, "\n"); tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: lines %q, want %q", tc.name, got, tc.want)
		}
		if tc.name == "needs brief" && len(res.Held) != 1 {
			t.Errorf("needs brief: held %v, want it announced only", res.Held)
		}
	}
}

func statErr(p string) error { _, err := os.Stat(p); return err }

// concludeBatch writes the batch's log envelope as its run would on finishing with status.
func concludeBatch(t *testing.T, s autofillSpawned, status string) {
	t.Helper()
	id, _ := s.line["id"].(string)
	writeFile(t, filepath.Join(autofillDir(), id+".log"), `{"task_id":"`+id+`","status":"`+status+`"}`+"\n")
}

// a failed run's claim is released and the attempt count, not the claim, decides the retry; an ok run keeps it.
func TestAutofillReleasesConcludedClaims(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	failed := 0
	DispatchHealth = func(TaskKey) DispatchState { return DispatchState{Attempts: failed} }
	in.Slots = 9 // recentSpawns counts the fresh batches; the slots are not what this test checks
	for i, status := range []string{"error", "error"} {
		if res := Autofill(in); len(res.Sent) != 1 {
			t.Fatalf("run %d: sent %v, lines %v", i, res.Sent, res.Lines)
		}
		concludeBatch(t, (*spawned)[i], status)
		failed++
	}
	res := Autofill(in)
	if len(res.Sent) != 0 || !strings.Contains(strings.Join(res.Lines, "\n"), "released "+fillKey) ||
		!strings.Contains(strings.Join(res.Held, "\n"), "2 failed attempts") {
		t.Fatalf("after two failures: sent %v, lines %v, held %v", res.Sent, res.Lines, res.Held)
	}
	in2, spawned2 := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	Autofill(in2)
	concludeBatch(t, (*spawned2)[0], "ok")
	if res := Autofill(in2); len(res.Sent) != 0 || len(res.Lines) != 0 {
		t.Fatalf("an ok run: sent %v, lines %v, want its claim kept", res.Sent, res.Lines)
	}
	old := filepath.Join(autofillDir(), "autofill-old.log")
	writeFile(t, old, "x")
	week := time.Now().Add(-autofillKeep - time.Hour)
	if err := os.Chtimes(old, week, week); err != nil {
		t.Fatal(err)
	}
	if Autofill(in2); !errors.Is(statErr(old), fs.ErrNotExist) {
		t.Fatalf("%s outlived the %s keep", old, autofillKeep)
	}
}

// a batch with no envelope is released only once it is past autofillStale and its process is gone.
func TestAutofillReleasesCrashedBatch(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	in.Slots = 9
	Autofill(in)
	id, _ := (*spawned)[0].line["id"].(string)
	batch := filepath.Join(autofillDir(), id+".jsonl")
	old := time.Now().Add(-autofillStale - time.Minute)
	if err := os.Chtimes(batch, old, old); err != nil {
		t.Fatal(err)
	}
	// the pid file names this live test process; proc.Alive reports no pid alive off unix
	if res := Autofill(in); runtime.GOOS != "windows" && len(res.Sent) != 0 {
		t.Fatalf("a live old batch was released: sent %v, lines %v", res.Sent, res.Lines)
	}
	writeFile(t, filepath.Join(autofillDir(), id+".pid"), "0")
	var failed []TaskKey
	oldFailed := DispatchFailed
	t.Cleanup(func() { DispatchFailed = oldFailed })
	DispatchFailed = func(k TaskKey, _ error) { failed = append(failed, k) }
	res := Autofill(in)
	if len(failed) != 1 || failed[0].PlanTask != fillKey || failed[0].Text != "do the thing\n" {
		t.Fatalf("a crashed batch must count one failed attempt: %v", failed)
	}
	if len(res.Sent) != 1 || !strings.Contains(strings.Join(res.Lines, "\n"), "released "+fillKey+": its run ended crashed") {
		t.Fatalf("a dead batch with no envelope: sent %v, lines %v", res.Sent, res.Lines)
	}
}

// a batch with no pid file and no record never ran: it is released as unsent and counts no failure.
func TestAutofillUnsentBatchCountsNothing(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	Autofill(in)
	id, _ := (*spawned)[0].line["id"].(string)
	old := time.Now().Add(-autofillStale - time.Minute)
	if err := os.Chtimes(filepath.Join(autofillDir(), id+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(autofillDir(), id+".pid")); err != nil {
		t.Fatal(err)
	}
	var failed []TaskKey
	oldFailed := DispatchFailed
	t.Cleanup(func() { DispatchFailed = oldFailed })
	DispatchFailed = func(k TaskKey, _ error) { failed = append(failed, k) }
	if res := Autofill(in); len(failed) != 0 || !strings.Contains(strings.Join(res.Lines, "\n"), "its run ended unsent") {
		t.Fatalf("unsent batch: failed %v, lines %v", failed, res.Lines)
	}
}

// a dead batch whose worker left a record is the record's to count, so the sweep counts nothing more.
func TestAutofillCrashedBatchWithRecordCountsOnce(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	Autofill(in)
	id, _ := (*spawned)[0].line["id"].(string)
	old := time.Now().Add(-autofillStale - time.Minute)
	if err := os.Chtimes(filepath.Join(autofillDir(), id+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(autofillDir(), id+".pid"), "0")
	oldRecord := BatchRecord
	t.Cleanup(func() { BatchRecord = oldRecord })
	BatchRecord = func(bid string, _ time.Time) (string, bool) { return "error", bid == id }
	var failed []TaskKey
	oldFailed := DispatchFailed
	t.Cleanup(func() { DispatchFailed = oldFailed })
	DispatchFailed = func(k TaskKey, _ error) { failed = append(failed, k) }
	res := Autofill(in)
	if len(failed) != 0 || !strings.Contains(strings.Join(res.Lines, "\n"), "released "+fillKey+": its run ended error") {
		t.Fatalf("recorded run counted again: failed %v, lines %v", failed, res.Lines)
	}
}

// a spawn that fails gives the claim back; a batch another trigger just started takes the one free slot.
func TestAutofillRollbackAndRecentSlot(t *testing.T) {
	in, _ := autofillFixture(t, "claude", "x\n", DispatchState{})
	autofillSpawn = func(string, []string, string, *os.File) int { return 0 }
	if res := Autofill(in); len(res.Sent) != 0 || !strings.Contains(strings.Join(res.Held, ";"), "dispatch:") {
		t.Fatalf("failed spawn: sent %v, held %v", res.Sent, res.Held)
	}
	if p, _ := Parse(in.Plans[0]); p.Tasks[0].Claim != "" || recentSpawns() != 0 {
		t.Fatalf("claim %q or %d batch files kept after a failed spawn", p.Tasks[0].Claim, recentSpawns())
	}
	in2, spawned := autofillFixture(t, "claude", "x\n", DispatchState{})
	writeFile(t, filepath.Join(autofillDir(), "autofill-peer.jsonl"), "{}\n")
	in2.Slots = 1
	if res := Autofill(in2); len(res.Sent) != 0 || len(*spawned) != 0 {
		t.Fatalf("one slot, a fresh peer batch: sent %v", res.Sent)
	}
}

// slowAutofill makes the run outlast a short deadline; cleanup takes the run lock the worker holds until it is done.
func slowAutofill(t *testing.T) {
	t.Cleanup(func() {
		_ = paths.WithFileLockWithin(filepath.Join(autofillDir(), "run"), 5*time.Second, func() error { return nil })
	})
	autofillSpawn = func(string, []string, string, *os.File) int { time.Sleep(300 * time.Millisecond); return 0 }
}

// a run past its deadline returns one notice instead of waiting.
func TestAutofillDeadline(t *testing.T) {
	in, _ := autofillFixture(t, "claude", "x\n", DispatchState{})
	old := autofillDeadline
	t.Cleanup(func() { autofillDeadline = old })
	autofillDeadline = 20 * time.Millisecond
	slowAutofill(t)
	res := Autofill(in)
	if len(res.Sent) != 0 || len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "deadline") {
		t.Fatalf("sent %v, lines %v, want one deadline line", res.Sent, res.Lines)
	}
}

// the claim's batch file exists before the claim, so a crash between them is swept as a crashed batch.
func TestAutofillBatchWrittenBeforeClaim(t *testing.T) {
	in, _ := autofillFixture(t, "claude", "x\n", DispatchState{})
	autofillSpawn = func(string, []string, string, *os.File) int {
		if recentSpawns() != 1 {
			t.Errorf("batch file missing at spawn")
		}
		return 0
	}
	Autofill(in)
}

// pruning removes only the running repo's old batches (and orphans), never another repository's evidence.
func TestAutofillPruneKeepsOtherRepo(t *testing.T) {
	in, _ := autofillFixture(t, "claude", "x\n", DispatchState{})
	week := time.Now().Add(-autofillKeep - time.Hour)
	for id, root := range map[string]string{"autofill-mine": in.Root, "autofill-theirs": "/other/repo"} {
		for _, ext := range []string{".jsonl", ".log"} {
			p := filepath.Join(autofillDir(), id+ext)
			writeFile(t, p, `{"workdir":"`+root+`"}`+"\n")
			if err := os.Chtimes(p, week, week); err != nil {
				t.Fatal(err)
			}
		}
	}
	Autofill(in)
	if !errors.Is(statErr(filepath.Join(autofillDir(), "autofill-mine.jsonl")), fs.ErrNotExist) {
		t.Fatal("this repo's old batch was kept")
	}
	if statErr(filepath.Join(autofillDir(), "autofill-theirs.jsonl")) != nil {
		t.Fatal("another repo's old batch was pruned")
	}
}

// two ready tasks owning one file send one task per run; the other waits for the next.
func TestAutofillSkipsOverlappingOwns(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	writeFile(t, in.Plans[0], "- [ ] 1.1 One — leaf: claude — verify: go test ./a — owns: a/a.go — brief: "+filepath.Join(filepath.Dir(in.Plans[0]), "brief.md")+
		"\n- [ ] 1.2 Two — leaf: claude — verify: go test ./a — owns: a — brief: "+filepath.Join(filepath.Dir(in.Plans[0]), "brief.md")+"\n")
	res := Autofill(in)
	if len(res.Sent) != 1 || len(*spawned) != 1 || len(res.Held) != 1 || !strings.Contains(res.Held[0], "owns overlap") {
		t.Fatalf("sent %v, held %v, spawns %d, want one sent and one held for overlap", res.Sent, res.Held, len(*spawned))
	}
}

// a worker whose context is done claims nothing and spawns nothing.
func TestAutofillCancelledContextDoesNothing(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "do the thing\n", DispatchState{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := autofillLocked(ctx, in)
	p, _ := Parse(in.Plans[0])
	if len(res.Sent) != 0 || len(*spawned) != 0 || p.Tasks[0].Claim != "" {
		t.Fatalf("sent %v, spawns %d, claim %q, want none", res.Sent, len(*spawned), p.Tasks[0].Claim)
	}
	job := autofillJob{root: in.Root, task: ReadyTask{Plan: in.Plans[0], ID: "Phase 1.1"}, planID: "1.1", key: fillKey}
	if why := job.launch(ctx); !strings.Contains(why, "deadline") || len(*spawned) != 0 {
		t.Fatalf("launch on a done context: %q, spawns %d", why, len(*spawned))
	}
}

// A degraded dispatch state no longer stops the fill (owner, 2026-11-26): the ready task is still sent.
func TestAutofillSendsWhileDegraded(t *testing.T) {
	in, spawned := autofillFixture(t, "claude", "x\n", DispatchState{Degraded: true})
	t.Setenv(AutofillEnv, "")
	if res := Autofill(in); len(res.Sent) != 1 || len(*spawned) != 1 {
		t.Fatalf("degraded: sent %v, lines %q", res.Sent, res.Lines)
	}
}
