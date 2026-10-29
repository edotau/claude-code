package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	DefaultPort = 18765
	maxLogBytes = 10 << 20
)

// shutdownDrain bounds a SIGTERM's wait on in-flight streams; a var so a test can shorten it.
var shutdownDrain = 30 * time.Second

// ErrNotRunning means no healthy router answers for router.json.
var ErrNotRunning = errors.New("proxy is not running")

// State is router.json: written by a serving daemon, removed when it exits.
type State struct {
	Schema  int       `json:"schema"`
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	Started time.Time `json:"started"`
}

func stateFile() string  { return filepath.Join(paths.StateDir(), "router.json") }
func secretFile() string { return filepath.Join(paths.StateDir(), "router.secret") }

// LogFile receives a spawned daemon's stdio.
func LogFile() string { return filepath.Join(paths.StateDir(), "router.log") }

// Base is the router URL for port.
func Base(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// SessionBase is the ANTHROPIC_BASE_URL for a session whose default provider is provider.
func SessionBase(base, provider string) string { return base + "/p/" + provider }

// ClientHeader carries the router secret when Authorization holds the client's own login (subscription OAuth).
const ClientHeader = "X-Claude-Code-Router"

// ReadState reads router.json.
func ReadState() (State, error) {
	var st State
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return st, err
	}
	_, err = schemas.Decode(routerFormat, b, &st)
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

// errTestBinary: under `go test`, os.Executable is the test binary, which would re-run its own tests as a "router".
var errTestBinary = errors.New("proxy: refusing to spawn the daemon from a test binary (stub the router seam)")

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
		return errTestBinary
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

// Serve runs the router on 127.0.0.1:port until ctx ends, then drains in-flight streams.
func Serve(ctx context.Context, port int) error {
	// A test binary never binds (or reclaims) the live router's port: reclaim would SIGTERM the user's router.
	if exe, err := os.Executable(); err == nil && port == DefaultPort && proc.IsSelfUnderTest(exe) {
		return errTestBinary
	}
	secret, err := ClientSecret()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errors.Is(err, syscall.EADDRINUSE) && port != 0 {
		// Never move to a random port: clients hold this one. Replace an older router of ours, else refuse.
		if err = reclaim(ctx, port); err == nil {
			ln, err = lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		}
	}
	if err != nil {
		return err
	}
	st := State{Schema: stateSchema, PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Started: time.Now().UTC()}
	if !ownedElsewhere(st) { // a side proxy (--port N) must not clobber a live daemon's router.json
		data, _ := json.Marshal(st)
		if err := paths.AtomicWrite(stateFile(), data, 0o600); err != nil {
			_ = ln.Close()
			return err
		}
		defer func() {
			if cur, err := ReadState(); err == nil && cur.PID == st.PID && cur.Port == st.Port {
				_ = os.Remove(stateFile())
			}
		}()
	}
	srv := &http.Server{Handler: newRouter(secret), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	fmt.Fprintf(os.Stderr, "proxy: listening on %s (pid %d)\n", Base(st.Port), st.PID)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
	defer cancel()
	// A stream past the drain is closed, not an error: a slow stop is still a stop.
	if err := srv.Shutdown(dctx); errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "proxy: drain exceeded %s, closing the remaining streams\n", shutdownDrain)
		_ = srv.Close()
	} else if err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ownedElsewhere reports a live router on another port already holding router.json.
func ownedElsewhere(st State) bool {
	cur, err := ReadState()
	return err == nil && cur.PID != 0 && cur.PID != st.PID && cur.Port != st.Port && alive(cur.PID)
}

// Stop SIGTERMs a healthy router and waits briefly for it to exit; a stale router.json is removed.
func Stop(ctx context.Context) (State, error) {
	st, ok := Running(ctx)
	if !ok {
		if st.PID != 0 && !alive(st.PID) {
			_ = os.Remove(stateFile())
		}
		return st, ErrNotRunning
	}
	if err := terminate(st.PID); err != nil {
		return st, err
	}
	// The wait follows the daemon's drain: a busy router is still stopping, not failing to stop.
	for deadline := time.Now().Add(shutdownDrain + 5*time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !alive(st.PID) {
			return st, nil
		}
	}
	return st, fmt.Errorf("pid %d signalled but still draining", st.PID)
}

// Restart replaces a running router with one spawned from the current binary; a stopped router is left alone.
func Restart(ctx context.Context) (old, cur State, restarted bool, err error) {
	old, ok := Running(ctx)
	if !ok {
		return old, State{}, false, nil
	}
	if _, err = Stop(ctx); err != nil {
		return old, State{}, false, err
	}
	if _, err = Ensure(ctx); err != nil {
		return old, State{}, false, err
	}
	cur, ok = Running(ctx)
	if !ok {
		return old, State{}, false, fmt.Errorf("proxy did not come back healthy (see %s)", LogFile())
	}
	return old, cur, true, nil
}

// PIDHeader is the /healthz response header carrying the router's pid.
const PIDHeader = "X-Claude-Code-Router-Pid"

// holder identifies who answers on port: router reports a claude-code /healthz, pid its PIDHeader (0 = older build).
func holder(ctx context.Context, port int) (pid int, router bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base(port)+"/healthz", nil)
	if err != nil {
		return 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	_ = resp.Body.Close()
	pid, _ = strconv.Atoi(resp.Header.Get(PIDHeader))
	return pid, resp.StatusCode == http.StatusOK
}

// reclaim stops an older claude-code proxy holding port; anything else is an error naming the holder.
func reclaim(ctx context.Context, port int) error {
	pid, router := holder(ctx, port)
	switch {
	case !router:
		return fmt.Errorf("proxy: 127.0.0.1:%d is held by another process; free it or pass --port", port)
	case pid <= 0 || pid == os.Getpid():
		return fmt.Errorf("proxy: 127.0.0.1:%d is held by an older claude-code proxy that reports no pid; "+
			"find it with `ss -ltnp 'sport = :%d'` and kill it", port, port)
	}
	if err := terminate(pid); err != nil {
		return fmt.Errorf("proxy: stop older proxy pid %d on :%d: %w", pid, port, err)
	}
	for deadline := time.Now().Add(shutdownDrain + 5*time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !alive(pid) {
			fmt.Fprintf(os.Stderr, "proxy: replaced older proxy pid %d on :%d\n", pid, port)
			return nil
		}
	}
	return fmt.Errorf("proxy: older proxy pid %d on :%d is still draining", pid, port)
}
