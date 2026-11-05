package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
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

func TestClaudeJSONFollowsNonDefaultConfigDirOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct{ env, want string }{
		{"", filepath.Join(home, ".claude.json")},
		{filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")}, // default dir set explicitly
		{filepath.Join(home, "wt"), filepath.Join(home, "wt", ".claude.json")},
	}
	for _, c := range cases {
		t.Setenv("CLAUDE_CONFIG_DIR", c.env)
		if got := ClaudeJSON(); got != c.want {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: ClaudeJSON() = %s, want %s", c.env, got, c.want)
		}
	}
}

func TestStatHelpers(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("  hello \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope")

	tests := []struct {
		name               string
		fn                 func(string) bool
		file, dir, missing bool
	}{
		{"IsFile", IsFile, true, false, false},
		{"IsDir", IsDir, false, true, false},
		{"FileExists", FileExists, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := [3]bool{tt.fn(file), tt.fn(dir), tt.fn(missing)}
			if want := [3]bool{tt.file, tt.dir, tt.missing}; got != want {
				t.Errorf("%s: file/dir/missing = %v, want %v", tt.name, got, want)
			}
		})
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"a": "123", "sub/b": "45678"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := DirSize(dir); got != 8 {
		t.Errorf("DirSize(dir) = %d, want 8", got)
	}
	if got := DirSize(filepath.Join(dir, "a")); got != 3 {
		t.Errorf("DirSize(file) = %d, want 3", got)
	}
	if got := DirSize(filepath.Join(dir, "nope")); got != 0 {
		t.Errorf("DirSize(missing) = %d, want 0", got)
	}
}

func TestSafeName(t *testing.T) {
	for name, ok := range map[string]bool{
		"claude":            true,
		"databricks-expert": true,
		"../../escape":      false,
		"a/b":               false,
		`a\b`:               false,
		".":                 false,
		"..":                false,
		"":                  false,
		".hidden":           false,
		"/etc/passwd":       false,
	} {
		if got := SafeName(name); got != ok {
			t.Errorf("SafeName(%q) = %v, want %v", name, got, ok)
		}
	}
}

// writeBackup numbers collisions base, base-1, base-2 and never overwrites; files are 0600.
func TestWriteBackupNumbersCollisions(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cfg.bak")
	for i, want := range []string{base, base + "-1", base + "-2"} {
		got, err := writeBackup(base, []byte{byte('a' + i)})
		if err != nil || got != want {
			t.Fatalf("call %d: got %q, %v; want %q", i, got, err, want)
		}
		b, _ := os.ReadFile(got)
		if string(b) != string([]byte{byte('a' + i)}) {
			t.Fatalf("%s content %q", got, b)
		}
		if fi, _ := os.Stat(got); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", got, fi.Mode().Perm())
		}
	}
}

// ClientSecret mints 64 hex chars once, stores them 0600, and returns the same value afterwards.
func TestClientSecretMintsOnceAndReuses(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	a, err := ClientSecret(file)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a) {
		t.Fatalf("secret %q, %v", a, err)
	}
	b, err := ClientSecret(file)
	if err != nil || a != b {
		t.Fatalf("second call %q, %v; want %q", b, err, a)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// An empty secret file is re-minted rather than returned.
func TestClientSecretReplacesEmptyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ClientSecret(file)
	if err != nil || len(s) != 64 {
		t.Fatalf("secret %q, %v", s, err)
	}
}

func TestVSCodeSettingsIsPlatformSpecific(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based home dir override does not apply on windows")
	}

	dest, err := VSCodeSettings()
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		want := filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json")
		if dest != want {
			t.Errorf("dest = %s, want %s", dest, want)
		}
	default:
		// No vscode-server dir yet: falls back to the desktop (User scope) path.
		want := filepath.Join(home, ".config", "Code", "User", "settings.json")
		if dest != want {
			t.Errorf("dest (no vscode-server) = %s, want %s", dest, want)
		}
		if err := os.MkdirAll(filepath.Join(home, ".vscode-server"), 0o755); err != nil {
			t.Fatal(err)
		}
		dest, err = VSCodeSettings()
		if err != nil {
			t.Fatal(err)
		}
		want = filepath.Join(home, ".vscode-server", "data", "Machine", "settings.json")
		if dest != want {
			t.Errorf("dest (with vscode-server) = %s, want %s", dest, want)
		}
	}
}

// TryFileLock must not run fn while another fd already holds the lock, and must run it once released.
func TestTryFileLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !TryLock(f) {
		t.Fatal("could not take the lock")
	}

	ran, err := TryFileLock(path, 100*time.Millisecond, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("TryFileLock must not run fn while the lock is held elsewhere")
	}

	unlock(f)
	f.Close()

	ran, err = TryFileLock(path, 100*time.Millisecond, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("TryFileLock must run fn once the lock is released")
	}
}

func TestCanonDeepMissingLeafThroughSymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	want := filepath.Join(Canon(real), "a", "b")
	if got := CanonDeep(filepath.Join(link, "a", "b")); got != want {
		t.Fatalf("CanonDeep through symlink with missing tail = %q, want %q", got, want)
	}
	if got := Canon(filepath.Join(link, "a")); got != filepath.Join(link, "a") {
		t.Fatalf("Canon of an unresolvable path = %q, want it cleaned only", got)
	}
}

func TestAppendLineConcurrent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sub", "log")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if err := AppendLine(file, fmt.Appendf(nil, "line-%02d", i)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d lines, want 20: %q", len(lines), data)
	}
	if err := AppendLine(file, []byte("x\n")); err != nil || !strings.HasSuffix(readStr(t, file), "\nx\n") {
		t.Fatalf("pre-terminated line got a second newline or failed: %v", err)
	}
}

func readStr(t *testing.T, f string) string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
