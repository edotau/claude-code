package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/shell"
)

// rtkFixture installs a pinned rtk under a temp CLAUDE_CONFIG_DIR and points userBinDir at a temp dir.
func rtkFixture(t *testing.T) (pin, userBin string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(shell.RTKBinaryEnv, "")
	pin = shell.RTKPinned()
	writeExec(t, pin, "#!/bin/sh\necho 'rtk 1.1.0'\n")
	userBin = t.TempDir()
	orig := userBinDir
	userBinDir = func() string { return userBin }
	t.Cleanup(func() { userBinDir = orig })
	return pin, userBin
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Install links userBinDir/rtk to the pin like claude-code, and reports, never replaces, a real file there.
func TestLinkUserBinsLinksPinnedRTK(t *testing.T) {
	pin, userBin := rtkFixture(t)
	t.Setenv("PATH", userBin)
	var out bytes.Buffer
	if err := linkUserBins(&out, "/opt/bin/claude-code", false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(userBin, "rtk")); err != nil || got != pin {
		t.Fatalf("userBin/rtk -> %q (err %v), want %q; output:\n%s", got, err, pin, out.String())
	}
	real := filepath.Join(t.TempDir(), "rtk")
	writeExec(t, real, "#!/bin/sh\n")
	userBin2 := filepath.Dir(real)
	userBinDir = func() string { return userBin2 }
	out.Reset()
	if err := linkUserBins(&out, "/opt/bin/claude-code", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "skip     "+real+" (a real file is there)") {
		t.Errorf("real rtk file not reported: %q", out.String())
	}
	if fi, err := os.Lstat(real); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("real rtk file was replaced: %v %v", fi, err)
	}
}

// Doctor passes only when PATH's rtk is the pin; a shadow or a missing PATH entry fails naming the fix.
func TestRTKDoctorStatusNamesShadow(t *testing.T) {
	pin, userBin := rtkFixture(t)
	if err := os.Symlink(pin, filepath.Join(userBin, "rtk")); err != nil {
		t.Fatal(err)
	}
	shadowDir := t.TempDir()
	mark := filepath.Join(t.TempDir(), "ran")
	writeExec(t, filepath.Join(shadowDir, "rtk"), "#!/bin/sh\n: > '"+mark+"'\necho 'rtk 0.42.4'\n")
	for _, tc := range []struct {
		name, path, want string
		ok               bool
	}{
		{"pin first", userBin + ":" + shadowDir, "PATH rtk is this binary", true},
		{"shadow first", shadowDir + ":" + userBin, "PATH rtk " + filepath.Join(shadowDir, "rtk") + " shadows " +
			pin + " — run make install, or put ~/.local/bin first on PATH", false},
		{"none on PATH", t.TempDir(), pin + " is not on PATH — run make install", false},
	} {
		t.Setenv("PATH", tc.path)
		ok, detail := rtkDoctorStatus()
		if ok != tc.ok || !strings.Contains(detail, tc.want) {
			t.Errorf("%s: (%v, %q), want (%v, …%q…)", tc.name, ok, detail, tc.ok, tc.want)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("doctor executed the shadowing rtk")
	}
}
