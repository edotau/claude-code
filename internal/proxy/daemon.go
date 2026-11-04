package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	DefaultPort = 18765
	maxLogBytes = 10 << 20
)

// ShutdownDrain bounds a SIGTERM's wait on in-flight streams; a var so a test can shorten it.
var ShutdownDrain = 30 * time.Second

// ErrNotRunning means no healthy proxy answers for proxy.json.
var ErrNotRunning = errors.New("proxy is not running")

var ErrKeptHolder = errors.New("proxy: keeping running holder with live work")

// ErrForeignHolder: the port is held by something that is not a replaceable proxy of this config dir.
var ErrForeignHolder = errors.New("proxy: holder is not this config dir's proxy")

var terminateHolder = terminate

// State is proxy.json: written by a serving daemon, removed when it exits.
type State struct {
	Schema  int       `json:"schema"`
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	Started time.Time `json:"started"`
}

func stateFile() string { return filepath.Join(paths.StateDir(), "proxy.json") }

// legacyStateFile is the pre-rename state file a daemon from an older build still writes; read until 2026-11-14.
func legacyStateFile() string { return filepath.Join(paths.StateDir(), "router.json") }

func secretFile() string { return filepath.Join(paths.StateDir(), "router.secret") }

// LogFile receives a spawned daemon's stdio.
func LogFile() string { return filepath.Join(paths.StateDir(), "proxy.log") }

// Base is the proxy URL for port.
func Base(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// SessionBase is the ANTHROPIC_BASE_URL for a session whose default provider is provider.
func SessionBase(base, provider string) string { return base + "/p/" + provider }

// ClientHeader carries the proxy secret when Authorization holds the client's own login (subscription OAuth).
const ClientHeader = "X-Claude-Code-Proxy"

// LegacyClientHeader is the pre-rename secret header live sessions launched by an older build still send.
const LegacyClientHeader = "X-Claude-Code-Router"

// ReadState reads proxy.json, else the legacy router.json.
func ReadState() (State, error) {
	var st State
	b, err := os.ReadFile(stateFile())
	if errors.Is(err, os.ErrNotExist) {
		b, err = os.ReadFile(legacyStateFile())
	}
	if err != nil {
		return st, err
	}
	_, err = schemas.Decode(routerFormat, b, &st)
	if errors.Is(err, schemas.ErrNewer) {
		// PID and Port are stable across versions: a newer daemon stays visible and stoppable; Schema marks it newer.
		if json.Unmarshal(b, &st) == nil && st.PID != 0 {
			return st, nil
		}
	}
	return st, err
}

// Running returns the state of a live, healthy router.
func Running(ctx context.Context) (State, bool) {
	st, err := ReadState()
	if err != nil || !alive(st.PID) {
		return st, false
	}
	return st, Health(ctx, st.Port) == nil
}

// Health GETs /healthz on port.
func Health(ctx context.Context, port int) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base(port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// Ensure starts the router daemon if it is not already healthy and returns its base URL (http://127.0.0.1:<port>).
func Ensure(ctx context.Context) (string, error) {
	Release() // any start, by hand or by a launch, ends a deliberate --stop
	if st, ok := Running(ctx); ok {
		return Base(st.Port), nil
	}
	if _, err := ClientSecret(); err != nil {
		return "", err
	}
	var base string
	err := paths.WithFileLock(filepath.Join(paths.StateDir(), "router.spawn"), func() error {
		if st, ok := Running(ctx); ok {
			base = Base(st.Port)
			return nil
		}
		if err := spawn(); err != nil {
			return err
		}
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if st, ok := Running(ctx); ok {
				base = Base(st.Port)
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
		return fmt.Errorf("proxy did not become healthy within 3s (see %s)", LogFile())
	})
	return base, err
}

// ErrTestBinary: under `go test`, os.Executable is the test binary, which would re-run its own tests as a "router".
var ErrTestBinary = errors.New("proxy: refusing to spawn the daemon from a test binary (stub the router seam)")

// daemonCommand builds the detached daemon's command line.
func daemonCommand(exe string) *exec.Cmd {
	return exec.Command(exe, "gateway", "proxy", "--serve") //nolint:noctx // The detached daemon must outlive its caller.
}

func spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if proc.IsSelfUnderTest(exe) {
		return ErrTestBinary
	}
	// Resolve shims: argv[0] `claude` would dispatch to the launcher instead of `gateway proxy --serve`.
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(LogFile()); err == nil && fi.Size() > maxLogBytes {
		flags |= os.O_TRUNC
	}
	lf, err := os.OpenFile(LogFile(), flags, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := daemonCommand(exe)
	cmd.Dir = paths.StateDir()
	cmd.Stdout, cmd.Stderr = lf, lf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// ownedElsewhere reports a live router on another port already holding router.json.
func ownedElsewhere(st State) bool {
	cur, err := ReadState()
	if err == nil && cur.Schema > stateSchema { // never overwrite a live newer build's record, even on the same port
		return cur.PID != 0 && alive(cur.PID)
	}
	return err == nil && cur.PID != 0 && cur.PID != st.PID && cur.Port != st.Port && alive(cur.PID)
}

// Stop SIGTERMs a healthy proxy and waits briefly for it to exit; a stale state file is removed.
func Stop(ctx context.Context) (State, error) {
	st, ok := Running(ctx)
	if !ok {
		if st.PID != 0 && !alive(st.PID) {
			_ = os.Remove(stateFile())
			_ = os.Remove(legacyStateFile())
		}
		return st, ErrNotRunning
	}
	if err := terminate(st.PID); err != nil {
		return st, err
	}
	// The wait follows the daemon's drain: a busy router is still stopping, not failing to stop.
	for deadline := time.Now().Add(ShutdownDrain + 5*time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !alive(st.PID) {
			return st, nil
		}
	}
	return st, fmt.Errorf("pid %d signalled but still draining", st.PID)
}

// awaitPortFree waits up to 5s for 127.0.0.1:port to stop accepting connections (the daemon closes it on SIGTERM).
func awaitPortFree(ctx context.Context, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		d := net.Dialer{Timeout: 200 * time.Millisecond}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil
		}
		_ = c.Close()
	}
	return fmt.Errorf("proxy still accepting on :%d after SIGTERM", port)
}

// Restart replaces a running router with one spawned from the current binary, leaving a gap of only the respawn time;
// a stopped router is left alone.
func Restart(ctx context.Context) (old, cur State, restarted bool, err error) {
	old, ok := Running(ctx)
	if !ok {
		return old, State{}, false, nil
	}
	if err = terminate(old.PID); err != nil {
		return old, State{}, false, err
	}
	if err = awaitPortFree(ctx, old.Port); err != nil {
		return old, State{}, false, err
	}
	// The old daemon keeps draining in the background: its listener is closed, so the replacement binds at once.
	if _, err = Ensure(ctx); err != nil {
		return old, State{}, false, err
	}
	cur, ok = Running(ctx)
	if !ok {
		return old, State{}, false, fmt.Errorf("proxy did not come back healthy (see %s)", LogFile())
	}
	return old, cur, true, nil
}

// PIDHeader is the /healthz response header carrying the proxy's pid.
const PIDHeader = "X-Claude-Code-Proxy-Pid"

// Health headers identify the build and work that would be interrupted by a takeover.
const (
	HeaderBuild    = "X-Claude-Code-Proxy-Build"
	HeaderSessions = "X-Claude-Code-Proxy-Sessions"
	HeaderInflight = "X-Claude-Code-Proxy-Inflight"
	HeaderConfig   = "X-Claude-Code-Proxy-Config"
)

// ConfigID names this process's config dir without exposing the path.
func ConfigID() string {
	sum := sha256.Sum256([]byte(paths.ConfigDir()))
	return hex.EncodeToString(sum[:8])
}

// HolderState is what a claude-code proxy's /healthz headers report.
type HolderState struct {
	PID      int
	Build    string
	Sessions int
	Inflight int64
	Config   string
}

// Holder identifies who answers on port: router reports a claude-code /healthz, pid its PIDHeader (0 = older build).
func Holder(ctx context.Context, port int) (HolderState, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base(port)+"/healthz", nil)
	if err != nil {
		return HolderState{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return HolderState{}, false
	}
	_ = resp.Body.Close()
	var st HolderState
	st.PID, _ = strconv.Atoi(resp.Header.Get(PIDHeader))
	st.Build = resp.Header.Get(HeaderBuild)
	st.Config = resp.Header.Get(HeaderConfig)
	st.Sessions, _ = strconv.Atoi(resp.Header.Get(HeaderSessions))
	st.Inflight, _ = strconv.ParseInt(resp.Header.Get(HeaderInflight), 10, 64)
	return st, resp.StatusCode == http.StatusOK
}

// Inflight counts the requests in flight on port; 0 when unreadable, so a wedged proxy can still be restarted.
func Inflight(ctx context.Context, port int) int64 {
	st, _ := Holder(ctx, port)
	return st.Inflight
}

// Reclaim stops an older claude-code proxy holding port; anything else is an error naming the holder.
func Reclaim(ctx context.Context, port int, force bool) error {
	st, router := Holder(ctx, port)
	pid := st.PID
	if err := checkReclaimable(st, router, port, force); err != nil {
		return err
	}
	if err := terminateHolder(pid); err != nil {
		return fmt.Errorf("proxy: stop older proxy pid %d on :%d: %w", pid, port, err)
	}
	return awaitHolderExit(pid, port)
}

// checkReclaimable returns nil when the holder st on port is an older proxy of this config that may be stopped.
func checkReclaimable(st HolderState, router bool, port int, force bool) error {
	pid := st.PID
	switch {
	case !router:
		return fmt.Errorf("%w (127.0.0.1:%d is held by another process); free it or pass --port", ErrForeignHolder, port)
	case pid <= 0 || pid == os.Getpid():
		return fmt.Errorf("proxy: 127.0.0.1:%d is held by an older claude-code proxy that reports no pid; "+
			"find it with `ss -ltnp 'sport = :%d'` and kill it", port, port)
	}
	if st.Config != "" && st.Config != ConfigID() { // another config dir's proxy is never ours to replace
		return fmt.Errorf("%w (127.0.0.1:%d answers as a proxy of another config dir or host, left alone); pass --port",
			ErrForeignHolder, port)
	}
	if !force && st.Build != textutil.BuildVersion() && (st.Inflight > 0 || st.Sessions > 0) {
		fmt.Fprintf(os.Stderr, "proxy: keeping running proxy pid %d (build %s) with live work; this build %s not taking over\n",
			pid, st.Build, textutil.BuildVersion())
		return ErrKeptHolder
	}
	return nil
}

// awaitHolderExit waits for pid to exit after it was told to stop.
func awaitHolderExit(pid, port int) error {
	for deadline := time.Now().Add(ShutdownDrain + 5*time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !alive(pid) {
			fmt.Fprintf(os.Stderr, "proxy: replaced older proxy pid %d on :%d\n", pid, port)
			return nil
		}
	}
	return fmt.Errorf("proxy: older proxy pid %d on :%d is still draining", pid, port)
}

// stateSchema versions router-runtime.
const stateSchema = 1

var routerFormat = schemas.Register(schemas.Format{
	Name: "router-runtime", Owner: "internal/proxy", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "runtime state; removed when the daemon exits",
	Migrate: schemas.SameShape,
})

// WriteState publishes this daemon's state and returns its conditional cleanup.
func WriteState(st State) (func(), error) {
	st.Schema = stateSchema
	if ownedElsewhere(st) {
		return func() {}, nil
	}
	data, _ := json.Marshal(st)
	if err := paths.AtomicWrite(stateFile(), data, 0o600); err != nil {
		return nil, err
	}
	_ = os.Remove(legacyStateFile()) // this daemon now owns proxy.json
	return func() {
		if cur, err := ReadState(); err == nil && cur.PID == st.PID && cur.Port == st.Port {
			_ = os.Remove(stateFile())
		}
	}, nil
}

// Optional launcher-set request headers that attribute a ledger entry; absent today, so those fields stay empty.
const (
	AgentHeader   = "X-Claude-Code-Agent"
	SessionHeader = "X-Claude-Code-Session"
)
