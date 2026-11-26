package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	_ "github.com/edotau/claude-code/internal/driver/codex"    // registers the codex engine the prod binary links
	_ "github.com/edotau/claude-code/internal/driver/gemini"   // registers the gemini route engine the prod binary links
	_ "github.com/edotau/claude-code/internal/driver/opencode" // registers the opencode engine the prod binary links
	"github.com/edotau/claude-code/internal/platform/clipboard"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/paths/pathstest"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/agentid"
)

// TestSnapshotPreInstallWarnsLoudlyOnFailure
func TestSnapshotPreInstallWarnsLoudlyOnFailure(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if got := snapshotPreInstall(&buf, false, true); got != "" {
		t.Fatalf("commit = %q, want empty on failure", got)
	}
	out := buf.String()
	if !strings.Contains(out, "warning  history snapshot failed") || !strings.Contains(out, "no rollback point") {
		t.Fatalf("output lacks loud warning: %q", out)
	}
}

func installTestBinary(t *testing.T) {
	t.Helper()
	self := filepath.Join(t.TempDir(), "claude-code")
	for _, name := range []string{"claude-code", paths.HookBinaryName} { // install refuses without the hooks binary
		if err := os.WriteFile(filepath.Join(filepath.Dir(self), name), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	selfBinary = func() (string, error) { return self, nil }
}

func TestInstallKeepsExistingSettings(t *testing.T) {
	hermetic(t)
	codex := pathstest.FakeVendorOutsideConfig(t, "codex", "")
	t.Setenv("PATH", filepath.Dir(codex))
	installTestBinary(t)
	file := settings.SettingsFile()
	orig := `{"spinnerTipsEnabled": false, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "foreign stop"}]}]}}`
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := install(&out, true, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Fatal("--dry-run wrote settings.json")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(file), "bin", "claude")); err == nil {
		t.Fatal("--dry-run created vendor links")
	}
	if err := install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	assertInstalledSettings(t, file, orig)
	assertInstalledLinks(t, file, codex)
}

func assertInstalledSettings(t *testing.T, file, orig string) {
	t.Helper()
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Errorf("install wrote an existing settings.json:\n%s", b)
	}
	if b, _ := os.ReadFile(tmuxConfPaths()[0]); !bytes.Equal(b, assets.TmuxConf()) {
		t.Errorf("tmux.conf not seeded: %q", b)
	}
	if err := settings.CheckFile(file); err != nil {
		t.Errorf("doctor validity check: %v", err)
	}
}

func assertInstalledLinks(t *testing.T, file, codex string) {
	t.Helper()
	if link, _ := os.Readlink(filepath.Join(filepath.Dir(file), "bin", "codex")); link != codex {
		t.Errorf("codex link -> %q, want %q", link, codex)
	}
	onPath := filepath.Join(userBinDir(), "claude-code")
	if link, _ := os.Readlink(onPath); link != filepath.Join(filepath.Dir(file), "bin", "claude-code") {
		t.Errorf("%s -> %q, want the installed binary", onPath, link)
	}
	if _, err := os.Lstat(filepath.Join(userBinDir(), "codex")); err == nil {
		t.Error("only claude-code may be linked onto PATH")
	}
}

func retiredShimSources(t *testing.T) map[string]string {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string)
	var dirs []string
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "gemini"} {
		vendor := agentid.Binary(name)
		source := pathstest.FakeVendorOutsideConfig(t, vendor, "")
		sources[name] = source
		dirs = append(dirs, filepath.Dir(source))
		if err := os.Symlink("claude-code", filepath.Join(paths.BinDir(), name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
	return sources
}

func TestInstallBinaryAndLinksReplacesRetiredShims(t *testing.T) {
	hermetic(t)
	t.Setenv("HOME", t.TempDir())
	installTestBinary(t)
	sources := retiredShimSources(t)
	var out bytes.Buffer
	target, err := installBinaryAndLinks(&out, true)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(paths.BinDir(), "claude-code") {
		t.Fatalf("target = %q", target)
	}
	assertRetiredShimsDryRun(t, target, sources)
	out.Reset()
	if _, err := installBinaryAndLinks(&out, false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "binary" {
		t.Fatalf("installed binary = %q, %v", data, err)
	}
	assertReplacedVendorLinks(t, sources, out.String())
	assertNoRetiredShims(t, target)
}

func assertRetiredShimsDryRun(t *testing.T, target string, sources map[string]string) {
	t.Helper()
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run binary: %v, want not exist", err)
	}
	for name := range sources {
		link, err := os.Readlink(filepath.Join(paths.BinDir(), name))
		if err != nil || link != "claude-code" {
			t.Fatalf("dry-run changed %s: %q, %v", name, link, err)
		}
	}
}

func assertReplacedVendorLinks(t *testing.T, sources map[string]string, output string) {
	t.Helper()
	for name, source := range sources {
		dst := filepath.Join(paths.BinDir(), name)
		if name == "claude" {
			assertClaudeLauncher(t, dst, output)
			continue
		}
		link, err := os.Readlink(dst)
		if err != nil || link != source {
			t.Errorf("%s -> %q, %v; want %q", name, link, err, source)
		}
		if _, err := paths.AgentBin(name); err != nil {
			t.Errorf("installed vendor %s: %v", name, err)
		}
		want := "link     " + dst + " → " + source
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in output:\n%s", want, output)
		}
	}
}

func assertClaudeLauncher(t *testing.T, dst, output string) {
	t.Helper()
	body, err := os.ReadFile(dst)
	if fi, lerr := os.Lstat(dst); lerr != nil || !fi.Mode().IsRegular() || err != nil {
		t.Fatalf("bin/claude = %v, %v; want a regular launcher script", lerr, err)
	}
	if !strings.Contains(string(body), "claude --launcher") {
		t.Errorf("bin/claude is not the launcher:\n%s", body)
	}
	if !strings.Contains(output, "launcher "+dst) {
		t.Errorf("missing launcher line in output:\n%s", output)
	}
}

func assertNoRetiredShims(t *testing.T, target string) {
	t.Helper()
	entries, err := os.ReadDir(paths.BinDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		dst := filepath.Join(paths.BinDir(), entry.Name())
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(dst)
		if err != nil {
			t.Fatal(err)
		}
		if real == target || filepath.Base(real) == "claude-code" {
			t.Errorf("retired harness link remains: %s -> %s", dst, real)
		}
	}
}

// Install writes the wl-paste proxy into the harness BinDir on WSL; it must never touch the real HOME.
func TestInstallEnsuresClipboardProxy(t *testing.T) {
	hermetic(t)
	installTestBinary(t)
	file := settings.SettingsFile()
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scratchHome := t.TempDir()
	t.Setenv("HOME", scratchHome)
	proxyPath := filepath.Join(paths.BinDir(), "wl-paste")
	var out bytes.Buffer
	if err := install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	if !proc.OnWSL() {
		if _, err := os.Lstat(proxyPath); err == nil {
			t.Fatal("off-WSL: Install wrote the wl-paste proxy")
		}
		return
	}
	assertInstalledClipboardProxy(t, proxyPath, out.String())
	out.Reset()
	if err := install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("re-run output = %q, want a current line", out.String())
	}
	b2, _ := os.ReadFile(proxyPath)
	if !bytes.Equal(b2, clipboard.ProxyScript()) {
		t.Error("re-run modified an already-current proxy")
	}
}

func assertInstalledClipboardProxy(t *testing.T, proxyPath, output string) {
	t.Helper()
	b, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatalf("WSL: Install did not write the wl-paste proxy: %v", err)
	}
	if !bytes.Equal(b, clipboard.ProxyScript()) {
		t.Error("written proxy does not match the embedded script")
	}
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(output, "wl-paste") {
		t.Errorf("install output missing a wl-paste row:\n%s", output)
	}
}

func TestInstallBinaryTargetCopiesOnceAndHonorsDryRun(t *testing.T) {
	hermetic(t)
	installTestBinary(t)
	target := filepath.Join(paths.BinDir(), "claude-code")
	var out bytes.Buffer
	if got, err := installBinaryTarget(&out, true); err != nil || got != target {
		t.Fatalf("dry-run = %q, %v", got, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) || !strings.Contains(out.String(), "copy     ") {
		t.Fatalf("dry-run: stat err %v, output %q", err, out.String())
	}
	out.Reset()
	if _, err := installBinaryTarget(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "binary" {
		t.Fatalf("installed binary = %q", data)
	}
	if _, err := installBinaryTarget(&out, false); err != nil || out.Len() != 0 {
		t.Fatalf("identical binary recopied: err %v, output %q", err, out.String())
	}
}

func TestInstallBinaryRepairsIdenticalNonExecutableCopy(t *testing.T) {
	hermetic(t)
	installTestBinary(t)
	target, err := installBinaryTarget(io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installBinaryTarget(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("identical installed copy remained non-executable: %v", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "binary" {
		t.Fatalf("repaired binary = %q, %v; want original bytes", data, err)
	}
}

func TestInstallBinaryRepairsIdenticalNonExecutableHook(t *testing.T) {
	hosttest.SkipOnWindows(t, "Windows does not use POSIX execute bits")
	hermetic(t)
	installTestBinary(t)
	if _, err := installBinaryTarget(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(paths.BinDir(), paths.HookBinaryName)
	if err := os.Chmod(hook, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installBinaryTarget(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hook)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("installed hook mode = %v, %v; want executable", info, err)
	}
}

func TestInstallBuildStampRecordsInstalledMain(t *testing.T) {
	hermetic(t)
	self, mtime := installStampedTestBinary(t)
	if _, err := installBinaryTarget(io.Discard, true); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(paths.BinDir(), paths.StampFile)
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote stamp: %v", err)
	}
	target, err := installBinaryTarget(io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s %d %d\n", paths.BuildStamp, info.Size(), info.ModTime().Unix())
	if got, err := os.ReadFile(stamp); err != nil || string(got) != want {
		t.Fatalf("installed stamp = %q, %v; want %q", got, err, want)
	}
	if err := os.WriteFile(self, []byte("replacement binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(self, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if _, err := installBinaryTarget(io.Discard, false); err == nil {
		t.Fatal("install accepted stale source stamp with preserved main mtime")
	}
}

func installStampedTestBinary(t *testing.T) (string, time.Time) {
	t.Helper()
	installTestBinary(t)
	self, err := selfBinary()
	if err != nil {
		t.Fatal(err)
	}
	old := paths.BuildStamp
	paths.BuildStamp = "install-stamp"
	t.Cleanup(func() { paths.BuildStamp = old })
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(self, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(filepath.Dir(self), paths.StampFile)
	data := fmt.Appendf(nil, "%s %d %d\n", paths.BuildStamp, len("binary"), mtime.Unix())
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return self, mtime
}

// No settings.json is seeded from the render; an existing file is never written, invalid or not.
func TestInstallSettingsSeedsMissingAndNeverWritesExisting(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	if err := installSettings(&out, true); err != nil || !strings.Contains(out.String(), "would seed") {
		t.Fatalf("dry run: %v %q", err, out.String())
	}
	if _, err := os.Stat(settings.SettingsFile()); err == nil {
		t.Fatal("--dry-run created settings.json")
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(settings.SettingsFile()); !strings.Contains(string(got), ".hookd PreToolUse --safety-scan") {
		t.Fatalf("seeded settings.json has no harness hooks:\n%s", got)
	}
	for _, user := range []string{`{"model": "sonnet", "zzForeign": 1}`, `{"cleanupPeriodDays": "x"`} {
		if err := os.WriteFile(settings.SettingsFile(), []byte(user), 0o600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := installSettings(&out, false); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(settings.SettingsFile()); string(got) != user {
			t.Errorf("install rewrote an existing settings.json:\n%s", got)
		}
		if !strings.Contains(out.String(), "kept (yours)") {
			t.Errorf("status line %q, want kept (yours)", out.String())
		}
	}
}

func hermetic(t *testing.T) {
	t.Helper()
	hosttest.HermeticEnv(t) // install writes completion and the wl-paste proxy under HOME/XDG
	dir := paths.ConfigDir()
	userBin := filepath.Join(dir, "localbin")
	oldUserBin := userBinDir
	userBinDir = func() string { return userBin }
	t.Cleanup(func() { userBinDir = oldUserBin })
	oldTmux := tmuxConfPaths
	tmuxConfPaths = func() []string {
		return []string{filepath.Join(dir, ".tmux.conf"), filepath.Join(dir, "xdg", "tmux.conf")}
	}
	t.Cleanup(func() { tmuxConfPaths = oldTmux })
	oldClip := installClipAgent
	installClipAgent = func(io.Writer, string, bool) error { return nil }
	t.Cleanup(func() { installClipAgent = oldClip })
	oldDream := installDreamAgent
	installDreamAgent = func(io.Writer, string, bool) error { return nil }
	t.Cleanup(func() { installDreamAgent = oldDream })
	oldSelf := selfBinary
	selfBinary = func() (string, error) { return "/opt/bin/claude-code", nil }
	t.Cleanup(func() { selfBinary = oldSelf })
}

// TestMain runs in the hermetic host (hosttest), so no test touches the live vault or sees a vendor CLI.
func rtkFixture(t *testing.T) (pin, userBin string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(paths.RTKBinaryEnv, "")
	pin = paths.RTKPinned()
	hosttest.WriteExec(t, pin, "#!/bin/sh\necho 'rtk 1.1.0'\n")
	userBin = t.TempDir()
	orig := userBinDir
	userBinDir = func() string { return userBin }
	t.Cleanup(func() { userBinDir = orig })
	return pin, userBin
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
	hosttest.WriteExec(t, real, "#!/bin/sh\n")
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

func TestSeedTmuxConfSkipsNonENOENTProbeFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // os.UserHomeDir on unix
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	xdg := filepath.Join(home, "xdg-loop")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	hosttest.SymlinkLoop(t, xdg)
	var out bytes.Buffer
	seedTmuxConf(&out, false)
	if _, err := os.Lstat(filepath.Join(home, ".tmux.conf")); !os.IsNotExist(err) {
		t.Fatalf("tmux seeded config despite failed XDG probe: Lstat = %v", err)
	}
	if !strings.Contains(out.String(), "skip") || !strings.Contains(out.String(), xdg) {
		t.Fatalf("tmux output = %q, want skip naming failed probe", out.String())
	}
}

func TestTmuxConfPathsEmptyWithoutHomeAndSeedSkips(t *testing.T) {
	t.Setenv("HOME", "")        // os.UserHomeDir on unix
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on windows
	if got := tmuxConfPaths(); len(got) != 0 {
		t.Fatalf("tmuxConfPaths() = %v, want none when HOME is unresolved", got)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	var out bytes.Buffer
	seedTmuxConf(&out, false)
	if !strings.Contains(out.String(), "skip") {
		t.Errorf("seedTmuxConf output = %q, want a skip line", out.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, ".tmux.conf")); err == nil {
		t.Error("seedTmuxConf wrote .tmux.conf into the working directory")
	}
}

func TestRenderDreamPlist(t *testing.T) {
	got, err := renderDreamPlist("/b/cc&", "/cfg", "/log")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/b/cc&amp;", "<string>schedule</string>", "<integer>21600</integer>", "<key>RunAtLoad</key><false/>"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestPrepDreamLogTruncatesOversize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "dream-launchd.log")
	if err := prepDreamLog(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, dreamLogMax+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepDreamLog(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Size() != 0 {
		t.Fatalf("size %d, want 0", fi.Size())
	}
}

func TestExtrasCompactInstallsDreamAgent(t *testing.T) {
	called := false
	old := installDreamAgent
	installDreamAgent = func(w io.Writer, _ string, _ bool) error { called = true; fmt.Fprintln(w, "ok"); return nil }
	t.Cleanup(func() { installDreamAgent = old })
	oldClip := installClipAgent
	installClipAgent = func(io.Writer, string, bool) error { return nil }
	t.Cleanup(func() { installClipAgent = oldClip })
	if got := clipboardExtras(io.Discard); !called || !slices.Contains(got, "dream agent ok") {
		t.Fatalf("called=%v tokens=%v", called, got)
	}
}

// TestInstallContinuesAfterSettingsFailure
func TestInstallContinuesAfterSettingsFailure(t *testing.T) {
	hermetic(t)
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	if err := os.WriteFile(filepath.Join(filepath.Dir(self), paths.HookBinaryName), []byte("hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := settings.SettingsFile()
	if err := os.MkdirAll(file, 0o755); err != nil { // settings.json as a directory: os.ReadFile fails, not ErrNotExist
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := install(&out, false, true)
	if err == nil {
		t.Fatal("Install with an unreadable settings.json returned nil error")
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("Install err = %v, want it to name settings", err)
	}
	if !strings.Contains(out.String(), "settings skip") {
		t.Errorf("output missing the settings-skip report:\n%s", &out)
	}
	if !strings.Contains(out.String(), "agents   sync") {
		t.Errorf("syncVendors did not run after the settings failure:\n%s", &out)
	}
	if !strings.Contains(out.String(), "tmux") {
		t.Errorf("installGitHooks/earlier steps did not run either:\n%s", &out)
	}
}

// compactStepPrefix classifies one line of installCompact's default output by step
func compactStepPrefix(t *testing.T, line string) string {
	t.Helper()
	switch {
	case strings.HasPrefix(line, "settings (windows)"):
		return "settings (windows)"
	case strings.HasPrefix(line, "install"):
		return "install"
	case strings.HasPrefix(line, "rtk-config"):
		return "rtk-config"
	case strings.HasPrefix(line, "mdlint"):
		return "mdlint"
	case strings.HasPrefix(line, "links"):
		return "links"
	case strings.HasPrefix(line, "settings"):
		return "settings"
	case strings.HasPrefix(line, "agents"):
		return "agents"
	case strings.HasPrefix(line, "extras"):
		return "extras"
	default:
		t.Fatalf("unrecognized line in default install output: %q", line)
		return ""
	}
}

// shortTempDir is a t.TempDir() without the test name, so paths stay representative for an 80-column check.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/private/tmp" // macOS $TMPDIR is /var/folders/…/T, itself too long for the 80-column check
	}
	dir, err := os.MkdirTemp(base, "ciq")
	if err != nil && base != "" {
		dir, err = os.MkdirTemp("", "ciq")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// setupCompactFixture wires a hermetic install where every linked name resolves on PATH, under a short config path.
func setupCompactFixture(t *testing.T, linked []string) {
	t.Helper()
	hermetic(t)
	root := shortTempDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	// hermetic() already redirected userBinDir/tmuxConfPaths, but at its own (long, test-name-based) root
	oldUserBin := userBinDir
	userBinDir = func() string { return filepath.Join(root, "localbin") }
	t.Cleanup(func() { userBinDir = oldUserBin })
	oldTmux := tmuxConfPaths
	tmuxConfPaths = func() []string {
		return []string{filepath.Join(root, ".tmux.conf"), filepath.Join(root, "xdg", "tmux.conf")}
	}
	t.Cleanup(func() { tmuxConfPaths = oldTmux })
	self := filepath.Join(shortTempDir(t), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	if err := os.WriteFile(filepath.Join(filepath.Dir(self), paths.HookBinaryName), []byte("hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, name := range linked {
		vendor := agentid.Binary(name)
		source := pathstest.FakeVendorOutsideConfig(t, vendor, "")
		dirs = append(dirs, filepath.Dir(source))
		if name == agentid.Pi { // Pi links only its own state install, never a PATH copy
			own := filepath.Join(paths.StateDir(), "pi", "npm", "node_modules", ".bin", "pi")
			if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(own, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Only git rides in from the real PATH (installGitHooks/history need it)
	if p, err := exec.LookPath("git"); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	// userBinDir on PATH too, same as a real shell profile — otherwise linkOnPath's "not on PATH"
	dirs = append(dirs, filepath.Join(root, "localbin"))
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
	noWindowsProfile(t)
}

// noWindowsProfile makes the host read as non-WSL with no Windows profile, so no "settings (windows)" step runs.
func noWindowsProfile(t *testing.T) {
	t.Helper()
	orig := onWSL
	onWSL = func() bool { return false } // never shell out to cmd.exe; a test opts into the windows line via the env
	t.Cleanup(func() { onWSL = orig })
	t.Setenv(windowsProfileEnv, "")
}

// TestInstallCompactDefaultOutputHasExactlyTheStepLines
func TestInstallCompactDefaultOutputHasExactlyTheStepLines(t *testing.T) {
	for _, windowsProfile := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows profile %v", windowsProfile), func(t *testing.T) {
			setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini", "pi"})
			if windowsProfile { // a resolved Windows profile (WSL) adds the settings (windows) step
				t.Setenv(windowsProfileEnv, shortTempDir(t))
			}
			var out bytes.Buffer
			if err := install(&out, false, false); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, l := range splitNonEmpty(out.String()) {
				got = append(got, compactStepPrefix(t, l))
			}
			want := []string{"install", "mdlint", "links", "settings"}
			if runtime.GOOS != "windows" {
				want = []string{"install", "rtk-config", "mdlint", "links", "settings"}
			}
			if windowsProfile {
				want = append(want, "settings (windows)")
			}
			want = append(want, "agents", "extras")
			if !slices.Equal(got, want) {
				t.Errorf("default install steps = %v, want %v\nfull output:\n%s", got, want, &out)
			}
		})
	}
}

// TestInstallCompactDefaultOutputLinesFitEightyColumns
func TestInstallCompactDefaultOutputLinesFitEightyColumns(t *testing.T) {
	setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini", "pi"})
	var out bytes.Buffer
	if err := install(&out, false, false); err != nil {
		t.Fatal(err)
	}
	for _, l := range splitNonEmpty(out.String()) {
		if len(l) > 80 {
			t.Errorf("default output line over 80 chars (%d): %q", len(l), l)
		}
	}
}

// TestInstallCompactDefaultModePrintsFailingVendorAndSettingsSkipInFull
func TestInstallCompactDefaultModePrintsFailingVendorAndSettingsSkipInFull(t *testing.T) {
	setupCompactFixture(t, []string{"claude", "codex", "opencode", "gemini"}) // copilot left unlinked
	file := settings.SettingsFile()
	if err := os.MkdirAll(file, 0o755); err != nil { // settings.json as a directory: forces its read to fail
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := install(&out, false, false)
	if err == nil {
		t.Fatal("Install with an unreadable settings.json returned nil error")
	}
	if !strings.Contains(out.String(), "settings skip") {
		t.Errorf("default output missing the settings-skip line in full:\n%s", &out)
	}
	if !strings.Contains(out.String(), "copilot not installed") {
		t.Errorf("default output missing the unlinked vendor's full line:\n%s", &out)
	}
}

// historySnapshotHash normalizes the pre-install snapshot's commit hash, which differs between identical fixtures.
var historySnapshotHash = regexp.MustCompile(`history  [0-9a-f]+ \(pre-install snapshot\)`)

// runVerboseFixture sets up one hermetic, vendor-free install (so every vendor reports "missing"
func runVerboseFixture(t *testing.T, fn func(io.Writer) error) (output, root, self string) {
	t.Helper()
	hermetic(t)
	root = os.Getenv("CLAUDE_CONFIG_DIR")
	self = filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	if err := os.WriteFile(filepath.Join(filepath.Dir(self), paths.HookBinaryName), []byte("hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // an empty dir: every vendor resolves to "missing", deterministically
	noWindowsProfile(t)           // windows settings off in both fixtures alike
	var out bytes.Buffer
	if err := fn(&out); err != nil {
		t.Fatal(err)
	}
	return out.String(), root, self
}

func normalizeVerboseOutput(out, root, self string) string {
	out = historySnapshotHash.ReplaceAllString(out, "history  <hash> (pre-install snapshot)")
	out = strings.ReplaceAll(out, self, "$SELF")
	out = strings.ReplaceAll(out, filepath.Join(filepath.Dir(self), paths.HookBinaryName), "$HOOKS")
	out = strings.ReplaceAll(out, root, "$ROOT")
	return out
}

// TestInstallVerboseFlagMatchesInstallVerboseDirectly
func TestInstallVerboseFlagMatchesInstallVerboseDirectly(t *testing.T) {
	outA, rootA, selfA := runVerboseFixture(t, func(w io.Writer) error { return install(w, false, true) })
	outB, rootB, selfB := runVerboseFixture(t, func(w io.Writer) error { return installVerbose(w, false) })
	normA := normalizeVerboseOutput(outA, rootA, selfA)
	normB := normalizeVerboseOutput(outB, rootB, selfB)
	if normA != normB {
		t.Errorf("Install(verbose=true) diverges from installVerbose called directly:\nA:\n%s\nB:\n%s", normA, normB)
	}
}

// The "still ran" settings error must print only after the steps it claims ran, in both output modes.
func TestInstallSettingsSkipPrintsAfterTheStepsItClaimsRan(t *testing.T) {
	for _, verbose := range []bool{true, false} {
		name := "compact"
		if verbose {
			name = "verbose"
		}
		t.Run(name, func(t *testing.T) {
			assertSettingsSkipOrder(t, verbose)
		})
	}
}

func assertSettingsSkipOrder(t *testing.T, verbose bool) {
	t.Helper()
	setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini", "pi"})
	if err := os.MkdirAll(settings.SettingsFile(), 0o755); err != nil { // settings.json as a directory: read fails
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := install(&out, false, verbose); err == nil {
		t.Fatal("Install with an unreadable settings.json returned nil error")
	}
	s := out.String()
	skip := strings.Index(s, "still ran")
	agents := strings.Index(s, "agents")
	if skip < 0 || agents < 0 {
		t.Fatalf("missing skip or agents line:\n%s", s)
	}
	if skip < agents {
		t.Errorf("settings skip printed before the agents step:\n%s", s)
	}
	if verbose {
		if g := strings.Index(s, "tmux"); g < 0 || skip < g {
			t.Errorf("settings skip printed before the git-hooks/extras steps:\n%s", s)
		}
	}
}

// Install must re-render agent configs so a stale harness hook in codex's hooks.json is purged.
func TestInstallRendersAgentConfigs(t *testing.T) {
	hermetic(t)
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	hooksFile := filepath.Join(home, ".codex", "hooks.json")
	stale := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command",` +
		`"command":"HARNESS_HOOK_VENDOR=codex $HOME/.claude/bin/claude-code hook safety-scan"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksFile, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	got := agentConfigsSummary(&out)
	if !strings.Contains(got, "configs rendered:") || !strings.Contains(got, "codex") {
		t.Fatalf("summary = %q (warnings: %s)", got, out.String())
	}
	b, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "HARNESS_HOOK_VENDOR=codex $HOME") {
		t.Errorf("stale entry survived:\n%s", b)
	}
	if again := agentConfigsSummary(&out); again != "configs unchanged" {
		t.Errorf("second run = %q, want unchanged", again)
	}
}
