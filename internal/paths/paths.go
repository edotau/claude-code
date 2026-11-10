// Package paths locates the harness config dir and provides atomic, locked file writes.
package paths

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// ConfigDir is CLAUDE_CONFIG_DIR, else the git clone holding the running binary in its bin/, else ~/.claude.
func ConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d
	}
	if r := installRoot(); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// installRoot is the clone whose bin/ holds the running binary; "" for non-clones, linked worktrees and ~/.claude.
var installRoot = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ""
	}
	bin := filepath.Dir(exe)
	if filepath.Base(bin) != "bin" {
		return ""
	}
	root := filepath.Dir(bin)
	if !IsDir(filepath.Join(root, ".git")) || !IsDir(filepath.Join(root, "cmd", "claude-code")) {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		if def, err := filepath.EvalSymlinks(filepath.Join(home, ".claude")); err == nil && def == root {
			return ""
		}
	}
	return root
})

// ConfigDirIsInstall reports whether the config directory is the clone the running binary was installed from.
func ConfigDirIsInstall() bool {
	r := installRoot()
	return r != "" && filepath.Clean(ConfigDir()) == r
}

// ExportConfigDir sets CLAUDE_CONFIG_DIR to the install clone when unset, so Claude Code and children share it.
func ExportConfigDir() {
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" {
		return
	}
	if r := installRoot(); r != "" {
		_ = os.Setenv("CLAUDE_CONFIG_DIR", r)
	}
}

// BinDir is the install target for the claude-code binary and its bare-name shims.
func BinDir() string { return filepath.Join(ConfigDir(), "bin") }

// CacheDir holds backups, shadow history and vendor disk caches; it is the harness state dir.
func CacheDir() string { return StateDir() }

// BackupPath maps src to its mirrored backup location under CacheDir, tagged with now; pure, no I/O.
func BackupPath(src string, now time.Time) string {
	var rel string
	if home, err := os.UserHomeDir(); err == nil {
		if r, err := filepath.Rel(home, src); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		} else {
			rel = strings.TrimPrefix(src, string(filepath.Separator))
		}
	} else {
		rel = strings.TrimPrefix(src, string(filepath.Separator))
	}
	ts := now.Format("20060102-150405.000000000")
	return filepath.Join(CacheDir(), "backups", rel+".bak-"+ts)
}

// BackupDir is the per-run directory for internal/setup's backup bundles.
func BackupDir(label string, now time.Time) string {
	return filepath.Join(CacheDir(), "backups", "setup", label+"-"+now.Format("20060102-150405"))
}

// StateDir holds runtime state (router pid/port/secret) that must never be committed.
func StateDir() string { return filepath.Join(ConfigDir(), "state", "harness") }

// DispatchEventsSuffix is the file suffix of a dispatch's NDJSON event log under StateDir()/dispatch.
const DispatchEventsSuffix = ".events.jsonl"

// ClaudeJSON is Claude Code's ~/.claude.json (MCP servers, projects); it moves under CLAUDE_CONFIG_DIR when set.
func ClaudeJSON() string {
	home, err := os.UserHomeDir()
	// A CLAUDE_CONFIG_DIR equal to the default ~/.claude still means the real ~/.claude.json.
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" && !ConfigDirIsDefault() {
		return filepath.Join(ConfigDir(), ".claude.json")
	}
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
	fail := func(err error) error { return errors.Join(err, f.Close(), os.Remove(tmp)) }
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

// WithFileLock runs fn holding an exclusive advisory lock on path+".lock".
func WithFileLock(path string, fn func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := acquire(f, path); err != nil {
		return err
	}
	defer func() {
		if uerr := unlock(f); uerr != nil {
			err = errors.Join(err, uerr)
		}
	}()
	return fn()
}

// AppendLine appends line (plus a trailing newline when missing) to file under its lock, creating the 0700 parent dir.
func AppendLine(file string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line = append(slices.Clip(line), '\n')
	}
	return WithFileLock(file, func() error {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(line)
		return errors.Join(err, f.Close())
	})
}

// ErrLockTimeout means WithFileLock gave up waiting for a held lock.
var ErrLockTimeout = errors.New("timed out waiting for file lock")

// lockTimeout bounds how long WithFileLock waits for a peer to release the lock.
var lockTimeout = 2 * time.Minute

// TryLock takes the exclusive lock on f without blocking; false means another holder has it (or locking is unsupported).
func TryLock(f *os.File) bool { ok, _ := tryLock(f); return ok }

// acquire polls tryLock until granted, lockTimeout passes (ErrLockTimeout) or locking itself fails.
func acquire(f *os.File, path string) error {
	for deadline := time.Now().Add(lockTimeout); ; time.Sleep(lockPoll()) {
		ok, err := tryLock(f)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s.lock", ErrLockTimeout, path)
		}
	}
}

// lockPoll is a jittered 1-50ms wait: a fixed period keeps contenders in lockstep, one winner per period.
func lockPoll() time.Duration { return time.Duration(1+rand.IntN(50)) * time.Millisecond }

// TryFileLock runs fn under an exclusive lock on path only if granted within wait; ran=false means fn did not run.
// For work that must not run unserialized (counting a shared marker dir); a locking error is returned, not swallowed.
func TryFileLock(path string, wait time.Duration, fn func() error) (ran bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for deadline := time.Now().Add(wait); ; time.Sleep(lockPoll()) {
		ok, lerr := tryLock(f)
		if lerr != nil {
			return false, lerr
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return false, nil
		}
	}
	defer func() {
		if uerr := unlock(f); uerr != nil {
			err = errors.Join(err, uerr)
		}
	}()
	return true, fn()
}

// IsFile reports whether p exists and is a regular (non-dir) entry.
func IsFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }

// IsDir reports whether p exists and is a directory.
func IsDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// FileExists reports whether p exists (any type).
func FileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// SafeName reports whether name is a single safe path component — an unsafe name (`../../escape`,
// `..`, a dot-file, a backslash on Linux) would let a joined path escape the root it is checked under.
func SafeName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		name == filepath.Base(name) &&
		!strings.ContainsAny(name, `/\`) &&
		!strings.HasPrefix(name, ".")
}

// DirSize sums a file's size, or every file under a directory. Best-effort: an unreadable entry counts 0.
func DirSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if !st.IsDir() {
		return st.Size()
	}
	var total int64
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree contributes 0, never aborts the walk
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// writeBackup writes data to base, or base-1, base-2... if taken, never overwriting; returns the path used.
func writeBackup(base string, data []byte) (string, error) {
	for i := 0; ; i++ {
		dest := base
		if i > 0 {
			dest = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if err = errors.Join(err, f.Close()); err != nil {
			return "", errors.Join(err, os.Remove(dest))
		}
		return dest, nil
	}
}

// Backup writes data to the cache-mirrored backup path for src (dir made 0700), never overwriting; returns the path.
func Backup(src string, data []byte) (string, error) {
	base := BackupPath(src, time.Now())
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", fmt.Errorf("backup dir %s: %w", filepath.Dir(base), err)
	}
	return writeBackup(base, data)
}

// vscodeGOOS picks VSCodeSettings' platform layout; a seam so tests run every branch on any host.
var vscodeGOOS = runtime.GOOS

// VSCodeSettings is the platform's VS Code Machine settings path; outside a vscode-server it is the User scope.
func VSCodeSettings() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch vscodeGOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json"), nil
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "settings.json"), nil
	default:
		if info, statErr := os.Stat(filepath.Join(home, ".vscode-server")); statErr == nil && info.IsDir() {
			return filepath.Join(home, ".vscode-server", "data", "Machine", "settings.json"), nil
		}
		return filepath.Join(home, ".config", "Code", "User", "settings.json"), nil
	}
}

// SelfBinary is the resolved absolute path of the running harness binary.
func SelfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Abs(exe)
}
