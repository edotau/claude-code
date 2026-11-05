package launch

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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

func TestTmuxConfPathsEmptyWithoutHomeAndSeedSkips(t *testing.T) {
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Skip("UserHomeDir reads USERPROFILE")
	}
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

func TestFormatsCheckFailsOnUnreadableStore(t *testing.T) {
	hermetic(t, "")
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault.Path(), bytes.Repeat([]byte("not a bolt db"), 400), 0o600); err != nil {
		t.Fatal(err)
	}
	c := formatsCheck()
	if c.OK || !strings.Contains(c.Detail, "unreadable") {
		t.Fatalf("formats row = %+v, want failing with 'unreadable'", c)
	}
}
