package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/clipboard"
)

func TestInstallMergesIntoConfigDir(t *testing.T) {
	hermetic(t, "")
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := SettingsFile()
	orig := `{"spinnerTipsEnabled": false, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "orca stop"}]}]}}`
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(&out, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Fatal("--dry-run wrote settings.json")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(file), "bin", "claude")); err == nil {
		t.Fatal("--dry-run created shims")
	}
	if err := Install(&out, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	for _, want := range []string{`"spinnerTipsEnabled": false`, `"orca stop"`, "hook stop-format", "hook bash-guard"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("merged settings missing %s:\n%s", want, b)
		}
	}
	if link, _ := os.Readlink(filepath.Join(filepath.Dir(file), "bin", "codex")); link != "claude-code" {
		t.Errorf("codex shim -> %q", link)
	}
	onPath := filepath.Join(userBinDir(), "claude-code")
	if link, _ := os.Readlink(onPath); link != filepath.Join(filepath.Dir(file), "bin", "claude-code") {
		t.Errorf("%s -> %q, want the installed binary", onPath, link)
	}
	if _, err := os.Lstat(filepath.Join(userBinDir(), "codex")); err == nil {
		t.Error("only claude-code may be linked onto PATH")
	}
	if b, _ := os.ReadFile(tmuxConfPaths()[0]); !bytes.Equal(b, tmuxConf) {
		t.Errorf("tmux.conf not seeded: %q", b)
	}
	baks, _ := filepath.Glob(file + ".bak-*")
	if len(baks) != 1 {
		t.Errorf("backups %v", baks)
	}
	if has, err := settingsHasHooks(file); !has || err != nil {
		t.Errorf("doctor hook check: %v %v", has, err)
	}
}

// Install's wl-paste proxy row follows the real box's WSL-ness (procVersionPath lives in the
// clipboard package and isn't overridable from here); either way it must never touch the real HOME.
func TestInstallEnsuresClipboardProxy(t *testing.T) {
	hermetic(t, "")
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := SettingsFile()
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scratchHome := t.TempDir()
	t.Setenv("HOME", scratchHome)
	proxyPath := filepath.Join(scratchHome, ".local", "bin", "wl-paste")

	var out bytes.Buffer
	if err := Install(&out, false); err != nil {
		t.Fatal(err)
	}

	if !clipboard.OnWSLKernel() {
		if _, err := os.Lstat(proxyPath); err == nil {
			t.Fatal("off-WSL: Install wrote the wl-paste proxy")
		}
		return
	}
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
	if !strings.Contains(out.String(), "wl-paste") {
		t.Errorf("install output missing a wl-paste row:\n%s", out.String())
	}

	// Re-run: proxy is now current, reported as such, and left byte-identical.
	out.Reset()
	if err := Install(&out, false); err != nil {
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

// A nested dir that holds only Claude Code's own worktrees is fine; other Keep reasons still ask for a move.
func TestNestedDetailWorktrees(t *testing.T) {
	only := clean.Nested{Dir: "/x/.claude", Files: []string{"worktrees"}, Keep: []string{clean.KeepWorktrees}}
	if !only.OnlyWorktrees() || !strings.Contains(nestedDetail(only), "holds only linked worktrees") {
		t.Fatalf("only-worktrees nested dir: %q", nestedDetail(only))
	}
	mixed := clean.Nested{Dir: "/x/.claude", Files: []string{"worktrees", "settings.json"}, Keep: []string{"settings.json:model", clean.KeepWorktrees}}
	if d := nestedDetail(mixed); mixed.OnlyWorktrees() || !strings.Contains(d, "move settings.json:model into") || strings.Contains(d, "worktrees") {
		t.Fatalf("mixed nested dir: %q", d)
	}
}
