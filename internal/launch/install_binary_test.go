package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
)

func installTestBinary(t *testing.T) {
	t.Helper()
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
}

func TestInstallMergesIntoConfigDir(t *testing.T) {
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
	assertInstalledSettings(t, file)
	assertInstalledLinks(t, file, codex)
}

func assertInstalledSettings(t *testing.T, file string) {
	t.Helper()
	b, _ := os.ReadFile(file)
	for _, want := range []string{`"spinnerTipsEnabled": false`, `"foreign stop"`, "hook Stop", "hook PreToolUse --safety-scan"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("merged settings missing %s:\n%s", want, b)
		}
	}
	if b, _ := os.ReadFile(tmuxConfPaths()[0]); !bytes.Equal(b, tmuxConf) {
		t.Errorf("tmux.conf not seeded: %q", b)
	}
	bakSample := paths.BackupPath(file, time.Now())
	baks, _ := filepath.Glob(filepath.Join(filepath.Dir(bakSample), filepath.Base(file)+".bak-*"))
	if len(baks) != 1 {
		t.Errorf("backups %v", baks)
	}
	if has, err := settingsHasHooks(file); !has || err != nil {
		t.Errorf("doctor hook check: %v %v", has, err)
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
		vendor := paths.Vendors[name]
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
