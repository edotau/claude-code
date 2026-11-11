package project

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestSyncCheckReportsPruneReadError(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("initial sync = %d, want 0", code)
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Fatalf("current projections check = %d, want 0", code)
	}
	// ENOTDIR exercises the read-error path even when the test runs with permission bypass privileges.
	legacy := legacyGeminiSkillsDir(home)
	projectWriteFile(t, legacy, "not a directory\n")
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 1 {
		t.Fatalf("check with unreadable prune destination = %d, want 1", code)
	}
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("check with missing optional prune destination = %d, want 0", code)
	}
}

func assertPruneResult(t *testing.T, got, want syncResult) {
	t.Helper()
	if got != want {
		t.Fatalf("prune result = %+v, want %+v", got, want)
	}
}

func TestLegacySkillPrunePreservesMarkerForRetry(t *testing.T) {
	for _, extra := range []string{"notes.txt", "subdir/notes.txt"} {
		t.Run(extra, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "legacy")
			marker := filepath.Join(dir, "SKILL.md")
			content := realSkillBannerPrefix + "legacy/SKILL.md; do not edit by hand. -->\n"
			projectWriteFile(t, marker, content)
			projectWriteFile(t, filepath.Join(dir, extra), "hand-authored\n")
			res := pruneLegacyGeminiSkills(io.Discard, root, false)
			assertPruneResult(t, res, syncResult{errored: 1})
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != content {
				t.Fatalf("blocked prune destroyed retry marker: content %q, err %v", got, err)
			}
			entry, _, _ := strings.Cut(extra, "/")
			if err := os.RemoveAll(filepath.Join(dir, entry)); err != nil {
				t.Fatal(err)
			}
			res = pruneLegacyGeminiSkills(io.Discard, root, false)
			assertPruneResult(t, res, syncResult{pruned: 1})
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("legacy directory survived successful retry: %v", err)
			}
		})
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func newLegacySkillFIFO(t *testing.T) (root, fifo string) {
	t.Helper()
	mkfifo := "/usr/bin/mkfifo" // hosttest's PATH allowlist leaves mkfifo out; coreutils and macOS both ship it here
	if _, err := os.Stat(mkfifo); runtime.GOOS == "windows" || err != nil {
		t.Skip("needs a unix FIFO (mkfifo)")
	}
	root = t.TempDir()
	dir := filepath.Join(root, "legacy")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo = filepath.Join(dir, "SKILL.md")
	if out, err := exec.Command(mkfifo, "-m", "600", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	// Keep the FIFO open so a regressed reader blocks until cleanup, not forever on open.
	pipe, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pipe.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, fifo
}

func TestLegacySkillPruneRejectsFIFO(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(strconv.FormatBool(dryRun), func(t *testing.T) {
			root, fifo := newLegacySkillFIFO(t)
			done := make(chan syncResult, 1)
			go func() { done <- pruneLegacyGeminiSkills(io.Discard, root, dryRun) }()
			select {
			case res := <-done:
				assertPruneResult(t, res, syncResult{})
			case <-time.After(2 * time.Second):
				t.Fatalf("legacy skill prune blocked reading FIFO (dryRun=%v)", dryRun)
			}
			if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("FIFO changed during prune (dryRun=%v): %v", dryRun, err)
			}
		})
	}
}
