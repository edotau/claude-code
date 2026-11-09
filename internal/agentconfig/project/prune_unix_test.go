//go:build unix

package project

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func newLegacySkillFIFO(t *testing.T) (root, fifo string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "legacy")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo = filepath.Join(dir, "SKILL.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
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
