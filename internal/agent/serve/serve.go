// Package serve is `agents serve`: a local daemon that admits detached `ask --job` children per provider pool.
package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/schemas"
)

// ErrNotRunning means no daemon answers on the socket.
var ErrNotRunning = errors.New("agents serve is not running")

// ErrUnsupported: Windows has no 0600 Unix socket guard here, so submit always runs directly.
var ErrUnsupported = errors.New("agents serve: not served on windows; submit runs directly")

// ErrTestBinary refuses to spawn the daemon from a test binary.
var ErrTestBinary = errors.New("agents serve: refusing to spawn the daemon from a test binary")

const (
	maxLogBytes   = 10 << 20
	maxStateBytes = 16 << 10
	stateSchema   = 1
	sockName      = "agents.sock"
)

// Dir is the daemon's 0700 directory under the harness state dir: socket, state, log and spawn lock.
func Dir() string { return filepath.Join(paths.StateDir(), "serve") }

// SockPath is the daemon's control socket.
func SockPath() string { return filepath.Join(Dir(), sockName) }

func stateFile() string { return filepath.Join(Dir(), "serve.json") }

func logFile() string { return filepath.Join(Dir(), "serve.log") }

// State is serve.json: written by a serving daemon, removed when it exits.
type State struct {
	Schema  int       `json:"schema"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Exe     string    `json:"exe,omitempty"` // sha256 of the daemon's executable
	Cap     int       `json:"cap,omitempty"` // ceiling on every pool's cap; 0 = account limits
}

var stateFormat = schemas.Register(schemas.Format{
	Name: "serve-runtime", Owner: "internal/agent/serve", Kind: schemas.Record, Version: stateSchema,
	Glob: func() string { return stateFile() },
	New:  func() any { return new(State) },
	Keep: "runtime state; removed when the daemon exits",
})

// ReadState reads serve.json.
func ReadState() (State, error) {
	var st State
	f, err := openServeFile("serve.json", os.O_RDONLY)
	if err != nil {
		return st, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return st, err
	}
	if len(b) > maxStateBytes {
		return st, errors.New("agents serve: state exceeds size limit")
	}
	_, err = schemas.Decode(stateFormat, b, &st)
	return st, err
}

// writeState publishes st and returns its cleanup, which removes the file only while it is still st's.
func writeState(st State) (func(), error) {
	st.Schema = stateSchema
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if err := withServeLock("serve.json.lock", func() error { return paths.AtomicWrite(stateFile(), data, 0o600) }); err != nil {
		return nil, err
	}
	return func() {
		_ = withServeLock("serve.json.lock", func() error {
			if cur, err := ReadState(); err == nil && cur.PID == st.PID && cur.Started.Equal(st.Started) {
				_ = os.Remove(stateFile())
			}
			return nil
		})
	}, nil
}

// Running verifies the socket peer, or the daemon named in its held lifetime lock when the socket is unavailable.
func Running() (State, bool) {
	st, readErr := ReadState()
	got, err := QueryStatus(time.Second)
	if err != nil || got.PID <= 0 || !proc.Alive(got.PID) {
		return lockedDaemon(st, readErr)
	}
	if readErr != nil || st.PID != got.PID || !st.Started.Equal(got.Started) {
		st = State{Schema: stateSchema, PID: got.PID, Started: got.Started, Cap: got.Cap}
	}
	return st, true
}

// lockedDaemon is the lock's owner, with serve.json's exe hash and cap when that file names the same daemon.
func lockedDaemon(st State, readErr error) (State, bool) {
	owner, ok := lockOwner()
	if ok && readErr == nil && st.PID == owner.PID && st.Started.Equal(owner.Started) {
		return st, true
	}
	return owner, ok
}

// Start runs the daemon detached unless one already runs on this binary; one on an older build is restarted.
func Start(ceiling int) (State, error) {
	var out State
	err := withSpawnLock(func() error {
		if st, ok := Running(); ok {
			if st.Exe == "" || st.Exe == proxy.ExeHash() {
				out = st
				return nil
			}
			if err := stop(st); err != nil { // a rebuilt binary replaces the daemon
				return err
			}
		}
		var err error
		out, err = spawnAndWait(ceiling)
		return err
	})
	return out, err
}

// Stop SIGTERMs the daemon and waits for it to drain; a stale state file is removed.
func Stop() (State, error) {
	var st State
	err := withSpawnLock(func() error {
		var ok bool
		if st, ok = Running(); ok {
			return stop(st)
		}
		return removeStoppedState()
	})
	return st, err
}

// Restart drains the running daemon (queued jobs re-queue) and starts this binary's; ceiling < 0 keeps the cap.
func Restart(ceiling int) (old, cur State, err error) {
	err = withSpawnLock(func() error {
		var ok bool
		if old, ok = Running(); ok {
			if ceiling < 0 {
				ceiling = old.Cap
			}
			if err := stop(old); err != nil {
				return err
			}
		}
		var serr error
		cur, serr = spawnAndWait(max(ceiling, 0))
		return serr
	})
	return old, cur, err
}

// withSpawnLock runs fn under the spawn lock, after checking the daemon's directory is ours and 0700.
func withSpawnLock(fn func() error) error {
	if runtime.GOOS == "windows" {
		return ErrUnsupported
	}
	if err := ensureDir(Dir()); err != nil {
		return err
	}
	return withServeLock("spawn.lock", fn)
}

func withServeLock(name string, fn func() error) error {
	f, err := openServeFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer f.Close()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(50 * time.Millisecond) {
		if paths.TryLock(f) {
			return fn()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s", paths.ErrLockTimeout, name)
		}
	}
}

// daemonProcess scans argv in order, so flags before --serve or a split binary path still match.
func daemonProcess(pid int) bool {
	if pid <= 0 || proc.Comm(pid) != "claude-code" {
		return false
	}
	argv, err := proc.Argv(pid)
	if err != nil || len(argv) == 0 {
		return false
	}
	want := []string{"agents", "serve", "--serve"}
	for _, a := range argv[1:] {
		if len(want) > 0 && a == want[0] {
			want = want[1:]
		}
	}
	return len(want) == 0
}

// lockOwner reads the held daemon.lock's "<pid> <started-unix-nano>" when that pid is the serve daemon.
func lockOwner() (State, bool) {
	f, err := openServeFile("daemon.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return State{}, false
	}
	defer f.Close()
	if paths.TryLock(f) {
		return State{}, false
	}
	b, err := io.ReadAll(io.LimitReader(f, 64))
	var pid int
	var nano int64
	if err != nil || len(b) == 0 || b[len(b)-1] != '\n' {
		return State{}, false
	}
	if _, err := fmt.Sscanf(string(b), "%d %d\n", &pid, &nano); err != nil || !daemonProcess(pid) {
		return State{}, false
	}
	return State{Schema: stateSchema, PID: pid, Started: time.Unix(0, nano).UTC()}, true
}

func removeStoppedState() error {
	f, err := openServeFile("daemon.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer f.Close()
	if !paths.TryLock(f) {
		return errors.New("agents serve: daemon lock held but identity unavailable; not removing state")
	}
	return withServeLock("serve.json.lock", func() error {
		if err := os.Remove(stateFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return ErrNotRunning
	})
}

// drainWait bounds a stop's wait for the daemon to exit; a var so a test can shorten it.
var drainWait = 10 * time.Second

func stop(st State) error {
	if !daemonProcess(st.PID) {
		return fmt.Errorf("agents serve: pid %d is not the serve daemon; not signalling it", st.PID)
	}
	p, err := os.FindProcess(st.PID)
	if err == nil {
		err = p.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return fmt.Errorf("agents serve: stop pid %d: %w", st.PID, err)
	}
	for deadline := time.Now().Add(drainWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !proc.Alive(st.PID) {
			return nil
		}
	}
	return fmt.Errorf("agents serve: pid %d signalled but still draining", st.PID)
}

// spawnAndWait starts `agents serve --serve` detached and waits up to 3s for it to answer.
func spawnAndWait(ceiling int) (State, error) {
	if err := spawn(ceiling); err != nil {
		return State{}, err
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st, ok := Running(); ok {
			return st, nil
		}
	}
	return State{}, fmt.Errorf("agents serve did not answer within 3s (see %s)", logFile())
}

func spawn(ceiling int) error {
	exe, err := paths.SelfBinary()
	if err != nil {
		return err
	}
	if proc.IsSelfUnderTest(exe) {
		return ErrTestBinary
	}
	lf, err := openLog()
	if err != nil {
		return err
	}
	defer lf.Close()
	args := []string{"agents", "serve", "--serve"}
	if ceiling > 0 {
		args = append(args, "--cap", strconv.Itoa(ceiling))
	}
	if proc.SpawnDetachedPID(exe, args, Dir(), lf) <= 0 {
		return fmt.Errorf("agents serve: could not start %s", exe)
	}
	return nil
}

func openLog() (*os.File, error) {
	f, err := openServeFile("serve.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && fi.Size() > maxLogBytes {
		err = f.Truncate(0)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
