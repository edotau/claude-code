package launch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/tools"
)

func writeToolScript(t *testing.T, dir, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestToolPATHCopiesFlagsShadowAndExtras(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	shim := filepath.Join(scratch, "bin")
	if err := os.MkdirAll(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	a, b, c := filepath.Join(scratch, "A"), filepath.Join(scratch, "B"), filepath.Join(scratch, "C")
	aBin, bBin := writeToolScript(t, a, "1.0.0"), writeToolScript(t, b, "2.0.0")
	// C/opencode is a symlink to A/opencode: same real file, must dedupe.
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aBin, filepath.Join(c, "opencode")); err != nil {
		t.Fatal(err)
	}
	join := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

	cases := []struct {
		name        string
		path        string
		wantCopies  []string
		wantShadows bool
	}{
		{"shim first", join(shim, a, b), []string{aBin, bBin}, false},
		{"tool before shim", join(a, shim, b), []string{aBin, bBin}, true},
		{"no shim dir", join(a, b), []string{aBin, bBin}, false},
		{"symlink dedup", join(shim, a, c), []string{aBin}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			copies, shadows := toolPATHCopies("opencode")
			if !slices.Equal(copies, tc.wantCopies) || shadows != tc.wantShadows {
				t.Fatalf("toolPATHCopies = %v, %v; want %v, %v", copies, shadows, tc.wantCopies, tc.wantShadows)
			}
		})
	}

	status := tools.Status{Name: "opencode", Path: aBin, Version: "1.0.0"}
	t.Run("detail shadowed", func(t *testing.T) {
		t.Setenv("PATH", join(a, shim, b))
		detail := toolDoctorDetail(status)
		for _, want := range []string{"shadows the shim", "also on PATH: " + bBin + " 2.0.0"} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail %q missing %q", detail, want)
			}
		}
	})
	t.Run("detail shim first", func(t *testing.T) {
		t.Setenv("PATH", join(shim, a, b))
		detail := toolDoctorDetail(status)
		if !strings.Contains(detail, "also on PATH") || strings.Contains(detail, "shadows") {
			t.Errorf("detail %q: want extras without shadow warning", detail)
		}
	})
}

func TestInteractiveVendorArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"--version"}, false},
		{[]string{"-v"}, false},
		{[]string{"-V"}, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
		{[]string{"run", "x"}, false},
		{[]string{"--model", "m"}, true},
		{[]string{"chat", "--version"}, false},
	}
	for _, tc := range cases {
		if got := interactiveVendorArgs(tc.args); got != tc.want {
			t.Errorf("interactiveVendorArgs(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
