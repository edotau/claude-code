// Package paths locates the harness config dir and provides atomic, locked file writes.
package paths

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// ConfigDir is CLAUDE_CONFIG_DIR, else ~/.claude — the harness repo is the config dir.
func ConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// EnvDir holds env.d/*.env files: pins (provider.env) and secrets (secrets.env), all gitignored.
func EnvDir() string { return filepath.Join(ConfigDir(), "env.d") }

// BinDir is the install target for the claude-code binary and its bare-name shims.
func BinDir() string { return filepath.Join(ConfigDir(), "bin") }

// StateDir holds runtime state (router pid/port/secret) that must never be committed.
func StateDir() string { return filepath.Join(ConfigDir(), "state", "harness") }

// ClaudeJSON is Claude Code's ~/.claude.json (MCP servers, projects); it moves under CLAUDE_CONFIG_DIR when set.
func ClaudeJSON() string {
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" {
		return filepath.Join(ConfigDir(), ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// WriteIfChanged atomically writes data unless the file already holds exactly it; reports whether it wrote.
func WriteIfChanged(dest string, data []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(dest); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	return true, AtomicWrite(dest, data, mode)
}

// AtomicWrite writes via temp file + rename so readers never observe a partial file.
func AtomicWrite(dest string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error { f.Close(); os.Remove(tmp); return err }
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// WithFileLock runs fn holding an exclusive advisory lock on path+".lock".
func WithFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lock(f); err != nil {
		return err
	}
	defer unlock(f)
	return fn()
}
