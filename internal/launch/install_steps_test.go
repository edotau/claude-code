package launch

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestSnapshotPreInstallWarnsLoudlyOnFailure: a snapshot that cannot be taken (history dir blocked by a file)
// returns "" and prints a warning that says the binary has no rollback point.
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
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
}

func TestInstallKeepsExistingSettings(t *testing.T) {
	hermetic(t, "")
	codex := vendorPATHFixture(t, "codex", "")
	t.Setenv("PATH", filepath.Dir(codex))
	installTestBinary(t)
	file := settings.SettingsFile()
	orig := `{"spinnerTipsEnabled": false, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "foreign stop"}]}]}}`
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(&out, true, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Fatal("--dry-run wrote settings.json")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(file), "bin", "claude")); err == nil {
		t.Fatal("--dry-run created vendor links")
	}
	if err := Install(&out, false, true); err != nil {
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
	if b, _ := os.ReadFile(tmuxConfPaths()[0]); !bytes.Equal(b, tmuxConf) {
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
	t.Helper()
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string)
	var dirs []string
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "gemini"} {
		vendor := agentid.Binary(name)
		source := vendorPATHFixture(t, vendor, "")
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
	hermetic(t, "")
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

// Install's wl-paste proxy row follows the real box's WSL-ness; it must never touch the real HOME.
func TestInstallEnsuresClipboardProxy(t *testing.T) {
	hermetic(t, "")
	installTestBinary(t)
	file := settings.SettingsFile()
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scratchHome := t.TempDir()
	t.Setenv("HOME", scratchHome)
	proxyPath := filepath.Join(scratchHome, ".local", "bin", "wl-paste")
	var out bytes.Buffer
	if err := Install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	if !clipboard.OnWSLKernel() {
		if _, err := os.Lstat(proxyPath); err == nil {
			t.Fatal("off-WSL: Install wrote the wl-paste proxy")
		}
		return
	}
	assertInstalledClipboardProxy(t, proxyPath, out.String())
	out.Reset()
	if err := Install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("re-run output = %q, want a current line", out.String())
	}
	b2, _ := os.ReadFile(proxyPath)
	if !bytes.Equal(b2, clipboard.ProxyScript) {
		t.Error("re-run modified an already-current proxy")
	}
}

func assertInstalledClipboardProxy(t *testing.T, proxyPath, output string) {
	t.Helper()
	b, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatalf("WSL: Install did not write the wl-paste proxy: %v", err)
	}
	if !bytes.Equal(b, clipboard.ProxyScript) {
		t.Error("written proxy does not match the embedded script")
	}
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(output, "wl-paste") {
		t.Errorf("install output missing a wl-paste row:\n%s", output)
	}
}

func TestInstallBinaryTargetCopiesOnceAndHonorsDryRun(t *testing.T) {
	hermetic(t, "")
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
	hermetic(t, "")
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
	if got, _ := os.ReadFile(settings.SettingsFile()); !strings.Contains(string(got), "hook PreToolUse --safety-scan") {
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

// hermetic isolates config, pins and seams; userProviders is an optional vault overlay/providers document.
func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// Install writes completion and the wl-paste proxy under HOME/XDG; keep them out of the real home.
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	for _, k := range []string{providers.EnvSessionProfile, "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "ANTHROPIC_API_KEY", "TEST_GW_KEY"} {
		t.Setenv(k, "")
	}
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
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
	oldSelf := selfBinary
	selfBinary = func() (string, error) { return "/opt/bin/claude-code", nil }
	t.Cleanup(func() { selfBinary = oldSelf })
}

func vendorPATHFixture(t *testing.T, name, body string) string {
	t.Helper()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := pathstest.FakeVendor(t, name, body)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if name == "agy" {
		dst := filepath.Join(filepath.Dir(bin), "agy")
		if err := os.Rename(bin, dst); err != nil {
			t.Fatal(err)
		}
		bin = dst
	}
	return bin
}

// TestMain runs in the hermetic host (hosttest), so no test touches the live vault or sees a vendor CLI.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	detectBrowsers = func() browser.Report { return browser.Report{} }
	os.Exit(hosttest.Main(m))
}

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
