package inflight

import (
	"context"
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

// Claim burst: of 30 racers on one account with For=12, exactly 12 win, 12 markers hold the
// account key, a second account still admits, and Total binds across accounts.
func TestTryClaimBurst(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	lim := Limits{Base: Base, Total: 10000}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := TryClaim(Claim{Owner: owner, ID: "leg." + strconv.Itoa(i), Account: "acct-a"}, lim); ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 12 {
		t.Fatalf("winners = %d, want exactly 12", got)
	}
	files := markers(t)
	if len(files) != 12 {
		t.Fatalf("marker files = %d, want 12: %v", len(files), files)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(Dir(), f))
		if err != nil || string(b) != "acct-a" {
			t.Errorf("marker %s content = %q, want the account key acct-a", f, b)
		}
	}
	if got := Count().Per["acct-a"]; got != 12 {
		t.Fatalf("Count().Per[acct-a] = %d, want 12", got)
	}

	// Per-account isolation: a second account has its own headroom.
	ok, _ := TryClaim(Claim{Owner: owner, ID: "leg.b1", Account: "acct-b"}, lim)
	if !ok {
		t.Fatal("second account refused despite a full first account")
	}
	// Total binds across accounts: 12 + 1 live, Total 13 leaves no room.
	tight := Limits{Base: Base, Total: 13}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "leg.b2", Account: "acct-b"}, tight); ok {
		t.Fatal("claim admitted past Total even though the per-account cap had room")
	}
}

// TryClaim N>1 writes the N leaf markers as one decision: refused whole when N does not fit,
// Fits writes nothing, and Release removes every leaf.
func TestTryClaimN(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	lim := Limits{Base: Base, Total: 100}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "wf", Account: "a", N: 3}, lim); !ok {
		t.Fatal("N=3 claim refused with room")
	}
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

	// Fits is the cap check: it must not write.
	if fits, _ := Fits(Claim{Owner: owner, ID: "fit", Account: "a", N: 3}, lim); !fits {
		t.Fatal("Fits refused with room")
	}
	for _, f := range markers(t) {
		if strings.Contains(f, "fit") {
			t.Errorf("Fits wrote marker %s", f)
		}
	}

	// 3 slots of 4 used on "a": an N=2 claim does not fit and must be refused whole.
	tight := Limits{Base: Base, Total: 100, Override: map[string]int{"a": 4}}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "no", Account: "a", N: 2}, tight); ok {
		t.Fatal("N=2 claim admitted with only 1 slot left")
	}
	for _, f := range markers(t) {
		if strings.Contains(f, "-no") {
			t.Errorf("refused claim left marker %s behind", f)
		}
	}

	Release(owner, "wf")
	for _, f := range markers(t) {
		if strings.Contains(f, "wf") {
			t.Errorf("Release left leaf %s behind", f)
		}
	}
}

// Acquire blocks while the account is full, wins after a Release, and a cancelled ctx returns
// ctx.Err() having written nothing.
func TestAcquireWaitCancel(t *testing.T) {
	setupDir(t)
	owner := os.Getpid()
	lim := Limits{Base: 1, Total: 1}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "first", Account: "a"}, lim); !ok {
		t.Fatal("setup claim refused")
	}
	won := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), Claim{Owner: owner, ID: "second", Account: "a"}, lim, 10*time.Millisecond)
		won <- err
	}()
	select {
	case err := <-won:
		t.Fatalf("Acquire won while full: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	Release(owner, "first")
	select {
	case err := <-won:
		if err != nil {
			t.Fatalf("Acquire after Release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire never won after Release freed the slot")
	}
	if !markerExists(owner, "second") {
		t.Error("winning Acquire wrote no marker")
	}

	// Refill the slot; a cancelled ctx must surface ctx.Err() and write nothing.
	Release(owner, "second")
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "third", Account: "a"}, lim); !ok {
		t.Fatal("re-fill claim refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Acquire(ctx, Claim{Owner: owner, ID: "fourth", Account: "a"}, lim, 10*time.Millisecond)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancelled Acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled Acquire never returned")
	}
	for _, f := range markers(t) {
		if strings.Contains(f, "fourth") {
			t.Errorf("cancelled Acquire wrote marker %s", f)
		}
	}
}

// Leg owner liveness: a dead owner's leg marker is neither counted nor allowed to survive a
// prune; a live owner's leg marker counts even past TTL.
func TestLegOwnerLiveness(t *testing.T) {
	if !ProbeTrusted() {
		t.Skip("proc probing untrusted on this platform; TTL is the sole backstop here")
	}
	setupDir(t)
	owner := os.Getpid()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot spawn a probe process: %v", err)
	}
	dead := cmd.Process.Pid
	deadName := markerName(dead, "leg.x")
	if err := os.WriteFile(filepath.Join(Dir(), deadName), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Count().Total; got != 0 {
		t.Fatalf("Count() with a dead leg owner = %d, want 0", got)
	}
	lim := Limits{Base: Base, Total: 100}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "probe", Account: "a"}, lim); !ok {
		t.Fatal("probe claim refused")
	}
	if _, err := os.Stat(filepath.Join(Dir(), deadName)); !os.IsNotExist(err) {
		t.Error("dead owner's leg marker survived a TryClaim prune")
	}

	oldName := markerName(owner, "leg.old")
	if err := os.WriteFile(filepath.Join(Dir(), oldName), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-TTL - time.Minute)
	if err := os.Chtimes(filepath.Join(Dir(), oldName), stale, stale); err != nil {
		t.Fatal(err)
	}
	if got := Count().Per["a"]; got != 2 {
		t.Fatalf("live owner's leg marker past TTL not counted: Per[a] = %d, want 2 (probe + leg.old)", got)
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
	lim := Limits{Base: Base, Total: 100}
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
	tmp := t.TempDir()
	transcript := filepath.Join(tmp, "sess.jsonl")
	subDir := SubagentDir(transcript)
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := "{\"tool_use_id\":\"toolu_b\"}\n{\"tool_use_id\":\"toolu_c\"}\n"
	if err := os.WriteFile(transcript, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMeta := func(agent, id string) {
		t.Helper()
		m := `{"toolUseId":"` + id + `"}`
		if err := os.WriteFile(filepath.Join(subDir, "agent-"+agent+".meta.json"), []byte(m), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMeta("a1", "toolu_a")
	writeMeta("c1", "toolu_c")
	idleLog := filepath.Join(subDir, "agent-a1.jsonl")
	if err := os.WriteFile(idleLog, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * Idle)
	if err := os.Chtimes(idleLog, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-c1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"toolu_a", "toolu_b", "toolu_c"} {
		if err := os.WriteFile(filepath.Join(Dir(), markerName(owner, id)), []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	lim := Limits{Base: Base, Total: 100}
	if ok, _ := TryClaim(Claim{Owner: owner, ID: "probe", Account: "a", Transcript: transcript}, lim); !ok {
		t.Fatal("probe claim refused")
	}
	if markerExists(owner, "toolu_a") {
		t.Error("spawn marker with an idle agent transcript survived the prune")
	}
	if markerExists(owner, "toolu_b") {
		t.Error("spawn marker answered by the session with no meta survived the prune (refused spawn)")
	}
	if !markerExists(owner, "toolu_c") {
		t.Error("background spawn with meta and a fresh agent transcript was pruned (2026-09-15 leak class)")
	}
}
