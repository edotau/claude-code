package launch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestToolPATHCopiesAndExtras(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	bin := paths.BinDir()
	primary := pathstest.FakeVendor(t, "opencode", "echo 1.0.0")
	aBin := vendorPATHFixture(t, "opencode", "echo 1.0.0")
	bBin := vendorPATHFixture(t, "opencode", "echo 2.0.0")
	a, b, c := filepath.Dir(aBin), filepath.Dir(bBin), filepath.Join(scratch, "C")
	// C/opencode is a symlink to A/opencode: same real file, must dedupe.
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aBin, filepath.Join(c, "opencode")); err != nil {
		t.Fatal(err)
	}
	join := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

	cases := []struct {
		name       string
		path       string
		wantCopies []string
	}{
		{"bin first", join(bin, a, b), []string{aBin, bBin}},
		{"tool before bin", join(a, bin, b), []string{aBin, bBin}},
		{"no bin dir", join(a, b), []string{aBin, bBin}},
		{"symlink dedup", join(bin, a, c), []string{aBin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			copies := toolPATHCopies("opencode")
			if !slices.Equal(copies, tc.wantCopies) {
				t.Fatalf("toolPATHCopies = %v; want %v", copies, tc.wantCopies)
			}
		})
	}

	status := agent.ToolStatus{Name: "opencode", Path: aBin, Version: "1.0.0"}
	for _, order := range []string{join(a, bin, b), join(bin, a, b)} {
		t.Run(order, func(t *testing.T) {
			t.Setenv("PATH", order)
			detail := toolDoctorDetail(status)
			want := primary + " 1.0.0 (direct install in bin/)" +
				" · also on PATH: " + aBin + " 1.0.0 · also on PATH: " + bBin + " 2.0.0"
			if detail != want {
				t.Errorf("detail = %q, want %q", detail, want)
			}
		})
	}
}

func TestDoctorVendorRows(t *testing.T) {
	for _, kind := range []string{"link", "direct", "broken link", "retired shim"} {
		t.Run(kind, func(t *testing.T) {
			doctorScratch(t)
			t.Setenv("HOME", t.TempDir())
			source := vendorPATHFixture(t, "opencode", "echo 1.2.3")
			t.Setenv("PATH", filepath.Dir(source))
			dst := pathstest.FakeVendor(t, "opencode", "echo 4.5.6")
			want := dst + " 4.5.6 (direct install in bin/)" +
				" · also on PATH: " + source + " 1.2.3"
			if kind != "direct" {
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
				target := source
				switch kind {
				case "link":
					want = dst + " → " + source + " 1.2.3"
				case "broken link":
					target = filepath.Join(t.TempDir(), "missing")
					want = "opencode: " + dst + " is a broken link (run claude-code install) · on PATH: " + source
				case "retired shim":
					target = filepath.Join(t.TempDir(), "claude-code")
					if err := os.WriteFile(target, []byte("must not execute"), 0o755); err != nil {
						t.Fatal(err)
					}
					want = "opencode: " + dst + " is the retired claude-code shim (run claude-code install) · on PATH: " + source
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			}
			checks, _ := DoctorChecks()
			row := findCheck(checks, "tool opencode")
			if row == nil {
				t.Fatal("missing tool opencode row")
			}
			if row.Detail != want {
				t.Errorf("detail = %q, want %q", row.Detail, want)
			}
			if wantOK := kind == "link" || kind == "direct"; row.OK != wantOK {
				t.Errorf("OK = %v, want %v", row.OK, wantOK)
			}
		})
	}
}
