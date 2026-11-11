package vscode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// extensionDirName is the installed-extension folder name VS Code expects: <publisher>.<name>-<version>.
const extensionDirName = "edotau.claude-code-agents-0.1.0"

// extensionResult reports whether linkExtension changed the symlink, and where it points.
type extensionResult struct {
	Changed bool
	Target  string
}

// defaultExtensionsDir returns ~/.vscode-server/extensions when a vscode-server install exists
// (the common case on a Remote-SSH Linux box), else the desktop client's ~/.vscode/extensions.
func defaultExtensionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(filepath.Join(home, ".vscode-server")); statErr == nil && info.IsDir() {
		return filepath.Join(home, ".vscode-server", "extensions"), nil
	}
	return filepath.Join(home, ".vscode", "extensions"), nil
}

// linkExtension symlinks <extensionsDir>/edotau.claude-code-agents-0.1.0 to the tracked extension
// source under claudeHome, so a no-build-step edit to the source takes effect immediately. Idempotent:
// a target already linked to the right source is left untouched. check=true (used by both --check
// and --dry-run) reports what would change without touching the filesystem.
func linkExtension(claudeHome, extensionsDir string, check bool) (extensionResult, error) {
	source := filepath.Join(claudeHome, "vscode", "claude-code-agents")
	target := filepath.Join(extensionsDir, extensionDirName)

	// A link to a deleted source is not in sync: VS Code cannot load the extension through it.
	if _, err := os.Stat(source); err != nil {
		return extensionResult{}, fmt.Errorf("extension source: %w", err)
	}
	if current, err := os.Readlink(target); err == nil && current == source {
		if !check {
			if err := bumpManifestScan(source); err != nil {
				return extensionResult{}, err
			}
		}
		return extensionResult{Changed: false, Target: target}, nil
	}
	if check {
		return extensionResult{Changed: true, Target: target}, nil
	}
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		return extensionResult{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		if err := os.RemoveAll(target); err != nil {
			return extensionResult{}, err
		}
	}
	if err := symlinkOrSame(source, target); err != nil {
		return extensionResult{}, err
	}
	if err := bumpManifestScan(source); err != nil {
		return extensionResult{}, err
	}
	return extensionResult{Changed: true, Target: target}, nil
}

// symlinkOrSame creates dst -> src; a concurrent run that already linked it to src is not an error.
func symlinkOrSame(src, dst string) error {
	err := os.Symlink(src, dst)
	if err != nil && errors.Is(err, fs.ErrExist) {
		if cur, rerr := os.Readlink(dst); rerr == nil && cur == src {
			return nil
		}
	}
	return err
}

// chtimes is os.Chtimes; tests swap it to inject a filesystem failure.
var chtimes = os.Chtimes

// bumpManifestScan touches the extension dir: VS Code caches scanned manifests by the directory's mtime, which an
// in-place package.json edit through the symlink never changes, so titles and commands would stay stale.
func bumpManifestScan(dir string) error {
	now := time.Now()
	return chtimes(dir, now, now)
}
