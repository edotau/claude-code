// Package paths locates the harness config dir and provides atomic, locked file writes.
package paths

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" && (err != nil || ConfigDir() != filepath.Join(home, ".claude")) {
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
	if err := lock(f); err != nil {
		return err
	}
	defer func() {
		if uerr := unlock(f); uerr != nil {
			err = errors.Join(err, uerr)
		}
	}()
	return fn()
}

// lockPoll is a jittered 1-50ms wait: a fixed period keeps contenders in lockstep, one winner per period.
func lockPoll() time.Duration { return time.Duration(1+rand.IntN(50)) * time.Millisecond }

// TryFileLock runs fn under an exclusive lock on path only if granted within wait; ran=false means fn did not run.
// For work that must not run unserialized (counting a shared marker dir), unlike the fail-open WithFileLock.
func TryFileLock(path string, wait time.Duration, fn func() error) (ran bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for deadline := time.Now().Add(wait); !tryLock(f); time.Sleep(lockPoll()) {
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

// WriteBackup writes data to base, or base-1, base-2... if taken, never overwriting; returns the path used.
func WriteBackup(base string, data []byte) (string, error) {
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
	return WriteBackup(base, data)
}

// ClientSecret returns the 32-byte hex secret in file, minting it 0600 under a file lock on first use.
func ClientSecret(file string) (string, error) {
	if s, err := readSecret(file); err == nil {
		return s, nil
	}
	var out string
	err := WithFileLock(file, func() error {
		if s, err := readSecret(file); err == nil {
			out = s
			return nil
		}
		b := make([]byte, 32)
		if _, err := cryptorand.Read(b); err != nil {
			return err
		}
		out = hex.EncodeToString(b)
		return AtomicWrite(file, []byte(out+"\n"), 0o600)
	})
	return out, err
}

func readSecret(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s, nil
	}
	return "", errors.New("empty secret file " + file)
}

// VSCodeSettings is the platform's VS Code Machine settings path; outside a vscode-server it is the User scope.
func VSCodeSettings() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
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
