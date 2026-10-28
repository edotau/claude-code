package paths

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheDirIsDotMemoryCacheUnderConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	want := filepath.Join(dir, "state", "harness")
	if got := CacheDir(); got != want {
		t.Errorf("CacheDir() = %s, want %s", got, want)
	}
}

var backupTS = time.Date(2026, 10, 21, 15, 4, 5, 123456789, time.UTC)

func TestBackupPathNoFilesystemAccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", filepath.Join(dir, "home-unused"))
	// BackupPath is pure: it must not create CacheDir, src, or any parent, even once called.
	src := filepath.Join(dir, "home-unused", ".config", "thing.json")
	_ = BackupPath(src, backupTS)
	if _, err := os.Stat(CacheDir()); !os.IsNotExist(err) {
		t.Errorf("BackupPath touched the filesystem: CacheDir() stat err = %v", err)
	}
}

func TestBackupPathInHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	src := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	got := BackupPath(src, backupTS)
	want := filepath.Join(dir, "state", "harness", "backups", ".config", "opencode", "opencode.jsonc.bak-20261021-150405.123456789")
	if got != want {
		t.Errorf("BackupPath() = %s, want %s", got, want)
	}
}

func TestBackupPathOutsideHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	// src lives entirely outside $HOME (e.g. a Windows WSL profile under /mnt): rel falls back to the
	// absolute path with its leading separator stripped, not a ".." escape.
	outside := filepath.Join(dir, "mnt", "c", "Users", "eric", ".claude", "settings.json")
	got := BackupPath(outside, backupTS)
	want := filepath.Join(dir, "state", "harness", "backups", filepath.Clean(outside[1:])+".bak-20261021-150405.123456789")
	if got != want {
		t.Errorf("BackupPath() = %s, want %s", got, want)
	}
}

func TestBackupDirFormat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	got := BackupDir("install", backupTS)
	want := filepath.Join(dir, "state", "harness", "backups", "setup", "install-20261021-150405")
	if got != want {
		t.Errorf("BackupDir() = %s, want %s", got, want)
	}
}

func TestBackupDirNoFilesystemAccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	_ = BackupDir("install", backupTS)
	if _, err := os.Stat(CacheDir()); !os.IsNotExist(err) {
		t.Errorf("BackupDir touched the filesystem: CacheDir() stat err = %v", err)
	}
}
