package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

const (
	DefaultPort = 18765
	maxLogBytes = 10 << 20
)

// shutdownDrain bounds a SIGTERM's wait on in-flight streams; a var so a test can shorten it.
var shutdownDrain = 30 * time.Second

// ErrNotRunning means no healthy router answers for router.json.
var ErrNotRunning = errors.New("router is not running")

// State is router.json: written by a serving daemon, removed when it exits.
type State struct {
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
	return st, json.Unmarshal(b, &st)
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// ClientSecret returns the per-install secret clients must present (created 0600 on first use).
func ClientSecret() (string, error) {
	file := secretFile()
	if s, err := readSecret(file); err == nil {
		return s, nil
	}
	var out string
	err := paths.WithFileLock(file, func() error {
		if s, err := readSecret(file); err == nil {
			out = s
			return nil
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		out = hex.EncodeToString(b)
		return paths.AtomicWrite(file, []byte(out+"\n"), 0o600)
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
	return "", errors.New("empty router secret")
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
		return fmt.Errorf("router did not become healthy within 3s (see %s)", LogFile())
	})
	return base, err
}

func spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Resolve shims: argv[0] `claude` would dispatch to the launcher instead of `router serve`.
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
	cmd := exec.Command(exe, "router", "serve")
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
	secret, err := ClientSecret()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errors.Is(err, syscall.EADDRINUSE) && port == DefaultPort {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // router.json carries the real port
	}
	if err != nil {
		return err
	}
	st := State{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Started: time.Now().UTC()}
	data, _ := json.Marshal(st)
	if err := paths.AtomicWrite(stateFile(), data, 0o600); err != nil {
		ln.Close()
		return err
	}
	defer func() {
		if cur, err := ReadState(); err == nil && cur.PID == st.PID && cur.Port == st.Port {
			os.Remove(stateFile())
		}
	}()
	srv := &http.Server{Handler: newRouter(secret), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	fmt.Fprintf(os.Stderr, "router: listening on %s (pid %d)\n", Base(st.Port), st.PID)
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
		fmt.Fprintf(os.Stderr, "router: drain exceeded %s, closing the remaining streams\n", shutdownDrain)
		_ = srv.Close()
	} else if err != nil {
		return err
	}
	<-errc
	return nil
}

// Stop SIGTERMs a healthy router and waits briefly for it to exit; a stale router.json is removed.
func Stop(ctx context.Context) (State, error) {
	st, ok := Running(ctx)
	if !ok {
		if st.PID != 0 && !alive(st.PID) {
			os.Remove(stateFile())
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
