package inflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setupDir points the marker dir at a scratch CLAUDE_CONFIG_DIR so no test touches real state.
func setupDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
}

// markers lists live-dir entries that parse as markers (excludes .lock and .tmp-* strays).
func markers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if _, ok := Owner(e.Name()); ok {
			out = append(out, e.Name())
		}
	}
	return out
}

func markerExists(owner int, id string) bool {
	_, err := os.Stat(filepath.Join(Dir(), markerName(owner, id)))
	return err == nil
}

// writeFile writes body to path or fails the test.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setMtime stamps path's modification time or fails the test.
func setMtime(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// mkdirAll creates dir or fails the test.
func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// probeClaim claims one slot for owner so the prune pass runs, failing the test when it is refused.
func probeClaim(t *testing.T, owner int, transcript string) {
	t.Helper()
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "probe", Account: "a", Transcript: transcript}, Limits{Total: 100}); !ok {
		t.Fatal("probe claim refused")
	}
}

// Claim burst: of 2×Base racers on one account, exactly Base win, Base markers hold the
// account key, a second account still admits, and Total binds across accounts.
func TestTryClaimBurst(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	lim := Limits{Total: 10000}
	if got := raceClaims(owner, lim, 2*Base); got != Base {
		t.Fatalf("winners = %d, want exactly %d", got, Base)
	}
	files := markers(t)
	if len(files) != Base {
		t.Fatalf("marker files = %d, want %d: %v", len(files), Base, files)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(Dir(), f))
		if err != nil || string(b) != "acct-a" {
			t.Errorf("marker %s content = %q, want the account key acct-a", f, b)
		}
	}
	if got := Count().Per["acct-a"]; got != Base {
		t.Fatalf("Count().Per[acct-a] = %d, want %d", got, Base)
	}

	t.Run("per-account isolation", func(t *testing.T) {
		ok, _ := TryClaim(Claim{Owner: owner, ID: "dispatch.b1", Account: "acct-b"}, lim)
		if !ok {
			t.Fatal("second account refused despite a full first account")
		}
	})
	t.Run("total binds across accounts", func(t *testing.T) {
		tight := Limits{Total: Base + 1} // Base + 1 live leaves no room
		if ok, _ := TryClaim(Claim{Owner: owner, ID: "dispatch.b2", Account: "acct-b"}, tight); ok {
			t.Fatal("claim admitted past Total even though the per-account cap had room")
		}
	})
}

// raceClaims runs n concurrent claims on acct-a and returns how many were admitted.
func raceClaims(owner int, lim Limits, n int) int32 {
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if ok, _ := TryClaim(Claim{Owner: owner, ID: "dispatch." + strconv.Itoa(i), Account: "acct-a"}, lim); ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	return wins.Load()
}

// TryClaim N>1 writes the N leaf markers as one decision: refused whole when N does not fit,
// Fits writes nothing, and Release removes every leaf.
func TestTryClaimN(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	lim := Limits{Total: 100}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "wf", Account: "a", N: 3}, lim); !ok {
		t.Fatal("N=3 claim refused with room")
	}
	assertLeaves(t, owner)

	t.Run("Fits does not write", func(t *testing.T) {
		if fits, _ := Fits(Claim{Owner: owner, ID: "fit", Account: "a", N: 3}, lim); !fits {
			t.Fatal("Fits refused with room")
		}
		assertNoMarker(t, "fit", "Fits wrote marker %s")
	})
	t.Run("refused whole when N does not fit", func(t *testing.T) {
		// 3 slots of 4 used on "a": an N=2 claim does not fit.
		tight := Limits{Total: 100, Override: map[string]int{"a": 4}}
		if ok, _ := TryClaim(Claim{Owner: owner, ID: "no", Account: "a", N: 2}, tight); ok {
			t.Fatal("N=2 claim admitted with only 1 slot left")
		}
		assertNoMarker(t, "-no", "refused claim left marker %s behind")
	})
	t.Run("Release removes every leaf", func(t *testing.T) {
		Release(owner, "wf")
		assertNoMarker(t, "wf", "Release left leaf %s behind")
	})
}

func TestTryClaimPartialWriteReleasesMarkers(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	lim := Limits{Total: 4, Override: map[string]int{"a": 4}}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "existing", Account: "a"}, lim); !ok {
		t.Fatal("existing claim refused with room")
	}
	claim := Claim{Owner: owner, ID: "partial", Account: "a", N: 3}
	mkdirAll(t, filepath.Join(Dir(), ".tmp-"+markerName(owner, claim.ID)+".2"))

	ok, snap := TryClaim(claim, lim)
	if !ok {
		t.Fatal("partial write must fail open")
	}
	if snap.Total != 1 || snap.Per["a"] != 1 {
		t.Fatalf("snapshot = %+v, want only the existing claim", snap)
	}
	assertNoMarker(t, "-partial", "partial write left marker %s behind")
	if !markerExists(owner, "existing") {
		t.Fatal("partial write cleanup removed the existing claim")
	}
	if got := Count(); got.Total != 1 || got.Per["a"] != 1 {
		t.Fatalf("count after partial write = %+v, want only the existing claim", got)
	}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "next", Account: "a", N: 3}, lim); !ok {
		t.Fatal("partial write left slots occupied, refusing the next N=3 claim")
	}
}

// assertLeaves checks the N=3 claim wrote leaves .1..3 holding the account key and no base marker.
func assertLeaves(t *testing.T, owner int) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		name := markerName(owner, "wf") + "." + strconv.Itoa(i)
		b, err := os.ReadFile(filepath.Join(Dir(), name))
		if err != nil {
			t.Errorf("leaf %s missing: %v", name, err)
		} else if string(b) != "a" {
			t.Errorf("leaf %s content = %q, want the account key", name, b)
		}
	}
	if markerExists(owner, "wf") {
		t.Error("base marker written for an N>1 claim; only .1..N leaves are expected")
	}
}

// assertNoMarker fails with format (taking the marker name) for any marker whose name contains sub.
func assertNoMarker(t *testing.T, sub, format string) {
	t.Helper()
	for _, f := range markers(t) {
		if strings.Contains(f, sub) {
			t.Errorf(format, f)
		}
	}
}

// Dispatch owner liveness: a dead owner's dispatch marker is neither counted nor allowed to survive a
// prune; a live owner's dispatch marker counts even past TTL.
func TestDispatchOwnerLiveness(t *testing.T) {
	if !probeTrusted() {
		t.Skip("proc probing untrusted on this platform; TTL is the sole backstop here")
	}
	setupDir(t)
	owner := os.Getpid()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot spawn a probe process: %v", err)
	}
	dead := cmd.Process.Pid
	deadName := markerName(dead, "dispatch.x")
	if err := os.WriteFile(filepath.Join(Dir(), deadName), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Count().Total; got != 0 {
		t.Fatalf("Count() with a dead dispatch owner = %d, want 0", got)
	}
	lim := Limits{Total: 100}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "probe", Account: "a"}, lim); !ok {
		t.Fatal("probe claim refused")
	}
	if _, err := os.Stat(filepath.Join(Dir(), deadName)); !os.IsNotExist(err) {
		t.Error("dead owner's dispatch marker survived a TryClaim prune")
	}

	oldName := markerName(owner, "dispatch.old")
	if err := os.WriteFile(filepath.Join(Dir(), oldName), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-ttl - time.Minute)
	if err := os.Chtimes(filepath.Join(Dir(), oldName), stale, stale); err != nil {
		t.Fatal(err)
	}
	if got := Count().Per["a"]; got != 2 {
		t.Fatalf("live owner's dispatch marker past TTL not counted: Per[a] = %d, want 2 (probe + dispatch.old)", got)
	}
}

// RetireAgent releases exactly the slot recorded in the agent's meta.json; no meta retires
// nothing and a second stop is a no-op that never touches a sibling marker.
func TestRetireAgent(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	subDir := SubagentDir(filepath.Join(t.TempDir(), "sess.jsonl"))
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-a1.meta.json"), []byte(`{"toolUseId":"toolu_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lim := Limits{Total: 100}
	for _, id := range []string{"toolu_x", "toolu_y"} {
		if ok, _ := TryClaim(Claim{Owner: owner, ID: id, Account: "a"}, lim); !ok {
			t.Fatalf("setup claim %s refused", id)
		}
	}
	if got := ToolUseID(subDir, "a1"); got != "toolu_x" {
		t.Fatalf("ToolUseID = %q, want toolu_x", got)
	}
	if got := ToolUseID(subDir, "ghost"); got != "" {
		t.Fatalf("ToolUseID for a meta-less agent = %q, want empty", got)
	}

	RetireAgent(owner, subDir, "a1")
	if markerExists(owner, "toolu_x") {
		t.Error("RetireAgent left the retired agent's marker")
	}
	if !markerExists(owner, "toolu_y") {
		t.Error("RetireAgent touched a sibling marker")
	}
	RetireAgent(owner, subDir, "a1")    // a second stop is a no-op
	RetireAgent(owner, subDir, "ghost") // no meta retires nothing
	if !markerExists(owner, "toolu_y") {
		t.Error("a no-op RetireAgent touched a sibling marker")
	}
}

// Self spawn prune: a spawn marker with meta but an idle agent transcript is pruned; one the
// session answered with NO meta is pruned (refused spawn); one WITH meta and a fresh agent
// transcript survives even though the session answered it (the 2026-09-15 leak class).
func TestSelfSpawnPrune(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	subDir := SubagentDir(transcript)
	mkdirAll(t, subDir)
	writeFile(t, transcript, "{\"tool_use_id\":\"toolu_b\"}\n{\"tool_use_id\":\"toolu_c\"}\n{\"tool_use_id\":\"toolu_e\"}\n")
	writeSpawnMeta(t, subDir, "a1", "toolu_a", false)
	writeSpawnMeta(t, subDir, "c1", "toolu_c", false)
	writeSpawnMeta(t, subDir, "d1", "toolu_d", true)
	idleLog := filepath.Join(subDir, "agent-a1.jsonl")
	writeFile(t, idleLog, "{}\n")
	setMtime(t, idleLog, time.Now().Add(-2*Idle))
	writeFile(t, filepath.Join(subDir, "agent-c1.jsonl"), "{}\n")
	writeFile(t, filepath.Join(subDir, "agent-d1.jsonl"), "{}\n")
	for _, id := range []string{"toolu_a", "toolu_b", "toolu_c", "toolu_d", "toolu_e"} {
		writeFile(t, filepath.Join(Dir(), markerName(owner, id)), "a\n"+transcript)
	}
	// past Settle, inside Grace
	setMtime(t, filepath.Join(Dir(), markerName(owner, "toolu_b")), time.Now().Add(-30*time.Second))

	probeClaim(t, owner, transcript)
	if markerExists(owner, "toolu_a") {
		t.Error("spawn marker with an idle agent transcript survived the prune")
	}
	if markerExists(owner, "toolu_b") {
		t.Error("spawn marker answered by the session with no meta survived the prune (refused spawn)")
	}
	if markerExists(owner, "toolu_d") {
		t.Error("spawn whose meta.json says finished survived the prune (foreground agent, no SubagentStop needed)")
	}
	if !markerExists(owner, "toolu_e") {
		t.Error("answered spawn with no meta yet was pruned inside Grace (meta.json may not be written yet)")
	}
	if !markerExists(owner, "toolu_c") {
		t.Error("background spawn with meta and a fresh agent transcript was pruned (2026-09-15 leak class)")
	}
}

// writeSpawnMeta writes agent-<agent>.meta.json under subDir tying the agent to a tool_use_id.
func writeSpawnMeta(t *testing.T, subDir, agent, id string, finished bool) {
	t.Helper()
	m := `{"toolUseId":"` + id + `"}`
	if finished {
		m = `{"toolUseId":"` + id + `","finished":true}`
	}
	writeFile(t, filepath.Join(subDir, "agent-"+agent+".meta.json"), m)
}

// A sibling session's long-running agent keeps counting past TTL (its marker carries that session's transcript),
// and its finished agent is pruned by the same rule, whoever scans.
func TestForeignSpawnFollowsItsOwnSession(t *testing.T) {
	setupDir(t)
	if !probeTrusted() {
		t.Skip("owner liveness is untrusted here")
	}
	helper := exec.Command("sleep", "30")
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })
	sib := helper.Process.Pid
	transcript := filepath.Join(t.TempDir(), "sib.jsonl")
	sub := SubagentDir(transcript)
	mkdirAll(t, sub)
	for agent, body := range map[string]string{
		"long": `{"toolUseId":"toolu_long"}`,
		"done": `{"toolUseId":"toolu_done","finished":true}`,
	} {
		writeFile(t, filepath.Join(sub, "agent-"+agent+".meta.json"), body)
		writeFile(t, filepath.Join(sub, "agent-"+agent+".jsonl"), "{}\n")
	}
	old := time.Now().Add(-2 * ttl)
	for _, id := range []string{"toolu_long", "toolu_done"} {
		path := filepath.Join(Dir(), markerName(sib, id))
		writeFile(t, path, "a\n"+transcript)
		setMtime(t, path, old)
	}
	live := liveness(time.Now()) // the read-only rule `agents cancel --lost` and the status line use
	if !live(markerName(sib, "toolu_long")) || live(markerName(sib, "toolu_done")) {
		t.Error("Liveness disagrees with the limiter on a sibling's long and finished agents")
	}
	probeClaim(t, os.Getpid(), "")
	if !markerExists(sib, "toolu_long") {
		t.Error("a live sibling's active agent stopped counting after TTL (cap overrun)")
	}
	if markerExists(sib, "toolu_done") {
		t.Error("a sibling's finished agent still counts")
	}
}

// A leaf marker (<tool_use_id>.<n>) follows its base id's agent: a finished agent prunes the leaf too.
func TestSpawnLeafFollowsBaseAgent(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	subDir := SubagentDir(transcript)
	mkdirAll(t, subDir)
	writeFile(t, transcript, "")
	writeSpawnMeta(t, subDir, "d1", "toolu_d", true)
	writeSpawnMeta(t, subDir, "l1", "toolu_l", false)
	writeFile(t, filepath.Join(subDir, "agent-l1.jsonl"), "{}\n")
	for _, id := range []string{"toolu_d.1", "toolu_l.1"} {
		writeFile(t, filepath.Join(Dir(), markerName(owner, id)), "a\n"+transcript)
	}
	probeClaim(t, owner, transcript)
	if markerExists(owner, "toolu_d.1") {
		t.Error("leaf of a finished agent survived the prune")
	}
	if !markerExists(owner, "toolu_l.1") {
		t.Error("leaf of a live agent was pruned")
	}
}

// Cap math: Total = min(Base × max(len(available),1), 4 × cpus); For uses the override else Base.
func TestComputeLimits(t *testing.T) {
	cases := []struct {
		name      string
		available int
		cpus      int
		want      int
	}{
		{"accounts bind before cpus", 2, 64, 2 * Base},
		{"cpus bind", 1, 2, 8},
		{"no accounts still allows one base", 0, 64, Base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim := ComputeLimits(make([]string, tc.available), nil, tc.cpus)
			if lim.Total != tc.want {
				t.Errorf("ComputeLimits(%d accounts, %d cpus).Total = %d, want %d",
					tc.available, tc.cpus, lim.Total, tc.want)
			}
		})
	}
	lim := ComputeLimits([]string{"a"}, map[string]int{"a": 3}, 16)
	if got := lim.For("a"); got != 3 {
		t.Errorf("For with override = %d, want 3", got)
	}
	if got := lim.For("b"); got != Base {
		t.Errorf("For without override = %d, want Base %d", got, Base)
	}
}
