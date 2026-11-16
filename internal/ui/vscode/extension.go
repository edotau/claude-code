package vscode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// extensionDirName is the folder name VS Code expects (<publisher>.<name>-<version>), read from source's package.json.
func extensionDirName(source string) (string, error) {
	// A link to a deleted source is not in sync: VS Code cannot load the extension through it.
	if _, err := os.Stat(source); err != nil {
		return "", fmt.Errorf("extension source: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(source, "package.json"))
	if err != nil {
		return "", fmt.Errorf("extension manifest: %w", err)
	}
	var m struct{ Publisher, Name, Version string }
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("extension manifest: %w", err)
	}
	if m.Publisher == "" || m.Name == "" || m.Version == "" {
		return "", errors.New("extension manifest: publisher, name and version are required")
	}
	return m.Publisher + "." + m.Name + "-" + m.Version, nil
}

// extensionResult reports whether linkExtension changed the symlink, and where it points.
type extensionResult struct {
	Changed bool
	Target  string
}

// defaultExtensionsDir prefers ~/.vscode-server/extensions when present, else ~/.vscode/extensions.
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

// linkExtension symlinks the tracked extension into extensionsDir; idempotent, and check=true reports without touching.
func linkExtension(claudeHome, extensionsDir string, check bool) (extensionResult, error) {
	source := filepath.Join(claudeHome, "vscode", "claude-code-agents")

	name, err := extensionDirName(source)
	if err != nil {
		return extensionResult{}, err
	}
	target := filepath.Join(extensionsDir, name)
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
	if err := replaceLink(extensionsDir, source, target); err != nil {
		return extensionResult{}, err
	}
	if err := bumpManifestScan(source); err != nil {
		return extensionResult{}, err
	}
	return extensionResult{Changed: true, Target: target}, nil
}

// replaceLink points target at source, creating extensionsDir and dropping a stale symlink first.
func replaceLink(extensionsDir, source, target string) error {
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		return err
	}
	if err := removeStaleLink(target); err != nil {
		return err
	}
	return symlinkOrSame(source, target)
}

// removeStaleLink removes target when it is a symlink (this verb's own); a real directory or file is an error.
func removeStaleLink(target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s exists and is not a symlink; remove it by hand to link the extension", target)
	}
	return os.Remove(target)
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

// bumpManifestScan touches the extension dir: VS Code caches manifests by dir mtime, which symlink edits never change.
func bumpManifestScan(dir string) error {
	now := time.Now()
	return chtimes(dir, now, now)
}
