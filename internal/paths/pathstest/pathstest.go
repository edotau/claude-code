// Package pathstest provides isolated vendor executables for tests.
package pathstest

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// FakeVendor writes a shell executable into the test config's bin directory.
func FakeVendor(t testing.TB, name, body string) string {
	t.Helper()
	bin, ok := paths.AgentName(name)
	if !ok {
		t.Fatalf("unknown vendor %q", name)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cfg := canonical(t, paths.ConfigDir())
	if cfg == canonical(t, filepath.Join(u.HomeDir, ".claude")) {
		t.Fatal("refusing to write a fake vendor into the real ~/.claude")
	}
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if bindir := canonical(t, paths.BinDir()); !paths.Within(bindir, cfg) {
		t.Fatalf("refusing to write a fake vendor: bin dir %q resolves outside %q", bindir, cfg)
	}
	p := filepath.Join(paths.BinDir(), bin)
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("refusing to overwrite symlink %q: it may point at an external executable", p)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// canonical returns the absolute, symlink-resolved form of p; a missing path is returned as its absolute form.
func canonical(t testing.TB, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if os.IsNotExist(err) {
		return abs
	}
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
