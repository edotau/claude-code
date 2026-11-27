package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

func stubResend(t *testing.T, f func(*exec.Cmd) ([]byte, error)) {
	t.Helper()
	old := resendOutput
	resendOutput = f
	t.Cleanup(func() { resendOutput = old })
}

func saveRec(t *testing.T, r Record) {
	t.Helper()
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestResendReturnsNewID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	saveRec(t, Record{ID: "h-1", Parent: "p-1", Status: "failed", Time: time.Now()})
	var args, env []string
	stubResend(t, func(c *exec.Cmd) ([]byte, error) {
		args, env = c.Args, c.Env
		return []byte(`{"id":"h-2"}`), nil
	})
	got, err := Resend(context.Background(), "h-1")
	if err != nil || got != "h-2" {
		t.Fatalf("Resend = %q, %v; want h-2", got, err)
	}
	if !slices.Contains(args, "--retry") || !slices.Contains(args, "h-1") || !slices.Contains(args, "--recover") {
		t.Fatalf("args = %v", args)
	}
	if !slices.Contains(env, proc.DispatchIDEnv+"=p-1") {
		t.Fatalf("env lacks %s=p-1", proc.DispatchIDEnv)
	}
}

func TestResendIgnoresDeadResumedMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	saveRec(t, Record{ID: "h-1", Parent: "p-1", Status: "failed", Time: time.Now()})
	stubResend(t, func(*exec.Cmd) ([]byte, error) { return nil, errors.New("boom") })
	if err := os.MkdirAll(WatchDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WatchMarker("resumed", "h-1"), []byte("h-3"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveRec(t, Record{ID: "h-3", Status: "failed", Time: time.Now()})
	if got, err := Resend(context.Background(), "h-1"); err == nil {
		t.Fatalf("Resend = %q, nil; want an error for a dead marker target", got)
	}
	saveRec(t, Record{ID: "h-3", Status: wire.StatusOK, Time: time.Now()})
	if got, err := Resend(context.Background(), "h-1"); err != nil || got != "h-3" {
		t.Fatalf("Resend = %q, %v; want h-3", got, err)
	}
}

// deadRunning saves a running record whose runner is gone, so a claim or a reap may act on it.
func deadRunning(t *testing.T, edit func(*Record)) Record {
	t.Helper()
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now().Add(-time.Minute)) // inside the attempt timeout: an older dead run is a budget end
	rec.PID, rec.Background, rec.FallbackPolicy, rec.PlanTask = adapterDeadPID(t), true, "all", "lock-test "+rec.ID
	if edit != nil {
		edit(&rec)
	}
	saveRec(t, rec)
	return rec
}

// holdAt blocks the first call of op while its record lock is held: entered fires, release lets it go on.
func holdAt(t *testing.T, op string) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, out, once := make(chan struct{}), make(chan struct{}), sync.Once{}
	old := underLockHook
	underLockHook = func(got string) {
		if got == op {
			once.Do(func() { close(in); <-out })
		}
	}
	t.Cleanup(func() { underLockHook = old })
	return in, sync.OnceFunc(func() { close(out) })
}

// assertBlocked fails when done fires while the lock holder is still held.
func assertBlocked(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s finished while the record lock was held: %v", what, err)
	case <-time.After(100 * time.Millisecond):
	}
}

// ownStart is this process's start time, the identity a live claimer or canceller carries; no source skips the test.
func ownStart(t *testing.T) string {
	t.Helper()
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no process start identity here: %v", err)
	}
	return start
}

// put writes rec as is, the way a crafted state reaches disk; no lock is needed when no other writer runs.
func put(t *testing.T, rec Record) {
	t.Helper()
	if err := rec.saveLocked(); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, id string) Record {
	t.Helper()
	rec, err := LoadRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// deadCancel is a cancelling whose canceller is gone.
func deadCancel(t *testing.T, phase CancelPhase) *CancelInfo {
	return &CancelInfo{Phase: phase, Token: "dead-token", PID: adapterDeadPID(t), Start: "gone"}
}

// Cancel table: exactly these edges exist, each taken by a live owner; every other pair is refused.
func TestCancelEdgeTable(t *testing.T) {
	start := ownStart(t)
	allowed := map[[2]CancelPhase]bool{
		{cancelNone, cancelCancelling}: true, {cancelCancelling, cancelNone}: true, {cancelCancelling, cancelSignalled}: true,
		{cancelCancelling, cancelDone}: true, {cancelSignalled, cancelDone}: true, {cancelNone, cancelDone}: true,
	}
	phases := []CancelPhase{cancelNone, cancelCancelling, cancelSignalled, cancelDone}
	for _, from := range phases {
		for _, to := range phases {
			r := Record{ID: "x", Status: "running"}
			if from != cancelNone {
				r.Cancel = &CancelInfo{Phase: from, Token: "tok", PID: os.Getpid(), Start: start}
			}
			got, err := r.stepCancel(to, "tok")
			if (err == nil) != allowed[[2]CancelPhase{from, to}] {
				t.Errorf("cancel %q -> %q: err = %v, want allowed=%t", from, to, err, allowed[[2]CancelPhase{from, to}])
			}
			if err == nil && got.Cancel.phase() != to {
				t.Errorf("cancel %q -> %q landed in %q", from, to, got.Cancel.phase())
			}
		}
	}
}

// A live canceller's cancel is rolled back or signalled only by its owner; anyone voids a dead one.
func TestCancelActorRules(t *testing.T) {
	ppid := os.Getppid()
	pstart, err := proc.StartTime(ppid)
	if err != nil {
		t.Skipf("no start identity for the parent: %v", err)
	}
	other := Record{ID: "x", Cancel: &CancelInfo{Phase: cancelCancelling, Token: "A", PID: ppid, Start: pstart}}
	if _, err := other.stepCancel(cancelNone, "B"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("another's rollback of a live cancel = %v, want refused", err)
	}
	if _, err := other.stepCancel(cancelSignalled, ""); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("a signal by a non-owner = %v, want refused", err)
	}
	if _, err := other.stepCancel(cancelNone, "A"); err != nil {
		t.Fatalf("the owner's rollback = %v", err)
	}
	dead := Record{ID: "x", Cancel: deadCancel(t, cancelCancelling)}
	if dead.cancelled() {
		t.Fatal("a dead canceller's cancelling still counts as a cancel")
	}
	if _, err := dead.stepCancel(cancelNone, "anyone"); err != nil {
		t.Fatalf("voiding a dead canceller = %v", err)
	}
}

// Claim table: exactly these edges exist for a live claimer; a launching claim is not released by it.
func TestClaimEdgeTable(t *testing.T) {
	start := ownStart(t)
	allowed := map[[2]ClaimPhase]bool{
		{claimNone, claimClaimed}: true, {claimClaimed, claimLaunching}: true, {claimClaimed, claimAdopted}: true,
		{claimLaunching, claimAdopted}: true, {claimClaimed, claimNone}: true, {claimAdopted, claimNone}: true,
	}
	phases := []ClaimPhase{claimNone, claimClaimed, claimLaunching, claimAdopted}
	for _, from := range phases {
		for _, to := range phases {
			r := Record{ID: "x"}
			if from != claimNone {
				r.Resumer = &Claim{Phase: from, Nonce: "n", PID: os.Getpid(), PIDStart: start, Successor: "s", SuccPID: os.Getpid(),
					Deadline: time.Now().Add(time.Hour)}
			}
			got, err := r.stepClaim(to, "n", "s")
			if (err == nil) != allowed[[2]ClaimPhase{from, to}] {
				t.Errorf("claim %q -> %q: err = %v, want allowed=%t", from, to, err, allowed[[2]ClaimPhase{from, to}])
			}
			if err == nil && got.Resumer.phase() != to {
				t.Errorf("claim %q -> %q landed in %q", from, to, got.Resumer.phase())
			}
		}
	}
}

// Only the attempt that holds the nonce moves its claim; another attempt of the same pid is refused.
func TestClaimNonceTellsAttemptsApart(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, a, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseResend(rec.ID, a); err != nil {
		t.Fatal(err)
	}
	_, b, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"release": ReleaseResend(rec.ID, a), "launch": MarkLaunching(rec.ID, a), "adopt": AdoptResend(rec.ID, a, "h-2"),
	} {
		if !errors.Is(err, ErrBadTransition) {
			t.Fatalf("A's delayed %s over B's claim = %v, want refused", name, err)
		}
	}
	if got := load(t, rec.ID).Resumer; got.Nonce != b || got.Phase != claimClaimed {
		t.Fatalf("claim after A's delayed calls = %+v, want B's untouched", got)
	}
}

// A stale snapshot carrying an older claim never overwrites a newer one.
func TestStaleSaveCannotOverwriteClaim(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, a, _ := ClaimResend(rec.ID)
	stale := load(t, rec.ID) // bears claim A
	if err := ReleaseResend(rec.ID, a); err != nil {
		t.Fatal(err)
	}
	_, b, _ := ClaimResend(rec.ID)
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}
	if got := load(t, rec.ID).Resumer; got == nil || got.Nonce != b {
		t.Fatalf("claim after a stale Save = %+v, want B's", got)
	}
}

// A stale pre-cancel snapshot saved while a cancel holds the lock lands after it and keeps the cancel.
func TestSaveKeepsCancelOfStaleSnapshot(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
	snapshot := rec
	entered, release := holdAt(t, "cancel-mark")
	cancelled, saved := make(chan error, 1), make(chan error, 1)
	go func() { _, err := MarkCancelling(rec.ID); cancelled <- err }()
	<-entered
	go func() { saved <- snapshot.Save() }()
	assertBlocked(t, saved, "Save")
	release()
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if got := load(t, rec.ID); got.Cancel.phase() != cancelCancelling || got.Cancel.PID != os.Getpid() {
		t.Fatalf("stale Save left cancel %+v, want the cancelling kept", got.Cancel)
	}
}

// A cancel that holds the record lock decides before a claim waiting on it: the claim sees the cancel.
func TestCancelBeforeClaimResendRefusesClaim(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	entered, release := holdAt(t, "cancel-mark")
	cancelled, claimed := make(chan error, 1), make(chan error, 1)
	go func() { _, err := MarkCancelling(rec.ID); cancelled <- err }()
	<-entered
	go func() { _, _, err := ClaimResend(rec.ID); claimed <- err }()
	assertBlocked(t, claimed, "ClaimResend")
	release()
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if err := <-claimed; !errors.Is(err, ErrNotResent) {
		t.Fatalf("claim after a cancel = %v, want not re-sent", err)
	}
	if load(t, rec.ID).taken() {
		t.Fatal("a refused claim left a claim")
	}
}

// A claim that holds the lock decides before a cancel waiting on it: the claim stands.
func TestClaimResendBeforeCancelWins(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	entered, release := holdAt(t, "claim-resend")
	claimed, cancelled := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := ClaimResend(rec.ID); claimed <- err }()
	<-entered
	go func() { _, err := MarkCancelling(rec.ID); cancelled <- err }()
	assertBlocked(t, cancelled, "MarkCancelling")
	release()
	if err := <-claimed; err != nil {
		t.Fatalf("claim = %v, want won", err)
	}
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if !load(t, rec.ID).taken() {
		t.Fatal("the winning claim left no claim")
	}
}

// ClaimResend refuses a live runner, a cancel and an earlier claim, each with its own typed error.
func TestClaimResendTypedRefusals(t *testing.T) {
	setConfigDir(t)
	live := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
	if _, _, err := ClaimResend(live.ID); !errors.Is(err, ErrHolderLive) {
		t.Fatalf("live holder = %v, want ErrHolderLive", err)
	}
	if load(t, live.ID).taken() {
		t.Fatal("a refused claim left a claim")
	}
	dead := deadRunning(t, nil)
	if _, _, err := ClaimResend(dead.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimResend(dead.ID); !errors.Is(err, ErrAlreadyResumed) {
		t.Fatalf("second claim = %v, want ErrAlreadyResumed", err)
	}
}

// Liveness is by identity: another start time is gone, an unreadable start time counts live.
func TestIdentityLive(t *testing.T) {
	start := ownStart(t)
	if !identityLive(os.Getpid(), start) || !identityLive(os.Getpid(), "") {
		t.Fatal("this process reads dead by its own identity")
	}
	if identityLive(os.Getpid(), start+"-other") || identityLive(adapterDeadPID(t), "") {
		t.Fatal("a reused or gone pid reads live")
	}
	old := procStart
	procStart = func(int) (string, error) { return "", errors.New("start time unreadable") }
	t.Cleanup(func() { procStart = old })
	if !identityLive(adapterDeadPID(t), "x") {
		t.Fatal("an unreadable start time counted dead: a live Windows runner could be re-sent")
	}
	setConfigDir(t)
	rec := deadRunning(t, nil)
	if _, _, err := ClaimResend(rec.ID); !errors.Is(err, ErrHolderLive) {
		t.Fatalf("claim over a holder with an unreadable start = %v, want ErrHolderLive", err)
	}
}

// A claimer's liveness and deadline decide when a claim is void; an adopted one never is.
func TestClaimVoidRules(t *testing.T) {
	start := ownStart(t)
	dead, live := adapterDeadPID(t), os.Getpid()
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Minute)
	for name, tc := range map[string]struct {
		c    Claim
		void bool
	}{
		"claimed, live claimer":            {Claim{Phase: claimClaimed, PID: live, PIDStart: start}, false},
		"claimed, dead claimer":            {Claim{Phase: claimClaimed, PID: dead, PIDStart: "gone"}, true},
		"claimed, reused pid":              {Claim{Phase: claimClaimed, PID: live, PIDStart: start + "-other"}, true},
		"launching, dead, before deadline": {Claim{Phase: claimLaunching, PID: dead, PIDStart: "gone", Deadline: future}, false},
		"launching, dead, past deadline":   {Claim{Phase: claimLaunching, PID: dead, PIDStart: "gone", Deadline: past}, true},
		"launching, live, past deadline":   {Claim{Phase: claimLaunching, PID: live, PIDStart: start, Deadline: past}, false},
		"adopted, dead claimer":            {Claim{Phase: claimAdopted, PID: dead, PIDStart: "gone", Deadline: past}, false},
	} {
		if got := tc.c.void(time.Now()); got != tc.void {
			t.Errorf("%s: void = %t, want %t", name, got, tc.void)
		}
	}
}

// An ambiguous launch keeps the claim launching: a rival claims only past the deadline with the claimer dead.
func TestAmbiguousLaunchStaysClaimed(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, nonce, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkLaunching(rec.ID, nonce); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseResend(rec.ID, nonce); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("the claimer's release of a launching claim = %v, want refused", err)
	}
	crafted := load(t, rec.ID)
	for name, tc := range map[string]struct {
		pid      int
		start    string
		deadline time.Time
		want     error
	}{
		"dead claimer, deadline ahead": {adapterDeadPID(t), "gone", time.Now().Add(time.Minute), ErrAlreadyResumed},
		"live claimer, deadline past":  {os.Getpid(), ownStart(t), time.Now().Add(-time.Minute), ErrAlreadyResumed},
		"dead claimer, deadline past":  {adapterDeadPID(t), "gone", time.Now().Add(-time.Minute), nil},
	} {
		c := *crafted.Resumer
		c.PID, c.PIDStart, c.Deadline = tc.pid, tc.start, tc.deadline
		crafted.Resumer = &c
		put(t, crafted)
		if _, _, err := ClaimResend(rec.ID); !errors.Is(err, tc.want) {
			t.Fatalf("%s: claim = %v, want %v", name, err, tc.want)
		}
	}
}

// The successor adopts with the nonce; an adopted claim never reverts, and a late adopter of a reverted one is refused.
func TestSuccessorAdoptsLaunchingClaim(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, nonce, _ := ClaimResend(rec.ID)
	if err := MarkLaunching(rec.ID, nonce); err != nil {
		t.Fatal(err)
	}
	if err := AdoptResend(rec.ID, "wrong", "h-2"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("adopt with another nonce = %v, want refused", err)
	}
	if err := AdoptResend(rec.ID, nonce, "h-2"); err != nil {
		t.Fatal(err)
	}
	if id, ok := ResumedAs(rec.ID); !ok || id != "h-2" || ResumedFrom("h-2") != rec.ID {
		t.Fatalf("after adopt: ResumedAs = %q, %t; ResumedFrom = %q", id, ok, ResumedFrom("h-2"))
	}
	crafted := load(t, rec.ID)
	crafted.Resumer.PID, crafted.Resumer.PIDStart, crafted.Resumer.Deadline = adapterDeadPID(t), "gone", time.Now().Add(-time.Minute)
	put(t, crafted)
	if _, _, err := ClaimResend(rec.ID); !errors.Is(err, ErrAlreadyResumed) {
		t.Fatalf("claim over an adopted claim with a dead claimer = %v, want ErrAlreadyResumed", err)
	}
	if err := RollbackResend(rec.ID, "h-2"); err != nil || load(t, rec.ID).taken() {
		t.Fatalf("rollback of the adopting process = %v, still taken %t", err, load(t, rec.ID).taken())
	}
	if err := AdoptResend(rec.ID, nonce, "h-3"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("a late adopt of a rolled-back claim = %v, want refused", err)
	}
}

// F7: an empty or malformed marker file is never trusted: the record's claim alone decides.
func TestClaimResendIgnoresMarkerFileContent(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	marker := WatchMarker("resumed", rec.ID)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"", "pid:\n", "pid:abc", "two words"} {
		if err := os.WriteFile(marker, []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		if id, ok := ResumedAs(rec.ID); ok || id != "" || load(t, rec.ID).taken() {
			t.Fatalf("marker %q: ResumedAs = %q, %t; taken %t; want unnamed", junk, id, ok, load(t, rec.ID).taken())
		}
	}
	if _, _, err := ClaimResend(rec.ID); err != nil {
		t.Fatalf("claim over a junk marker = %v, want won", err)
	}
}

// A second live canceller is refused; the owner's rollback lifts the cancel; another's rollback leaves it.
func TestCancelOwnerAndRollback(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
	tokA, errA := MarkCancelling(rec.ID)
	if _, errB := MarkCancelling(rec.ID); errA != nil || !errors.Is(errB, ErrCancelling) {
		t.Fatalf("A = %v, B = %v; want A won, B refused", errA, errB)
	}
	if err := ClearCancelling(rec.ID, "other"); !errors.Is(err, ErrBadTransition) || load(t, rec.ID).Cancel.phase() != cancelCancelling {
		t.Fatalf("another's rollback = %v; want refused and the cancel kept", err)
	}
	if err := ClearCancelling(rec.ID, tokA); err != nil || load(t, rec.ID).Cancel != nil {
		t.Fatalf("A's own rollback = %v, cancel %+v; want cleared", err, load(t, rec.ID).Cancel)
	}
}

// cancel(): a refusal before any signal rolls back; an error once the stop is recorded keeps the cancel.
func TestCancelRollbackOnlyBeforeSignal(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
	r := rec
	r.PIDStart, _ = proc.StartTime(r.PID)
	old := stopJob
	t.Cleanup(func() { stopJob = old })
	stopJob = func(*Record) error { return errNotSignalled }
	if _, err := cancel(&r); !errors.Is(err, errNotSignalled) || load(t, rec.ID).Cancel != nil {
		t.Fatalf("cancel refused before any signal = %v, cancel %+v; want rolled back", err, load(t, rec.ID).Cancel)
	}
	stopJob = func(r *Record) error {
		return errors.Join(signalCancel(r.ID), errors.New("still running after SIGKILL"))
	}
	if _, err := cancel(&r); err == nil || load(t, rec.ID).Cancel.phase() != cancelSignalled {
		t.Fatalf("cancel with an error after the signal = %v, cancel %+v; want signalled kept", err, load(t, rec.ID).Cancel)
	}
	if err := ClearCancelling(rec.ID, "any"); err != nil || load(t, rec.ID).Cancel.phase() != cancelSignalled {
		t.Fatalf("rollback after signalled = %v, cancel %+v; want refused quietly", err, load(t, rec.ID).Cancel)
	}
}

// R3-2: signalled is on disk before the signal leaves; a canceller dying at the signal leaves a cancel nobody voids.
func TestSignalledIsWrittenBeforeTheSignal(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	if _, err := MarkCancelling(rec.ID); err != nil {
		t.Fatal(err)
	}
	var atSignal CancelPhase
	old := interrupt
	t.Cleanup(func() { interrupt = old })
	interrupt = func(int) error { atSignal = load(t, rec.ID).Cancel.phase(); return errors.New("canceller died here") }
	if err := escalate(rec.PID, rec.ID); err == nil || atSignal != cancelSignalled {
		t.Fatalf("escalate = %v, phase at the signal %q; want signalled before it", err, atSignal)
	}
	crafted := load(t, rec.ID)
	crafted.Cancel.PID, crafted.Cancel.Start = adapterDeadPID(t), "gone" // the canceller is killed
	put(t, crafted)
	if _, _, err := ClaimResend(rec.ID); !errors.Is(err, ErrNotResent) {
		t.Fatalf("claim after a canceller died past the signal = %v, want not re-sent", err)
	}
	if tok, err := MarkCancelling(rec.ID); tok != "" || err != nil || load(t, rec.ID).Cancel.phase() != cancelSignalled {
		t.Fatalf("a new cancel over a signalled one = %q, %v; want the signalled state kept", tok, err)
	}
}

// R3-3: a dead canceller's cancelling is void, and reaping the record never makes it a permanent veto.
func TestReapedVoidCancelStaysOpen(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, func(r *Record) { r.Cancel = deadCancel(t, cancelCancelling) })
	if _, err := ResendGate(rec.ID); err != nil {
		t.Fatalf("gate over a void cancelling = %v, want open", err)
	}
	if got := reapLost(load(t, rec.ID)); got.Status != "lost" {
		t.Fatalf("reapLost status = %q, want lost", got.Status)
	}
	got := load(t, rec.ID)
	if got.cancelled() || got.NoResend(time.Now()) || got.FailKind == FailCancelled || got.Cancel != nil {
		t.Fatalf("reaped record = cancel %+v kind %q; want no cancel left", got.Cancel, got.FailKind)
	}
	if _, _, err := ClaimResend(rec.ID); err != nil {
		t.Fatalf("claim over a reaped void cancel = %v, want won", err)
	}
	if tok, err := MarkCancelling(deadRunning(t, func(r *Record) { r.Cancel = deadCancel(t, cancelCancelling) }).ID); tok == "" || err != nil {
		t.Fatalf("a new cancel over a void one = %q, %v; want its own", tok, err)
	}
}

// R3-4: a run finishing ok after the signal is recorded ok but counts as cancelled; a failed one is cancelled.
func TestFinishAfterSignalCountsCancelled(t *testing.T) {
	setConfigDir(t)
	for name, err := range map[string]error{"ok": nil, "failed": errors.New("model failed")} {
		rec := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
		if _, merr := MarkCancelling(rec.ID); merr != nil {
			t.Fatal(merr)
		}
		if serr := signalCancel(rec.ID); serr != nil {
			t.Fatal(serr)
		}
		rec.finishSave(wire.Result{}, err, time.Second, nil)
		got := load(t, rec.ID)
		wantStatus, wantKind := "ok", ""
		if err != nil {
			wantStatus, wantKind = "error", FailCancelled
		}
		if got.Status != wantStatus || got.FailKind != wantKind || got.Cancel.phase() != cancelDone {
			t.Fatalf("%s: %q %q cancel %q; want %s %q cancelled", name, got.Status, got.FailKind, got.Cancel.phase(), wantStatus, wantKind)
		}
		if _, gerr := ResendGate(rec.ID); !errors.Is(gerr, ErrNotResent) {
			t.Fatalf("%s: gate after a signalled finish = %v, want not re-sent", name, gerr)
		}
	}
}

// A vetoed run that ends in an error is not charged a failed attempt; an uncancelled one is.
func TestFinishSaveSkipsHealthUnderCancel(t *testing.T) {
	setConfigDir(t)
	vetoed := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
	if _, err := MarkCancelling(vetoed.ID); err != nil {
		t.Fatal(err)
	}
	plain := deadRunning(t, func(r *Record) { r.PID = os.Getpid() })
	for _, r := range []Record{vetoed, plain} {
		r.finishSave(wire.Result{}, errors.New("model failed"), time.Second, nil)
	}
	if got := DispatchHealth(vetoed.taskKey()).Attempts; got != 0 {
		t.Fatalf("cancelled run charged %d attempts, want 0", got)
	}
	if got := DispatchHealth(plain.taskKey()).Attempts; got != 1 {
		t.Fatalf("uncancelled failed run charged %d attempts, want 1", got)
	}
}

// A reader reaping a cancel-marked record as lost does not charge a failed attempt; an ordinary lost run is charged.
func TestReapLostSkipsHealthForCancel(t *testing.T) {
	setConfigDir(t)
	vetoed := deadRunning(t, nil)
	if _, err := MarkCancelling(vetoed.ID); err != nil {
		t.Fatal(err)
	}
	plain := deadRunning(t, nil)
	for _, r := range []Record{vetoed, plain} {
		if got := reapLost(load(t, r.ID)); got.Status != "lost" {
			t.Fatalf("reapLost status = %q, want lost", got.Status)
		}
	}
	if h := DispatchHealth(vetoed.taskKey()); h.Attempts != 0 {
		t.Fatalf("cancelled reap charged %d attempts", h.Attempts)
	}
	if h := DispatchHealth(plain.taskKey()); h.Attempts != 1 {
		t.Fatalf("plain lost reap charged %d attempts, want 1", h.Attempts)
	}
}

func TestAdoptionSurvivesMarkerFailure(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, nonce, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker := WatchMarker("resumed", rec.ID)
	if err := os.MkdirAll(filepath.Join(marker, "blocked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MarkLaunching(rec.ID, nonce); err != nil {
		t.Fatal(err)
	}
	if err := AdoptResend(rec.ID, nonce, "child"); err != nil {
		t.Fatalf("marker failure aborted a durable adoption: %v", err)
	}
	if got, ok := ResumedAs(rec.ID); !ok || got != "child" || ResumedFrom("child") != rec.ID {
		t.Fatal("adopted claim lost its chain without a readable marker")
	}
	if err := os.RemoveAll(marker); err != nil {
		t.Fatal(err)
	}
	if got := ResumedFrom("child"); got != rec.ID {
		t.Fatalf("missing marker lost reverse adoption: %q", got)
	}
	if err := RollbackResend(rec.ID, "child"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimResend(rec.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackIgnoresStaleMarker(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, nonce, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := AdoptResend(rec.ID, nonce, "child"); err != nil {
		t.Fatal(err)
	}
	if err := RollbackResend(rec.ID, "child"); err != nil {
		t.Fatal(err)
	}
	if err := writeResumedMarker(WatchMarker("resumed", rec.ID), "child"); err != nil {
		t.Fatal(err)
	}
	if next, ok := ResumedAs(rec.ID); ok || next != "" || ResumedFrom("child") != "" || load(t, rec.ID).taken() {
		t.Fatal("stale marker revived a rolled-back adoption")
	}
	if _, _, err := ClaimResend(rec.ID); err != nil {
		t.Fatal(err)
	}
}

// Pruning a rolled-back holder takes its leftover marker too: no record tombstone remains to outvote it.
func TestPruneRemovesRollbackMarker(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, nonce, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := AdoptResend(rec.ID, nonce, "child"); err != nil {
		t.Fatal(err)
	}
	if err := RollbackResend(rec.ID, "child"); err != nil {
		t.Fatal(err)
	}
	marker := WatchMarker("resumed", rec.ID)
	if err := writeResumedMarker(marker, "child"); err != nil {
		t.Fatal(err)
	}
	if !removeRecord(paths.DispatchDir(), rec.ID+".json") {
		t.Fatal("dead holder was not pruned")
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rollback marker outlived its pruned record: %v", err)
	}
	if next, ok := ResumedAs(rec.ID); ok || next != "" || ResumedFrom("child") != "" {
		t.Fatal("pruned holder's marker revived the rolled-back adoption")
	}
}

func TestOriginalJobFencesVoidClaimNonce(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	rec.Resumer = &Claim{Phase: claimLaunching, Nonce: "late", PID: adapterDeadPID(t), Deadline: time.Now().Add(-time.Second)}
	put(t, rec)
	if _, err := ClaimJob(rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := AdoptResend(rec.ID, "late", "late-child"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("late nonce survived original job takeover: %v", err)
	}
}

func TestSaveRefusesUnreadableCancelState(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	path := filepath.Join(filepath.Dir(rec.LogPath()), rec.ID+".json")
	const unreadable = "unreadable newer cancel state"
	if err := os.WriteFile(path, []byte(unreadable), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(); err == nil {
		t.Fatal("Save overwrote an unreadable record")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != unreadable {
		t.Fatalf("failed save changed the existing bytes: %q, %v", got, err)
	}
}

func TestUnreadableIdentityPreventsJobTakeoverAndReap(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	old := procStart
	procStart = func(int) (string, error) { return "", errors.New("identity unreadable") }
	t.Cleanup(func() { procStart = old })
	if got := reapLost(rec); got.Status != "running" {
		t.Fatal("an unreadable runner was marked lost")
	}
	if _, err := ClaimJob(rec.ID); !errors.Is(err, ErrJobNotFresh) {
		t.Fatalf("unreadable runner was taken over: %v", err)
	}
}

func TestClearedClaimSnapshotPreservesNewAdoption(t *testing.T) {
	setConfigDir(t)
	rec := deadRunning(t, nil)
	_, nonce, err := ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseResend(rec.ID, nonce); err != nil {
		t.Fatal(err)
	}
	stale := load(t, rec.ID)
	_, nonce, err = ClaimResend(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := AdoptResend(rec.ID, nonce, "new-child"); err != nil {
		t.Fatal(err)
	}
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}
	if id, ok := ResumedAs(rec.ID); !ok || id != "new-child" {
		t.Fatal("a cleared snapshot overwrote the newer adopted claim")
	}
}
