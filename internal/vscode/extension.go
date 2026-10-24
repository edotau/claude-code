package vscode

import (
	"os"
	"path/filepath"
	"time"
)

// extensionDirName is the installed-extension folder name VS Code expects: <publisher>.<name>-<version>.
const extensionDirName = "edotau.claude-code-agents-0.1.0"

// ExtensionResult reports whether LinkExtension changed the symlink, and where it points.
type ExtensionResult struct {
	Changed bool
	Target  string
}

// DefaultExtensionsDir returns ~/.vscode-server/extensions when a vscode-server install exists
// (the common case on a Remote-SSH Linux box), else the desktop client's ~/.vscode/extensions.
func DefaultExtensionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(filepath.Join(home, ".vscode-server")); statErr == nil && info.IsDir() {
		return filepath.Join(home, ".vscode-server", "extensions"), nil
	}
	return filepath.Join(home, ".vscode", "extensions"), nil
}

// LinkExtension symlinks <extensionsDir>/edotau.claude-code-agents-0.1.0 to the tracked extension
// source under claudeHome, so a no-build-step edit to the source takes effect immediately. Idempotent:
// a target already linked to the right source is left untouched. check=true (used by both --check
// and --dry-run) reports what would change without touching the filesystem.
func LinkExtension(claudeHome, extensionsDir string, check bool) (ExtensionResult, error) {
	source := filepath.Join(claudeHome, "vscode", "claude-code-agents")
	target := filepath.Join(extensionsDir, extensionDirName)

	if current, err := os.Readlink(target); err == nil && current == source {
		if !check {
			bumpManifestScan(source)
		}
		return ExtensionResult{Changed: false, Target: target}, nil
	}
	if check {
		return ExtensionResult{Changed: true, Target: target}, nil
	}
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		return ExtensionResult{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		if err := os.RemoveAll(target); err != nil {
			return ExtensionResult{}, err
		}
	}
	if err := os.Symlink(source, target); err != nil {
		return ExtensionResult{}, err
	}
	bumpManifestScan(source)
	return ExtensionResult{Changed: true, Target: target}, nil
}

// bumpManifestScan touches the extension dir: VS Code caches scanned manifests by the directory's mtime, which an
// in-place package.json edit through the symlink never changes, so titles and commands would stay stale.
func bumpManifestScan(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}
