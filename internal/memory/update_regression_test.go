package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestUpdateSettlementFailureRecordsFailure(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "settle-failure")
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string) (string, error) {
		path := filepath.Join(dir, "sessionHistory.md")
		if err := os.Remove(path); err != nil {
			return "fixture", err
		}
		return "fixture", os.Mkdir(path, 0o700)
	}
	err = update(updateOptions{CWD: root, Session: "settle-failure", Run: run})
	if err == nil || !strings.Contains(err.Error(), "settle:") {
		t.Fatalf("successful agent with failed settlement returned %v, want settlement error", err)
	}
	result, ok := ReadUpdateResult("settle-failure")
	if !ok || result.Status != UpdateFailed {
		t.Fatalf("failed settlement recorded %+v, ok=%v, want failed", result, ok)
	}
}

func TestReadUpdateResultRejectsInvalidTime(t *testing.T) {
	isolatedConfig(t)
	write(t, resultPath("invalid-time"), "status=pending\ntime=invalid\nbank_updated=false\n")
	if result, ok := ReadUpdateResult("invalid-time"); ok {
		t.Fatalf("malformed timestamp accepted: %+v", result)
	}
}

func TestUpdateSettlementHoldsWriterLock(t *testing.T) {
	clearChildEnv(t)
	isolatedConfig(t)
	root := repoRoot(t, "settle-lock")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	initStore(t)
	ready, release := installSettlementBarrier(t)
	history := filepath.Join(Dir(root), "sessionHistory.md")
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- update(updateOptions{CWD: root, Run: func(context.Context, string, string) (string, error) {
			return "fixture", os.WriteFile(history, []byte("# History\n\n## New session\n\n- Updated the bank.\n"), 0o600)
		}})
	}()
	defer finishSettlement(t, release, done)
	waitSettlementBarrier(t, ready, done)
	lock := lockPath(Dir(root)) + ".lock"
	ran, err := paths.TryFileLock(lock, 0, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("next writer acquired the store lock while update settlement was still running")
	}
}

func installSettlementBarrier(t *testing.T) (ready, release string) {
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
