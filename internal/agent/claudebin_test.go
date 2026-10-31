package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestClaudeBinaryPrefersBinDirOverVersions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	versions := filepath.Join(os.Getenv("HOME"), ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.0.9", "2.0.14"} {
		if err := os.WriteFile(filepath.Join(versions, version), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := pathstest.FakeVendor(t, "claude", "")
	got, err := ClaudeBinary()
	if err != nil || got != bin {
		t.Fatalf("ClaudeBinary = %q, %v; want %q", got, err, bin)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(versions, "2.0.14")
	got, err = ClaudeBinary()
	if err != nil || got != want {
		t.Fatalf("without bin/claude = %q, %v; want %q", got, err, want)
	}
}

func TestClaudeBinaryDoesNotSearchPATH(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	source := vendorPATHFixture(t, "claude", "")
	t.Setenv("PATH", filepath.Dir(source))
	if bin, err := ClaudeBinary(); err == nil || !strings.Contains(err.Error(), "not found in") || bin != "" {
		t.Fatalf("ClaudeBinary = %q, %v; want not found despite PATH vendor", bin, err)
	}
}
