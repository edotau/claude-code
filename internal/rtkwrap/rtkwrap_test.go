package rtkwrap

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

func TestLaunchSetsBashEnvOnlyWhenWrapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shims are unix-only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	must(t, os.MkdirAll(filepath.Dir(Binary()), 0o755))
	must(t, os.WriteFile(Binary(), []byte("#!/bin/sh\n"), 0o755))
	file := filepath.Join(Dir(), bashEnvName)
	env := Launch([]string{"BASH_ENV=/u/rc"}, "on", "", io.Discard)
	if !slices.Contains(env, "BASH_ENV="+file) || !slices.Contains(env, UserBashEnvVar+"=/u/rc") || slices.Contains(env, "BASH_ENV=/u/rc") {
		t.Errorf("wrapping: env = %v", env)
	}
	if raw, err := os.ReadFile(file); err != nil || !strings.Contains(string(raw), "trap : EXIT") {
		t.Errorf("%s = %q, %v", file, raw, err)
	}
	setsBashEnv := func(kv string) bool { return strings.HasPrefix(kv, "BASH_ENV=") }
	if env := Launch(nil, "on", "only", io.Discard); slices.ContainsFunc(env, setsBashEnv) {
		t.Errorf("code mode with RTK_WRAP=on: env sets BASH_ENV: %v", env)
	}
	if env := Launch(nil, "off", "on", io.Discard); slices.ContainsFunc(env, setsBashEnv) {
		t.Errorf("code mode alone: env sets BASH_ENV: %v", env)
	}
}
