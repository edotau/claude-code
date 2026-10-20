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
	bin, ok := paths.VendorName(name)
	if !ok {
		t.Fatalf("unknown vendor %q", name)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	canonical := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if os.IsNotExist(err) {
			return abs
		}
		if err != nil {
			t.Fatal(err)
		}
		return real
	}
	if canonical(paths.ConfigDir()) == canonical(filepath.Join(u.HomeDir, ".claude")) {
		t.Fatal("refusing to write a fake vendor into the real ~/.claude")
	}
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(paths.BinDir(), bin)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
