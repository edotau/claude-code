package vscode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// extensionHome is a claude home with the extension source; it skips where symlinks need privileges (Windows).
func extensionHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Symlink(home, filepath.Join(home, "probe")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(filepath.Join(home, "probe")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "vscode", "claude-code-agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLinkExtensionCreatesSymlink(t *testing.T) {
	claudeHome := extensionHome(t)
	extensionsDir := filepath.Join(t.TempDir(), "extensions")

	result, err := LinkExtension(claudeHome, extensionsDir, false)
	if err != nil {
		t.Fatalf("LinkExtension: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true on first link")
	}
	target, err := os.Readlink(result.Target)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	want := filepath.Join(claudeHome, "vscode", "claude-code-agents")
	if target != want {
		t.Errorf("symlink -> %q, want %q", target, want)
	}
}

func TestLinkExtensionIsIdempotent(t *testing.T) {
	claudeHome := extensionHome(t)
	extensionsDir := filepath.Join(t.TempDir(), "extensions")

	if _, err := LinkExtension(claudeHome, extensionsDir, false); err != nil {
		t.Fatalf("first link: %v", err)
	}
	result, err := LinkExtension(claudeHome, extensionsDir, false)
	if err != nil {
		t.Fatalf("second link: %v", err)
	}
	if result.Changed {
		t.Error("a second link to the same source must report Changed=false")
	}
}

func TestLinkExtensionCheckReportsDriftWithoutWriting(t *testing.T) {
	claudeHome := extensionHome(t)
	extensionsDir := filepath.Join(t.TempDir(), "extensions")

	result, err := LinkExtension(claudeHome, extensionsDir, true)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Changed {
		t.Error("--check on a missing link must report drift")
	}
	if _, err := os.Lstat(result.Target); err == nil {
		t.Error("--check must not create the symlink")
	}
}

func TestLinkExtensionReplacesAStaleTarget(t *testing.T) {
	claudeHome := extensionHome(t)
	extensionsDir := filepath.Join(t.TempDir(), "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(extensionsDir, extensionDirName)
	if err := os.Symlink(filepath.Join(t.TempDir(), "somewhere-else"), stale); err != nil {
		t.Fatal(err)
	}

	result, err := LinkExtension(claudeHome, extensionsDir, false)
	if err != nil {
		t.Fatalf("relink: %v", err)
	}
	if !result.Changed {
		t.Error("replacing a stale target must report Changed=true")
	}
	target, err := os.Readlink(result.Target)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if want := filepath.Join(claudeHome, "vscode", "claude-code-agents"); target != want {
		t.Errorf("symlink -> %q, want %q", target, want)
	}
}

func TestDefaultExtensionsDirPrefersVSCodeServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".vscode-server"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := DefaultExtensionsDir()
	if err != nil {
		t.Fatalf("DefaultExtensionsDir: %v", err)
	}
	want := filepath.Join(home, ".vscode-server", "extensions")
	if dir != want {
		t.Errorf("got %q, want %q", dir, want)
	}
}

func TestDefaultExtensionsDirFallsBackToDesktopClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := DefaultExtensionsDir()
	if err != nil {
		t.Fatalf("DefaultExtensionsDir: %v", err)
	}
	want := filepath.Join(home, ".vscode", "extensions")
	if dir != want {
		t.Errorf("got %q, want %q", dir, want)
	}
}

func TestLinkExtensionCheckRejectsDeletedSource(t *testing.T) {
	home := extensionHome(t)
	extensions := t.TempDir()
	if _, err := LinkExtension(home, extensions, false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "vscode", "claude-code-agents")); err != nil {
		t.Fatal(err)
	}
	_, err := LinkExtension(home, extensions, true)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check of dangling matching link = %v, want missing-source error", err)
	}
}

// A Chtimes failure in bumpManifestScan reaches the caller unchanged, naming the extension source.
func TestLinkExtensionPropagatesChtimesError(t *testing.T) {
	claudeHome := extensionHome(t)
	extensionsDir := t.TempDir()
	source := filepath.Join(claudeHome, "vscode", "claude-code-agents")
	if err := os.Symlink(source, filepath.Join(extensionsDir, extensionDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	injected := errors.New("chtimes refused")
	var touched string
	orig := chtimes
	chtimes = func(name string, _, _ time.Time) error { touched = name; return injected }
	t.Cleanup(func() { chtimes = orig })

	if _, err := LinkExtension(claudeHome, extensionsDir, false); !errors.Is(err, injected) {
		t.Fatalf("LinkExtension = %v, want the chtimes error", err)
	}
	if touched != source {
		t.Fatalf("chtimes(%q), want the extension source %q", touched, source)
	}
}
