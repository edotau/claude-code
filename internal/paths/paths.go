// Package paths locates the harness config dir and provides atomic, locked file writes.
package paths

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet" // test binaries refuse real DNS: tests mock their API calls
	"github.com/edotau/claude-code/internal/proc"
)

// testConfigDir is one private temp dir per test process, so an unset CLAUDE_CONFIG_DIR stays off live data.
var testConfigDir = sync.OnceValue(func() string {
	if d, err := os.MkdirTemp("", "claude-config-test-"); err == nil {
		return d
	}
	return filepath.Join(os.TempDir(), "claude-config-test")
})

// ConfigDir is CLAUDE_CONFIG_DIR, else the git clone holding the running binary in its bin/, else ~/.claude.
func ConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d
	}
	if testing.Testing() { // a test binary never falls back to the live ~/.claude
		return testConfigDir()
	}
	if r := installRoot(); r != "" {
		return r
	}
	if home := homeDir(); home != "" {
		return filepath.Join(home, ".claude")
	}
	return filepath.Join(noHomeRoot(), ".claude")
}

// noHomeRoot is the homeless-process root: our own 0700 uid-named temp dir, else an unguessable one.
var noHomeRoot = sync.OnceValue(func() string {
	root := filepath.Join(os.TempDir(), "claude-code-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(root, 0o700); err == nil || os.IsExist(err) {
		if fi, err := os.Lstat(root); err == nil && fi.IsDir() && fi.Mode().Perm()&0o077 == 0 {
			if uid, ok := OwnerUID(fi); ok && uid == os.Getuid() {
				return root
			}
		}
	}
	return filepath.Join(os.TempDir(), "claude-code-"+cryptorand.Text()) // unguessable, so nothing can be pre-planted
})

// homeDir is $HOME, else the passwd entry's home (launchd, cron, minimal containers leave HOME unset); "" if neither.
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
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
	if home := homeDir(); home != "" {
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
		_ = proc.Setenv("CLAUDE_CONFIG_DIR", r)
	}
}

// CAEnvNames are the CA bundle variables a harness launch exports and `network pki` writes.
var CAEnvNames = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

const caBundleName = "corp-ca-bundle.pem"

// CertsDir holds every certificate the harness writes or fetches.
func CertsDir() string { return filepath.Join(ConfigDir(), "state", "certs") }

// CABundle is the corporate CA bundle `network pki` writes beside root.pem and issuing.pem.
func CABundle() string { return filepath.Join(CertsDir(), caBundleName) }

// ExportCA points each unset CA variable at CABundle when it exists; one the user set wins.
func ExportCA() {
	bundle := CABundle()
	if !IsFile(bundle) && (!ConfigDirIsInstall() || !migrateCerts(legacyCertsDir(), CertsDir())) {
		return
	}
	for _, name := range CAEnvNames {
		if os.Getenv(name) == "" {
			_ = proc.Setenv(name, bundle)
		}
	}
}

// legacyCertsDir is pki's pre-state default: $XDG_DATA_HOME or ~/.local/share, then claude-code/certs.
func legacyCertsDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home := homeDir()
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "claude-code", "certs")
}

// migrateCerts copies the bundle, root.pem and issuing.pem from old once; old is never removed.
func migrateCerts(old, dir string) bool {
	if old == "" || !IsFile(filepath.Join(old, caBundleName)) {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	for _, name := range []string{"root.pem", "issuing.pem", caBundleName} { // bundle last: its presence marks done
		data, err := os.ReadFile(filepath.Join(old, name))
		if err != nil {
			if name == caBundleName {
				return false
			}
			continue
		}
		if AtomicWrite(filepath.Join(dir, name), data, 0o644) != nil {
			return false
		}
	}
	return true
}

// BinDir is the install target for the claude-code binary and its bare-name shims.
func BinDir() string { return filepath.Join(ConfigDir(), "bin") }

// BackupPath maps src to its mirrored backup location under StateDir, tagged with now; it writes nothing.
func BackupPath(src string, now time.Time) string {
	sep := string(filepath.Separator)
	rel := strings.TrimPrefix(filepath.Join(sep, src), sep) // a rooted Join drops leading ".."
	if home, err := os.UserHomeDir(); err == nil {
		if r, err := filepath.Rel(home, src); err == nil && r != ".." && !strings.HasPrefix(r, ".."+sep) {
			rel = r
		}
	}
	ts := now.Format("20060102-150405.000000000")
	return filepath.Join(StateDir(), "backups", rel+".bak-"+ts)
}

// BackupDir is the per-run directory for internal/setup's backup bundles.
func BackupDir(label string, now time.Time) string {
	return filepath.Join(StateDir(), "backups", "setup", label+"-"+now.Format("20060102-150405"))
}

// StateDir holds runtime state (router pid/port/secret) that must never be committed.
func StateDir() string { return filepath.Join(ConfigDir(), "state", "harness") }

// PackDir holds the context packs a run stages for agents that read them from a file, never inside a repo.
func PackDir() string { return filepath.Join(StateDir(), "packs") }

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
	return atomicWrite(dest, data, mode, true)
}

// AtomicWriteNoSync is AtomicWrite without the fsync, for derived caches that rebuild when a crash tears them.
func AtomicWriteNoSync(dest string, data []byte, mode os.FileMode) error {
	return atomicWrite(dest, data, mode, false)
}

func atomicWrite(dest string, data []byte, mode os.FileMode, syncData bool) error {
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
	if syncData {
		if err := f.Sync(); err != nil {
			return fail(err)
		}
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if syncData {
		return syncDir(filepath.Dir(dest))
	}
	return nil
}

// syncDir makes a completed rename durable; a seam so tests observe and inject it.
var syncDir = dirSync

// SyncDir fsyncs dir so the names created or removed in it survive a crash; a no-op where a directory cannot be synced.
func SyncDir(dir string) error { return syncDir(dir) }

// NoReplaceWrite and NoReplaceLink are the write and link steps of WriteNoReplace; tests swap them to inject failures.
var (
	NoReplaceWrite = func(f *os.File, data []byte) error { _, err := f.Write(data); return err }
	NoReplaceLink  = os.Link
)

// WriteNoReplace writes data to a new dest, never replacing an existing or racing file; false when it exists.
func WriteNoReplace(dest string, data []byte, mode os.FileMode) (created bool, err error) {
	tmp, err := StageFile(dest, data, mode)
	if err != nil {
		return false, err
	}
	return PlaceNoReplace(tmp, dest, data, mode)
}

// StageFile writes data to a synced temp file beside dest and returns its path.
func StageFile(dest string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return "", err
	}
	if err := errors.Join(NoReplaceWrite(f, data), f.Chmod(mode), f.Sync(), f.Close()); err != nil {
		return "", errors.Join(err, os.Remove(f.Name()))
	}
	return f.Name(), nil
}

// PlaceNoReplace links staged tmp to dest, else creates it exclusively, then removes tmp; false when dest exists.
func PlaceNoReplace(tmp, dest string, data []byte, mode os.FileMode) (bool, error) {
	lerr := NoReplaceLink(tmp, dest) // link(2) on Unix, CreateHardLinkW on Windows: neither replaces
	if lerr != nil && !errors.Is(lerr, fs.ErrExist) {
		lerr = createExclusive(dest, data, mode) // no hard links here (FAT, FUSE, network mounts)
	}
	rerr := os.Remove(tmp)
	if errors.Is(lerr, fs.ErrExist) {
		return false, rerr
	}
	return lerr == nil, errors.Join(lerr, rerr)
}

func createExclusive(dest string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := errors.Join(NoReplaceWrite(f, data), f.Sync(), f.Close()); err != nil {
		return errors.Join(err, os.Remove(dest)) // a part-written file is neither the old text nor the new
	}
	return nil
}

// WriteJSON writes v as JSON to path atomically with mode 0600, creating its parent dir 0700.
func WriteJSON(path string, v any) error { return writeJSON(path, v, AtomicWrite) }

// WriteJSONNoSync is WriteJSON without the fsync, for derived caches that rebuild when a crash tears them.
func WriteJSONNoSync(path string, v any) error { return writeJSON(path, v, AtomicWriteNoSync) }

func writeJSON(path string, v any, write func(string, []byte, os.FileMode) error) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return write(path, data, 0o600)
}

// Stale reports whether a cache stamped at is ttl or older at now, or stamped more than skew in the future.
func Stale(at time.Time, ttl, skew time.Duration, now time.Time) bool {
	age := now.Sub(at)
	return age >= ttl || age < -skew
}

// WithFileLock runs fn holding an exclusive advisory lock on path+".lock", waiting up to lockTimeout for it.
func WithFileLock(path string, fn func() error) error {
	return WithFileLockWithin(path, lockTimeout, fn)
}

// WithFileLockWithin is WithFileLock with a caller-chosen wait, for callers (hooks) that must not block long.
func WithFileLockWithin(path string, wait time.Duration, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	ran, err := TryFileLock(path+".lock", wait, fn)
	if !ran && err == nil {
		return fmt.Errorf("%w: %s.lock", ErrLockTimeout, path)
	}
	return err
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

// AppendLineRepair is AppendLine without the lock; it starts line on its own row when a torn write left no newline.
func AppendLineRepair(file string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line = append(slices.Clip(line), '\n')
	}
	if endsMidLine(f) {
		line = append([]byte{'\n'}, line...)
	}
	_, err = f.Write(line)
	return errors.Join(err, f.Close())
}

// endsMidLine reports whether the file is non-empty and its last byte is not a newline.
func endsMidLine(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return false
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return false
	}
	return last[0] != '\n'
}

// ErrLockTimeout means WithFileLock gave up waiting for a held lock.
var ErrLockTimeout = errors.New("timed out waiting for file lock")

var lockTimeout = 2 * time.Minute

// TryLock takes the exclusive lock on f without blocking; false means another holder has it or locking is unsupported.
func TryLock(f *os.File) bool { ok, _ := tryLock(f); return ok }

// lockPoll is a jittered doubling wait from 100us to 50ms: holders are short, and jitter keeps contenders apart.
var lockPoll = func(attempt int) time.Duration {
	d := 50 * time.Millisecond
	if attempt < 9 {
		d = 100 * time.Microsecond << attempt
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// TryFileLock runs fn under an exclusive lock on path only if granted within wait; ran=false means fn did not run.
func TryFileLock(path string, wait time.Duration, fn func() error) (ran bool, err error) {
	return tryFileLock(path, wait, fn, func() (*os.File, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	})
}

// maxLockReopens bounds how often TryFileLock reopens the path after the lock it took names a removed or replaced file.
const maxLockReopens = 8

// errLockReplaced means the lock path kept naming a different file than the one this process locked.
var errLockReplaced = errors.New("file lock path keeps being replaced")

// tryFileLock is TryFileLock with the open step injected, so a test can replace the path between open and acquire.
func tryFileLock(path string, wait time.Duration, fn func() error, open func() (*os.File, error)) (bool, error) {
	deadline := time.Now().Add(wait)
	for range maxLockReopens {
		f, err := open()
		if err != nil {
			return false, err
		}
		ok, err := acquireUntil(f, deadline)
		if err != nil || !ok {
			return false, joinIfAny(err, f.Close())
		}
		same, err := lockedPath(path, f)
		if err != nil {
			return false, joinIfAny(err, release(f))
		}
		if same {
			return runLocked(f, fn)
		}
		if err := release(f); err != nil {
			return false, err
		}
	}
	return false, errLockReplaced
}

// acquireUntil polls tryLock on f until it is granted or deadline passes; false with a nil error means it timed out.
func acquireUntil(f *os.File, deadline time.Time) (bool, error) {
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			time.Sleep(lockPoll(attempt - 1))
		}
		ok, err := tryLock(f)
		if err != nil || ok {
			return ok, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
	}
}

// lockedPath reports whether path still names the file f holds; a missing path means a holder unlinked it.
func lockedPath(path string, f *os.File) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	named, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(held, named), nil
}

// release unlocks f and closes it, returning both failures.
func release(f *os.File) error { return errors.Join(unlock(f), f.Close()) }

// joinIfAny is err unchanged when extra is nil, so callers still compare fn's sentinel errors by identity.
func joinIfAny(err, extra error) error {
	if extra == nil {
		return err
	}
	return errors.Join(err, extra)
}

// runLocked runs fn while f holds the lock, then releases it; a release failure joins fn's error.
func runLocked(f *os.File, fn func() error) (ran bool, err error) {
	defer func() { err = joinIfAny(err, release(f)) }()
	return true, fn()
}

// IsFile reports whether p exists and is a regular (non-dir) entry.
func IsFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }

// IsDir reports whether p exists and is a directory.
func IsDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// FileExists reports whether p exists (any type).
func FileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// MoveLegacy returns cur, first renaming old to it when only old exists; old stays in use if it cannot move.
func MoveLegacy(old, cur string) string {
	if FileExists(cur) {
		return cur
	}
	if fi, err := os.Lstat(old); err == nil && fi.IsDir() {
		_ = os.Rename(old, cur) // a directory rename never replaces a non-empty one
	} else if os.Link(old, cur) == nil { // a link never replaces an existing file
		_ = os.Remove(old)
	}
	if !FileExists(cur) && FileExists(old) {
		return old
	}
	return cur
}

// SafeName reports whether name is one safe path component; an unsafe one could escape the root it is joined under.
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

// ReadLive is os.ReadFile for WriteIfUnchanged; tests swap it to land a foreign save between the two checks.
var ReadLive = os.ReadFile

// checkUnchanged errors when dest's current bytes no longer match old (a missing file reads as empty).
func checkUnchanged(dest string, old []byte, label string) error {
	fresh, err := ReadLive(dest)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !bytes.Equal(fresh, old) {
		return fmt.Errorf("live %s changed under the merge (%s) — re-run to merge the newer file", label, dest)
	}
	return nil
}

// WriteIfUnchanged backs dest up and replaces it only while old is still on disk; a written backup's path is returned.
func WriteIfUnchanged(dest string, old, next []byte, mode os.FileMode, label string) (string, error) {
	if err := checkUnchanged(dest, old, label); err != nil {
		return "", err
	}
	backup := ""
	if len(old) > 0 {
		var err error
		if backup, err = Backup(dest, old); err != nil {
			return "", err
		}
	}
	if err := checkUnchanged(dest, old, label); err != nil {
		return backup, err
	}
	return backup, AtomicWrite(dest, next, mode)
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

// DispatchDir is the directory holding dispatch records and job logs.
func DispatchDir() string { return filepath.Join(StateDir(), "dispatch") }

// EnsureDispatchDir creates the 0700 dispatch dir and returns its path.
func EnsureDispatchDir() (string, error) {
	dir := DispatchDir()
	return dir, os.MkdirAll(dir, 0o700)
}

// SwapSymlink points link at target via a temp link and rename, so link is never absent or half-made.
func SwapSymlink(target, link string) error {
	tmp := fmt.Sprintf("%s.tmp%d-%s", link, os.Getpid(), cryptorand.Text())
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

// ResolveLink follows path through symlinks so a write lands on the real file
func ResolveLink(path string, followDangling bool) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	fi, lerr := os.Lstat(path)
	if !followDangling {
		if lerr == nil {
			return "", fmt.Errorf("%s is a symlink to a missing file: create its target or remove the link", path)
		}
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		t, rerr := os.Readlink(path)
		if rerr != nil {
			return "", rerr
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(path), t)
		}
		return ResolveLink(t, true)
	}
	return path, nil
}

// ShortHome replaces a leading home directory with ~, leaving sibling paths unchanged.
func ShortHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// ExpandHome expands only ~ and a leading ~/ using the current home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// ConfigDirIsDefault reports whether the config directory is the current home's .claude directory.
func ConfigDirIsDefault() bool {
	home := homeDir()
	return home != "" && filepath.Clean(ConfigDir()) == filepath.Join(home, ".claude")
}

// Within reports whether path equals base or sits under it, after cleaning both (so /a/foo is not inside /a/foobar).
func Within(path, base string) bool {
	path, base = filepath.Clean(path), filepath.Clean(base)
	if path == base {
		return true
	}
	if !strings.HasSuffix(base, string(filepath.Separator)) {
		base += string(filepath.Separator) // the root "/" already ends in one
	}
	return strings.HasPrefix(path, base)
}

// ExecutableRealPath is the absolute symlink-resolved path of candidate if it is an executable file, else "".
func ExecutableRealPath(candidate string) string {
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return ""
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return ""
	}
	return real
}

// AnchorPath anchors a relative path to cwd; an empty path or cwd leaves it unchanged.
func AnchorPath(path, cwd string) string {
	if path != "" && !filepath.IsAbs(path) && cwd != "" {
		return filepath.Join(cwd, path)
	}
	return path
}

// RelWithin is abs relative to root; false when abs escapes root.
func RelWithin(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// SessionIDEnv is the env var Claude Code sets in every live session's tools.
const SessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// protectedDBs are the state databases Protected and ProtectedDBs cover.
func protectedDBs() []string {
	state := filepath.Join(ConfigDir(), "state")
	return []string{filepath.Join(state, "vault.db"), filepath.Join(state, "memory.db"), AuditDBPath()}
}

// ProtectedDBs lists each state database Protected covers, then the state/backups/*.db snapshots that exist.
func ProtectedDBs() []string {
	snaps, _ := filepath.Glob(filepath.Join(ConfigDir(), "state", "backups", "*.db"))
	return append(protectedDBs(), snaps...)
}

// Protected reports whether p is or holds a state database, one of its sidecars or a state/backups/*.db snapshot.
func Protected(p string) bool {
	resolve := func(p string) string { return FoldCase(Real(p)) } // each side resolved once: Real is a syscall walk
	backups := filepath.Join(ConfigDir(), "state", "backups")
	rp, rdir, rbackups, base := resolve(p), resolve(filepath.Dir(p)), resolve(backups), FoldCase(filepath.Base(p))
	if rdir == rbackups && strings.HasSuffix(base, ".db") {
		return true
	}
	if rp == rbackups {
		if snaps, _ := filepath.Glob(filepath.Join(backups, "*.db")); len(snaps) > 0 {
			return true
		}
	}
	for _, path := range protectedDBs() {
		if Within(resolve(path), rp) {
			return true
		}
		db := filepath.Base(path)
		if rdir == resolve(filepath.Dir(path)) && (strings.HasPrefix(base, db+".") || slices.Contains([]string{"-wal", "-shm", "-journal"}, strings.TrimPrefix(base, db))) {
			return true
		}
	}
	return false
}

// AuditDBPath is the post-land audit queue's store (internal/worktree/auditq); Protected keeps clean away from it.
func AuditDBPath() string { return filepath.Join(StateDir(), "worktree", "audit", "audit.db") }

// MemoryStoreDir is the memory store root and the work tree of the local memory repo.
func MemoryStoreDir() string { return filepath.Join(ConfigDir(), "memory") }

// MemoryStateDir holds update stamps, results, logs and the session chain.
func MemoryStateDir() string { return filepath.Join(StateDir(), "memory") }

// MemorySubDir is the store root: a named sub is a filename prefix, not a directory.
func MemorySubDir(sub string) string { return filepath.Join(MemoryStoreDir(), sub) }

// MemorySub is name inside the store folder sub (archive, handoffs, reports, …); sub "" is a top-level file.
func MemorySub(sub, name string) string {
	return filepath.Join(MemoryStoreDir(), sub, filepath.FromSlash(name))
}
