package memory

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

func TestUpdateSettlementFailureRecordsFailure(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "settle-failure")
	_, err := Init()
	if err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string) (string, error) {
		path := Path("sessionHistory.md")
		if err := os.Remove(path); err != nil {
			return "fixture", err
		}
		return "fixture", os.Mkdir(path, 0o700)
	}
	err = update(updateOptions{CWD: root, Session: "settle-failure", Run: run})
	if err == nil || !strings.Contains(err.Error(), "settle:") {
		t.Fatalf("successful agent with failed settlement returned %v, want settlement error", err)
	}
	result, ok := readUpdateResult("settle-failure")
	if !ok || result.Status != updateFailed {
		t.Fatalf("failed settlement recorded %+v, ok=%v, want failed", result, ok)
	}
}

func TestReadUpdateResultRejectsInvalidTime(t *testing.T) {
	isolatedConfig(t)
	write(t, resultPath("invalid-time"), "status=pending\ntime=invalid\nbank_updated=false\n")
	if result, ok := readUpdateResult("invalid-time"); ok {
		t.Fatalf("malformed timestamp accepted: %+v", result)
	}
}

func TestUpdateSettlementHoldsWriterLock(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "settle-lock")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	initStore(t)
	ready, release := installSettlementBarrier(t)
	history := Path("sessionHistory.md")
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- update(updateOptions{CWD: root, Run: func(context.Context, string, string) (string, error) {
			return "fixture", os.WriteFile(history, []byte("# History\n\n## New session\n\n- Updated the bank.\n"), 0o600)
		}})
	}()
	defer finishSettlement(t, release, done)
	waitSettlementBarrier(t, ready, done)
	lock := lockPathOf() + ".lock"
	ran, err := paths.TryFileLock(lock, 0, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("next writer acquired the store lock while update settlement was still running")
	}
}

func installSettlementBarrier(t *testing.T) (ready, release string) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ready, release = filepath.Join(dir, "ready"), filepath.Join(dir, "release")
	git := filepath.Join(dir, "git")
	body := "#!/bin/sh\nfor arg do\ncase \"$arg\" in\ncommit)\nprintf ready > '" + ready +
		"'\nwhile [ ! -f '" + release + "' ]; do sleep 0.01; done\n;;\nesac\ndone\nexec '" + realGit + "' \"$@\"\n"
	write(t, git, body)
	if err := os.Chmod(git, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return ready, release
}

func waitSettlementBarrier(t *testing.T, ready string, done <-chan error) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				return
			}
		case err := <-done:
			t.Fatalf("update returned before settlement reached commit: %v", err)
		case <-timer.C:
			t.Fatal("settlement did not reach commit")
		}
	}
}

func finishSettlement(t *testing.T, release string, done <-chan error) {
	t.Helper()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Error(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("update: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("update did not finish after releasing settlement")
	}
}

// Given the final receipt write fails, When Update otherwise succeeds, Then the write error is returned.
func TestUpdateReturnsReceiptWriteError(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "receipt-failure")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(resultPath("receipt-failure"), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string) (string, error) { return "fixture", nil }
	err := update(updateOptions{CWD: root, Session: "receipt-failure", Run: run})
	if err == nil || !strings.Contains(err.Error(), "record update result") {
		t.Fatalf("Update = %v, want receipt write error", err)
	}
}

// Given a peer holds the store lock, When a dispatched update gives up as busy, Then its bg stamp claims no work.
func TestBusyUpdateUncoversDispatchStamp(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "busy-uncover")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	prev := updateLockWait
	updateLockWait = 20 * time.Millisecond
	t.Cleanup(func() { updateLockWait = prev })
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := writeStamp(stampPath("busy"), at, "bg", 7); err != nil {
		t.Fatal(err)
	}
	if err := markHandoff("busy", time.Now()); err != nil {
		t.Fatal(err)
	}
	var updateErr error
	ran, err := paths.TryFileLock(lockPathOf()+".lock", 0, func() error {
		updateErr = update(updateOptions{CWD: root, Session: "busy", Run: func(context.Context, string, string) (string, error) {
			t.Error("agent ran while a peer held the store lock")
			return "fixture", nil
		}})
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("hold peer lock: ran=%v err=%v", ran, err)
	}
	if StewardRunning("busy", time.Now()) {
		t.Error("busy worker left its handoff request")
	}
	if updateErr == nil {
		t.Fatal("busy update returned nil, want an error")
	}
	st, ok := readStamp(stampPath("busy"))
	if !ok || st.state != "bg" || st.edits != 0 || !st.at.Equal(at) {
		t.Errorf("stamp after busy update = %+v ok=%v, want bg with 0 covered edits at %v", st, ok, at)
	}
	if res, _ := readUpdateResult("busy"); res.Status != updateFailed {
		t.Errorf("receipt status %q, want failed", res.Status)
	}
}

// Given an update for one repo, When its agent runs, Then the shared home bank's lock is held so other agents queue.
func TestUpdateHoldsHomeBankLockDuringAgentRun(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "home-lock")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	ranAgent := false
	run := func(context.Context, string, string) (string, error) {
		ranAgent = true
		got, err := paths.TryFileLock(lockPathOf()+".lock", 0, func() error { return nil })
		if err != nil {
			t.Error(err)
		}
		if got {
			t.Error("another repo's worker took the home bank lock while this agent ran")
		}
		return "fixture", nil
	}
	if err := update(updateOptions{CWD: root, Session: "home-lock", Run: run}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !ranAgent {
		t.Fatal("agent never ran")
	}
}

// Given a peer holds the store lock past the foreground wait, When a detached update runs, Then it queues and saves.
func TestDetachedUpdateQueuesBehindALongLock(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "detached-wait")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	prevFg, prevDetached := updateLockWait, detachedLockWait
	updateLockWait, detachedLockWait = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { updateLockWait, detachedLockWait = prevFg, prevDetached })
	t.Setenv(DetachedEnv, "1")
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(lockPathOf()+".lock", 0, func() error { close(held); <-release; return nil })
	}()
	<-held
	time.AfterFunc(100*time.Millisecond, func() { close(release) }) // longer than updateLockWait, far inside detachedLockWait
	ranAgent := false
	err := update(updateOptions{CWD: root, Session: "detached", Run: func(context.Context, string, string) (string, error) {
		ranAgent = true
		return "fixture", nil
	}})
	if err != nil || !ranAgent {
		t.Fatalf("detached update behind a lock held 100ms: err=%v ran=%v, want it queued and run", err, ranAgent)
	}
}

func TestUpdatePlansAudit(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "audit")
	hosttest.In(t, root, "branch", "-M", "main")
	origin := t.TempDir()
	hosttest.In(t, origin, "init", "--bare", "-q")
	hosttest.In(t, root, "remote", "add", "origin", origin)
	slug := time.Now().Format(time.DateOnly) + "-update"
	hosttest.In(t, root, "commit", "--allow-empty", "-qm", "feat: fixture\n\nTask: "+slug+" 1.1")
	hosttest.In(t, root, "push", "-q", "origin", "main")
	plan := filepath.Join(plans.Home(root), slug+".md")
	body := "- [ ] 1.1 Fixture\n"
	write(t, plan, body)
	run := func(context.Context, string, string) (string, error) { return "fake", nil }
	var out bytes.Buffer
	opts := updateOptions{Run: run, CWD: root, Out: &out}
	start := time.Now()
	if err := update(opts); err != nil {
		t.Fatal(err)
	}
	t.Logf("fake update: %s", time.Since(start))
	if !strings.Contains(mustRead(t, plan), "[x]") || !strings.Contains(out.String(), "plans: audit 1 plans: ticked 1") {
		t.Fatalf("missing tick or log block: %s", out.String())
	}
	t.Logf("update log block: %s", out.String())
	log := filepath.Join(paths.MemoryStateDir(), "plans-audit.jsonl")
	before := mustRead(t, log)
	t.Logf("audit log line: %s", strings.Split(before, "\n")[0])
	checkUpdateAuditSkips(t, opts, plan, body, log, before)
	checkUpdateAuditWorktree(t, opts, plan, body)
}

func checkUpdateAuditSkips(t *testing.T, opts updateOptions, plan, body, log, before string) {
	t.Helper()
	for _, mode := range []string{"historical", "non-git", "no-home", "quiet", "error"} {
		write(t, plan, body)
		o := opts
		var out bytes.Buffer
		o.Out = &out
		switch mode {
		case "historical":
			o.Historical = true
		case "non-git":
			o.CWD = t.TempDir()
		case "no-home":
			o.CWD = gitRepo(t, t.TempDir(), "no-home")
		case "quiet":
			sha := strings.TrimSpace(hosttest.In(t, opts.CWD, "rev-parse", "--short=7", "HEAD"))
			write(t, plan, "- [x] 1.1 Fixture ("+sha+")\n")
		case "error":
			o.CWD = gitRepo(t, t.TempDir(), "no-trunk")
			write(t, filepath.Join(plans.Home(o.CWD), filepath.Base(plan)), body)
			hosttest.In(t, o.CWD, "branch", "-m", "topic")
			hosttest.In(t, o.CWD, "config", "init.defaultBranch", "missing")
		}
		if err := update(o); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if mode == "error" && !strings.Contains(out.String(), "memory update: plans audit:") {
			t.Fatalf("missing audit error: %s", out.String())
		}
		if mode != "error" && strings.Contains(out.String(), "plans:") {
			t.Fatalf("%s audited: %s", mode, out.String())
		}
		if mustRead(t, log) != before {
			t.Fatalf("%s wrote audit log", mode)
		}
		if mode == "historical" && mustRead(t, plan) != body {
			t.Fatal("replay ticked task")
		}
		t.Logf("%s: %s", mode, strings.TrimSpace(out.String()))
	}
}

func checkUpdateAuditWorktree(t *testing.T, opts updateOptions, plan, body string) {
	t.Helper()
	lane := filepath.Join(t.TempDir(), "lane")
	hosttest.In(t, opts.CWD, "worktree", "add", "-qb", "lane", lane)
	opts.CWD = lane
	write(t, plan, body)
	if err := update(opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mustRead(t, plan), "[x]") {
		t.Fatal("linked worktree did not audit trunk")
	}
	t.Log("linked worktree: ticked pushed trunk task")
}
