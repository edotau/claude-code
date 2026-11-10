package paths_test

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestVendorBinCases(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{"unknown name", vendorUnknownName},
		{"missing gemini names path and install tool", vendorMissingGemini},
		{"dangling symlink is broken link", vendorDanglingSymlink},
		{"symlink to executable is retired shim", vendorSelfSymlink},
		{"symlink to claude-code file is retired shim", vendorClaudeCodeSymlink},
		{"0644 file is not executable", vendorNonExecFile},
		{"directory is not executable", vendorDirectory},
		{"regular file returns bin path", vendorRegularFile},
		{"agy and gemini resolve to gemini bin path", vendorAgyAlias},
		{"symlink returns its own path", vendorSymlinkOwnPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			tt.run(t)
		})
	}
}

// binDirReady creates and returns the shim directory.
func binDirReady(t *testing.T) string {
	t.Helper()
	binDir := paths.BinDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

// linkCodex symlinks BinDir/codex to target and returns the link path.
func linkCodex(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(binDirReady(t), "codex")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// writeScript writes an executable shell stub at path.
func writeScript(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// codexErrContains asserts AgentBin("codex") fails with every fragment in its message.
func codexErrContains(t *testing.T, fragments ...string) {
	t.Helper()
	_, err := paths.AgentBin("codex")
	for _, fragment := range fragments {
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("AgentBin(codex) error = %v, want substring %q", err, fragment)
		}
	}
}

func vendorUnknownName(t *testing.T) {
	_, err := paths.AgentBin("unknown")
	if err == nil || !strings.Contains(err.Error(), `unknown vendor "unknown"`) {
		t.Fatalf("AgentBin(unknown) error = %v, want unknown vendor message", err)
	}
}

func vendorMissingGemini(t *testing.T) {
	_, err := paths.AgentBin("gemini")
	if err == nil {
		t.Fatal("AgentBin(gemini) succeeded for missing binary")
	}
	for _, fragment := range []string{
		filepath.Join(paths.BinDir(), "gemini"),
		"--tools=agy",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("AgentBin(gemini) error %q does not contain %q", err, fragment)
		}
	}
}

func vendorDanglingSymlink(t *testing.T) {
	link := linkCodex(t, filepath.Join(t.TempDir(), "missing"))
	codexErrContains(t, "broken link", link)
}

func vendorSelfSymlink(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := linkCodex(t, self)
	codexErrContains(t, "retired claude-code shim", link)
}

func vendorClaudeCodeSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "claude-code")
	writeScript(t, target)
	link := linkCodex(t, target)
	codexErrContains(t, "retired claude-code shim", link)
}

func vendorNonExecFile(t *testing.T) {
	link := filepath.Join(binDirReady(t), "codex")
	if err := os.WriteFile(link, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(link, 0o644); err != nil {
		t.Fatal(err)
	}
	codexErrContains(t, "not executable", link)
}

func vendorDirectory(t *testing.T) {
	link := filepath.Join(paths.BinDir(), "codex")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	codexErrContains(t, "not executable", link)
}

func vendorRegularFile(t *testing.T) {
	got := pathstest.FakeVendor(t, "codex", "exit 0")
	want := filepath.Join(paths.BinDir(), "codex")
	if got != want {
		t.Fatalf("FakeVendor path = %q, want %q", got, want)
	}
	if resolved, err := paths.AgentBin("codex"); err != nil || resolved != want {
		t.Fatalf("AgentBin(codex) = %q, %v; want %q, nil", resolved, err, want)
	}
}

func vendorAgyAlias(t *testing.T) {
	pathstest.FakeVendor(t, "agy", "exit 0")
	want := filepath.Join(paths.BinDir(), "gemini")
	for _, name := range []string{"agy", "gemini"} {
		got, err := paths.AgentBin(name)
		if err != nil || got != want {
			t.Errorf("AgentBin(%q) = %q, %v; want %q, nil", name, got, err, want)
		}
	}
}

func vendorSymlinkOwnPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "codex")
	writeScript(t, target)
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := linkCodex(t, target)
	got, err := paths.AgentBin("codex")
	if err != nil || got != link {
		t.Fatalf("AgentBin(codex) = %q, %v; want link path %q", got, err, link)
	}
	if got == target {
		t.Fatalf("AgentBin(codex) returned target %q instead of link %q", got, link)
	}
}

func TestLookPathRealSkipsBinDirSymlink(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vendorDir := t.TempDir()
	vendor := filepath.Join(vendorDir, "codex")
	writeScript(t, vendor)
	linkCodex(t, vendor)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(paths.BinDir(), alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", alias+string(os.PathListSeparator)+vendorDir)

	got, err := paths.LookPathReal("codex")
	if err != nil || got != vendor {
		t.Fatalf("LookPathReal(codex) = %q, %v; want external vendor %q, not BinDir alias", got, err, vendor)
	}
	t.Setenv("PATH", alias)
	if got, err := paths.LookPathReal("codex"); err == nil || got != "" {
		t.Fatalf("LookPathReal(codex) = %q, %v; BinDir alias alone must not resolve", got, err)
	}
}

func TestVendorName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	if got, ok := paths.AgentName("agy"); !ok || got != "gemini" {
		t.Fatalf("AgentName(agy) = %q, %t; want gemini, true", got, ok)
	}
}

func TestFakeVendorRefusesRealConfig(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", u.HomeDir)

	tb := &fatalTB{TB: t}
	returned := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pathstest.FakeVendor(tb, "codex", "exit 0")
		returned <- struct{}{}
	}()
	<-done

	if !tb.called {
		t.Fatal("FakeVendor did not call Fatal")
	}
	for _, fragment := range []string{"refusing to write", "real ~/.claude"} {
		if !strings.Contains(tb.message, fragment) {
			t.Errorf("Fatal message %q does not contain %q", tb.message, fragment)
		}
	}
	select {
	case <-returned:
		t.Fatal("FakeVendor returned instead of stopping before writing")
	default:
	}
}

type fatalTB struct {
	testing.TB
	called  bool
	message string
}

func (tb *fatalTB) Fatal(args ...any) {
	tb.called = true
	tb.message = fmt.Sprint(args...)
	runtime.Goexit()
}

func (tb *fatalTB) Fatalf(format string, args ...any) {
	tb.called = true
	tb.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestAgentBinAbsolutizesRelativeEnvDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Mkdir(filepath.Join(cwd, "fakebin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(cwd, "fakebin", "codex"))
	t.Setenv(paths.AgentBinDirEnv, "fakebin")

	got, err := paths.AgentBin("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("AgentBin(codex) = %q, want an absolute path", got)
	}
}

// TestBinKeyTracksSizeAndMtime: a rewrite of the same path changes the key; a missing path has none.
func TestBinKeyTracksSizeAndMtime(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tool")
	if _, ok := paths.BinKey(bin); ok {
		t.Fatal("BinKey of a missing file reported ok")
	}
	if err := os.WriteFile(bin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	k1, ok := paths.BinKey(bin)
	if !ok || !strings.HasPrefix(k1, bin+"|2|") {
		t.Fatalf("BinKey = %q, %v; want path|size|mtime", k1, ok)
	}
	if err := os.WriteFile(bin, []byte("v22"), 0o755); err != nil {
		t.Fatal(err)
	}
	if k2, _ := paths.BinKey(bin); k2 == k1 {
		t.Errorf("BinKey unchanged after an upgrade: %q", k2)
	}
}
