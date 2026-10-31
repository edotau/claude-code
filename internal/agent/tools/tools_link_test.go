package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestLinkVendorsLinksPathEntryAndIsIdempotent(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.path1, "agy")
	resolved := filepath.Join(t.TempDir(), "agy")
	writeToolsFixture(t, resolved, "#!/bin/sh\nexit 0\n", 0o755)
	symlinkToolsFixture(t, resolved, candidate)
	if candidate == resolved {
		t.Fatal("PATH entry must differ from its resolved target")
	}

	var first bytes.Buffer
	Link(&first, []string{"agy"}, false)
	dst := filepath.Join(paths.BinDir(), "gemini")
	if got := readlink(dst); got != candidate {
		t.Fatalf("gemini link target = %q, want PATH entry %q", got, candidate)
	}
	if want := "link     " + dst + " → " + candidate; !strings.Contains(first.String(), want) {
		t.Errorf("first output = %q, want %q", first.String(), want)
	}

	var second bytes.Buffer
	Link(&second, []string{"agy"}, false)
	if want := "ok       " + dst + " → " + candidate; !strings.Contains(second.String(), want) {
		t.Errorf("second output = %q, want %q", second.String(), want)
	}
}

func TestLinkVendorsKeepsDirectFileAndReportsPathCopy(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.path1, "codex")
	writeToolsFixture(t, candidate, "#!/bin/sh\nexit 0\n", 0o755)
	dst := filepath.Join(paths.BinDir(), "codex")
	writeToolsFixture(t, dst, "#!/bin/sh\nexit 0\n", 0o755)

	var out bytes.Buffer
	Link(&out, []string{"codex"}, false)
	if got, err := os.Lstat(dst); err != nil || !got.Mode().IsRegular() {
		t.Fatalf("direct install changed: stat = %v, err = %v", got, err)
	}
	if want := "keep     " + dst + " (installed here) · also on PATH: " + candidate; !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestLinkVendorsRemovesLegacyShimLinkWithoutCandidate(t *testing.T) {
	setupToolsTestEnv(t)
	legacy := filepath.Join(t.TempDir(), "claude-code")
	writeToolsFixture(t, legacy, "#!/bin/sh\nexit 0\n", 0o755)
	dst := filepath.Join(paths.BinDir(), "copilot")
	symlinkToolsFixture(t, legacy, dst)

	var out bytes.Buffer
	Link(&out, []string{"copilot"}, false)
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("legacy link remains: Lstat err = %v", err)
	}
	if !strings.Contains(out.String(), "remove   "+dst+"; copilot not installed (claude-code install --tools=copilot)") {
		t.Errorf("output = %q, want legacy-link removal", out.String())
	}
}

func TestLinkVendorsRemovesDanglingLink(t *testing.T) {
	setupToolsTestEnv(t)
	dst := filepath.Join(paths.BinDir(), "opencode")
	symlinkToolsFixture(t, filepath.Join(t.TempDir(), "missing"), dst)

	var out bytes.Buffer
	Link(&out, []string{"opencode"}, false)
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("dangling link remains: Lstat err = %v", err)
	}
	if !strings.Contains(out.String(), "remove   "+dst+"; opencode not installed") {
		t.Errorf("output = %q, want dangling-link removal", out.String())
	}
}

func TestLinkVendorsKeepsWorkingLinkWithoutCandidate(t *testing.T) {
	setupToolsTestEnv(t)
	target := filepath.Join(t.TempDir(), "opencode-real")
	writeToolsFixture(t, target, "#!/bin/sh\nexit 0\n", 0o755)
	dst := filepath.Join(paths.BinDir(), "opencode")
	symlinkToolsFixture(t, target, dst)

	var out bytes.Buffer
	Link(&out, []string{"opencode"}, false)
	if got := readlink(dst); got != target {
		t.Fatalf("existing link target changed: got %q, want untouched %q", got, target)
	}
	if want := "keep     " + dst + " → " + target + " (existing link)"; !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// TestLinkVendorsKeepsRealCodexInstallNotSymlink confirms a real codex binary already at
// BinDir()/codex is left as a regular file, never swapped for a symlink to a different PATH copy.
func TestLinkVendorsKeepsRealCodexInstallNotSymlink(t *testing.T) {
	env := setupToolsTestEnv(t)
	pathCopy := filepath.Join(env.path1, "codex")
	writeToolsFixture(t, pathCopy, "#!/bin/sh\nprintf 'path copy\\n'\n", 0o755)
	dst := filepath.Join(paths.BinDir(), "codex")
	writeToolsFixture(t, dst, "#!/bin/sh\nprintf 'real install\\n'\n", 0o755)

	var out bytes.Buffer
	Link(&out, []string{"codex"}, false)
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", dst, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s became a symlink (→ %s), want the real install left untouched", dst, readlink(dst))
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file: mode %v", dst, fi.Mode())
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "#!/bin/sh\nprintf 'real install\\n'\n" {
		t.Errorf("%s contents = %q, %v, want the original install untouched", dst, got, err)
	}
}

// TestLinkVendorsRelinksSymlinkWhenNoRealCodexInstall preserves the existing behavior: with no
// real file at BinDir()/codex, a stale symlink is replaced to point at the current PATH candidate.
func TestLinkVendorsRelinksSymlinkWhenNoRealCodexInstall(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.path1, "codex")
	writeToolsFixture(t, candidate, "#!/bin/sh\nexit 0\n", 0o755)
	dst := filepath.Join(paths.BinDir(), "codex")
	oldTarget := filepath.Join(t.TempDir(), "old-codex")
	writeToolsFixture(t, oldTarget, "#!/bin/sh\nexit 0\n", 0o755)
	symlinkToolsFixture(t, oldTarget, dst)

	var out bytes.Buffer
	Link(&out, []string{"codex"}, false)
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", dst, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a symlink, want the symlink-swap behavior preserved", dst)
	}
	if got := readlink(dst); got != candidate {
		t.Errorf("relinked target = %q, want the current PATH candidate %q", got, candidate)
	}
	if want := "link     " + dst + " → " + candidate; !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestReplaceLinkKeepsRegularFileWithoutReplacing(t *testing.T) {
	setupToolsTestEnv(t)
	dst := filepath.Join(t.TempDir(), "codex")
	writeToolsFixture(t, dst, "#!/bin/sh\nexit 0\n", 0o755)
	target := filepath.Join(t.TempDir(), "codex-on-path")
	writeToolsFixture(t, target, "#!/bin/sh\nexit 0\n", 0o755)

	kept, err := replaceLink(dst, target)
	if err != nil {
		t.Fatal(err)
	}
	if !kept {
		t.Error("kept = false, want true: dst is a regular file and must never be replaced by a link")
	}
	if fi, err := os.Lstat(dst); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("dst changed: stat = %v, err = %v", fi, err)
	}
}

func TestLinkVendorsReportsMissingTool(t *testing.T) {
	setupToolsTestEnv(t)
	var out bytes.Buffer
	Link(&out, []string{"claude"}, false)
	if !strings.Contains(out.String(), "missing  claude not installed (claude-code install --tools=claude)") {
		t.Errorf("output = %q, want missing-tool report", out.String())
	}
}

func TestLinkVendorsUsesLocalBinFallback(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.home, ".local", "bin", "claude")
	writeToolsFixture(t, candidate, "#!/bin/sh\nexit 0\n", 0o755)

	var out bytes.Buffer
	Link(&out, []string{"claude"}, false)
	dst := filepath.Join(paths.BinDir(), "claude")
	if got := readlink(dst); got != candidate {
		t.Fatalf("claude link target = %q, want local-bin candidate %q", got, candidate)
	}
	if !strings.Contains(out.String(), "link     "+dst+" → "+candidate) {
		t.Errorf("output = %q, want local-bin link", out.String())
	}
}

func TestLinkVendorsMarksNPMGlobalCandidate(t *testing.T) {
	env := setupToolsTestEnv(t)
	target := filepath.Join(t.TempDir(), "prefix", "lib", "node_modules", "@github", "copilot", "bin", "copilot")
	writeToolsFixture(t, target, "#!/bin/sh\nexit 0\n", 0o755)
	candidate := filepath.Join(env.path1, "copilot")
	symlinkToolsFixture(t, target, candidate)

	var out bytes.Buffer
	Link(&out, []string{"copilot"}, false)
	dst := filepath.Join(paths.BinDir(), "copilot")
	if got := readlink(dst); got != candidate {
		t.Fatalf("copilot link target = %q, want PATH entry %q", got, candidate)
	}
	if !strings.Contains(out.String(), "link     "+dst+" → "+candidate+" (npm-global)") {
		t.Errorf("output = %q, want npm-global marker", out.String())
	}
}

func TestLinkVendorsDryRunDoesNotChangeDisk(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.path1, "codex")
	writeToolsFixture(t, candidate, "#!/bin/sh\nexit 0\n", 0o755)
	dst := filepath.Join(paths.BinDir(), "codex")
	oldTarget := filepath.Join(t.TempDir(), "old-codex")
	symlinkToolsFixture(t, oldTarget, dst)

	var out bytes.Buffer
	Link(&out, []string{"codex"}, true)
	if got := readlink(dst); got != oldTarget {
		t.Fatalf("dry-run changed link target to %q, want %q", got, oldTarget)
	}
	if !strings.Contains(out.String(), "link     "+dst+" → "+candidate) {
		t.Errorf("output = %q, want planned link", out.String())
	}
}

func TestLinkVendorsLinksOnlyNamedTools(t *testing.T) {
	env := setupToolsTestEnv(t)
	for _, name := range []string{"codex", "copilot"} {
		writeToolsFixture(t, filepath.Join(env.path1, name), "#!/bin/sh\nexit 0\n", 0o755)
	}

	var out bytes.Buffer
	Link(&out, []string{"codex"}, false)
	if got := readlink(filepath.Join(paths.BinDir(), "codex")); got != filepath.Join(env.path1, "codex") {
		t.Errorf("codex link target = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(paths.BinDir(), "copilot")); !os.IsNotExist(err) {
		t.Errorf("unselected copilot was linked: Lstat err = %v", err)
	}
}

func TestStatusOneTargetIsLinkTargetOnly(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.path1, "codex")
	writeToolsFixture(t, candidate, "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	tool, ok := Lookup("codex")
	if !ok {
		t.Fatal("codex tool is missing")
	}

	Link(&bytes.Buffer{}, []string{"codex"}, false)
	dst := filepath.Join(paths.BinDir(), "codex")
	wantTarget, err := os.Readlink(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOne(tool).Target; got != wantTarget {
		t.Errorf("linked Target = %q, want Readlink %q", got, wantTarget)
	}

	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	writeToolsFixture(t, dst, "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	if got := statusOne(tool).Target; got != "" {
		t.Errorf("regular-file Target = %q, want empty", got)
	}
}
