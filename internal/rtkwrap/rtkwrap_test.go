package rtkwrap

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/shell"
)

// TestNamesAreRTKWords keeps every shim a command rtk rewrites, so a shim never wraps what rtk cannot compress.
func TestNamesAreRTKWords(t *testing.T) {
	words := shell.GuardSet("rtk_rewrite_words")
	for _, n := range Names {
		if !words[n] {
			t.Errorf("%s is shimmed but not in guards.json rtk_rewrite_words", n)
		}
	}
}

func TestEnsureConcurrentLeavesValidDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shims are unix-only")
	}
	bin := filepath.Join(t.TempDir(), "shim")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	dir := filepath.Join(t.TempDir(), "bin")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = Ensure(dir, bin) })
	}
	wg.Wait()
	for _, err := range errs {
		must(t, err)
	}
	for _, n := range Names {
		if got, err := os.Readlink(filepath.Join(dir, n)); err != nil || got != bin {
			t.Fatalf("%s -> %q, %v; want %q", n, got, err, bin)
		}
	}
}
