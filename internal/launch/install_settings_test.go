package launch

import (
	"bytes"
	"encoding/json"
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

// No settings.json restores the render whole; an existing file keeps its foreign keys through the merge.
func TestInstallSettingsFreshRestoresThenMergesExisting(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	want, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := installSettings(&bytes.Buffer{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings.SettingsFile()); err == nil {
		t.Fatal("--dry-run created settings.json")
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(settings.SettingsFile()); string(got) != string(want) {
		t.Fatalf("fresh install != render:\n%s", got)
	}
	merged := strings.Replace(string(want), "{\n", "{\n  \"zzForeign\": 1,\n", 1)
	if err := os.WriteFile(settings.SettingsFile(), []byte(merged), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(settings.SettingsFile()); !strings.Contains(string(got), "zzForeign") {
		t.Errorf("install over an existing file dropped a foreign key:\n%s", got)
	}
}

const applySettingsUserFixture = `{
  "env": {
    "MY_VAR": "1"
  },
  "permissions": {
    "allow": [
      "Bash(mytool *)"
    ]
  },
  "theme": "light",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/bin/my-foreign"
          }
        ]
      }
    ]
  }
}
`

// Existing env, permissions and every other key stay user-owned; hooks refresh while preserving foreign hooks.
func TestApplySettingsExistingFileOnlyRefreshesHooks(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	user := applySettingsUserFixture
	if err := os.WriteFile(settings.SettingsFile(), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, merged, sum, _, err := settings.ApplySettings(false, agent.NoticeOut)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(user), &want)
	assertUserSettingsKeys(t, got, want)
	if !strings.Contains(string(got["hooks"]), "my-foreign") || sum.HarnessHooks == 0 || sum.ForeignHooks != 1 {
		t.Errorf("hooks not refreshed with the foreign one kept: %s %+v", got["hooks"], sum)
	}
}

func assertUserSettingsKeys(t *testing.T, got, want map[string]json.RawMessage) {
	t.Helper()
	for _, k := range []string{"env", "permissions", "theme"} {
		if !bytes.Equal(got[k], want[k]) {
			t.Errorf("%s = %s, want the user's %s", k, got[k], want[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("keys = %d, want the user's %d (no template key added)", len(got), len(want))
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
