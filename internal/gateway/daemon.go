package gateway

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
	"github.com/edotau/claude-code/internal/providers"
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

// HolderComm names the process behind a holder's pid; a test seam (the /healthz pid header is unauthenticated).
var HolderComm = proc.Comm

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

// logFile receives a spawned daemon's stdio.
func logFile() string { return filepath.Join(paths.StateDir(), "proxy.log") }

// Base is the proxy URL for port.
func Base(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// SessionBase is the ANTHROPIC_BASE_URL for a session whose default provider is provider.
func SessionBase(base, provider string) string { return base + "/p/" + provider }

// ClientHeader carries the proxy secret when Authorization holds the client's own login (subscription OAuth).
const ClientHeader = "X-Claude-Code-Proxy"

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
	resp, err := probeHealth(ctx, st.Port)
	if err != nil || resp.StatusCode != http.StatusOK {
		return st, false
	}
	// A different pid means the recorded one was recycled; /healthz is unauthenticated, so only the OS names the process.
	if got, _ := strconv.Atoi(resp.Header.Get(PIDHeader)); got != 0 && got != st.PID {
		return st, false
	}
	return st, HolderComm(st.PID) == "claude-code"
}

// healthGet issues GET /healthz on port; the caller closes the body.
func healthGet(ctx context.Context, port int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base(port)+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// Health GETs /healthz on port.
func Health(ctx context.Context, port int) error {
	resp, err := probeHealth(ctx, port)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// healthReply is what a /healthz probe keeps: the status and headers, never an open body.
type healthReply struct {
	StatusCode int
	Status     string
	Header     http.Header
}

// probeHealth GETs /healthz with a 1s timeout and closes the body before returning.
func probeHealth(ctx context.Context, port int) (healthReply, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	resp, err := healthGet(ctx, port)
	if err != nil {
		return healthReply{}, err
	}
	_ = resp.Body.Close()
	return healthReply{resp.StatusCode, resp.Status, resp.Header}, nil
}

// Ensure starts the router daemon if it is not already healthy and returns its base URL (http://127.0.0.1:<port>).
func Ensure(ctx context.Context) (string, error) {
	release() // any start, by hand or by a launch, ends a deliberate --stop
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
		return fmt.Errorf("proxy did not become healthy within 3s (see %s)", logFile())
	})
	return base, err
}

// ErrTestBinary: under `go test`, os.Executable is the test binary, which would re-run its own tests as a "router".
var ErrTestBinary = errors.New("proxy: refusing to spawn the daemon from a test binary (stub the router seam)")

// daemonCommand builds the detached daemon's command line.
func daemonCommand(exe string) *exec.Cmd {
	return exec.Command(exe, "gateway", "proxy", "--serve") //nolint:noctx // the detached daemon outlives its caller
}

func spawn() error {
	exe, err := paths.SelfBinary() // resolved: argv[0] `claude` would dispatch to the launcher, not `gateway proxy --serve`
	if err != nil {
		return err
	}
	if proc.IsSelfUnderTest(exe) {
		return ErrTestBinary
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(logFile()); err == nil && fi.Size() > maxLogBytes {
		flags |= os.O_TRUNC
	}
	lf, err := os.OpenFile(logFile(), flags, 0o600)
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
	if !awaitExit(st.PID) {
		return st, fmt.Errorf("pid %d signalled but still draining", st.PID)
	}
	return st, nil
}

// awaitExit waits for pid to exit, following the daemon's drain: a busy router is still stopping, not failing to stop.
func awaitExit(pid int) bool {
	deadline := time.Now().Add(ShutdownDrain + 5*time.Second)
	for ; time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !alive(pid) {
			return true
		}
	}
	return false
}

// awaitPortFree waits up to 5s for 127.0.0.1:port to stop accepting connections (the daemon closes it on SIGTERM).
func awaitPortFree(ctx context.Context, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		d := net.Dialer{Timeout: 200 * time.Millisecond}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return ctx.Err() // a cancelled ctx is not a free port
		}
		_ = c.Close()
	}
	return fmt.Errorf("proxy still accepting on :%d after SIGTERM", port)
}

// restart replaces a running router with one from the current binary (gap: the respawn); a stopped one is left alone.
func restart(ctx context.Context) (old, cur State, restarted bool, err error) {
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
		return old, State{}, false, fmt.Errorf("proxy did not come back healthy (see %s)", logFile())
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
	resp, err := probeHealth(ctx, port)
	if err != nil {
		return HolderState{}, false
	}
	var st HolderState
	st.PID, _ = strconv.Atoi(resp.Header.Get(PIDHeader))
	st.Build = resp.Header.Get(HeaderBuild)
	st.Config = resp.Header.Get(HeaderConfig)
	st.Sessions, _ = strconv.Atoi(resp.Header.Get(HeaderSessions))
	st.Inflight, _ = strconv.ParseInt(resp.Header.Get(HeaderInflight), 10, 64)
	return st, resp.StatusCode == http.StatusOK
}

// Reclaim stops an older claude-code proxy holding port; anything else is an error naming the holder.
func Reclaim(ctx context.Context, port int, force bool) error {
	st, router := Holder(ctx, port)
	pid := st.PID
	if err := checkReclaimable(st, router, port, force); err != nil {
		return err
	}
	if st.Config == "" && !recordedPID(pid) { // a pid claimed by unauthenticated headers alone is never signalled
		return fmt.Errorf("%w (127.0.0.1:%d claims pid %d, which proxy.json does not record); free it or pass --port",
			ErrForeignHolder, port, pid)
	}
	if err := terminateHolder(pid); err != nil {
		return fmt.Errorf("proxy: stop older proxy pid %d on :%d: %w", pid, port, err)
	}
	return awaitHolderExit(pid, port)
}

// recordedPID reports whether the state file names pid as the proxy.
func recordedPID(pid int) bool {
	cur, err := ReadState()
	return err == nil && cur.PID == pid
}

// checkReclaimable returns nil when the holder st on port is an older proxy of this config that may be stopped.
func checkReclaimable(st HolderState, router bool, port int, force bool) error {
	pid := st.PID
	if err := checkHolderPid(router, pid, port); err != nil {
		return err
	}
	if st.Config != "" && st.Config != ConfigID() { // another config dir's proxy is never ours to replace
		return fmt.Errorf("%w (127.0.0.1:%d answers as a proxy of another config dir or host, left alone); pass --port",
			ErrForeignHolder, port)
	}
	if !force && st.Build != textutil.BuildVersion() && (st.Inflight > 0 || st.Sessions > 0) {
		fmt.Fprintf(os.Stderr,
			"proxy: keeping running proxy pid %d (build %s) with live work; this build %s not taking over\n", pid,
			st.Build, textutil.BuildVersion())
		return ErrKeptHolder
	}
	return nil
}

// checkHolderPid refuses a non-router holder, a pid-less one, and a pid that is not a claude-code process.
func checkHolderPid(router bool, pid, port int) error {
	switch {
	case !router:
		return fmt.Errorf("%w (127.0.0.1:%d is held by another process); free it or pass --port", ErrForeignHolder,
			port)
	case pid <= 0 || pid == os.Getpid():
		return fmt.Errorf("proxy: 127.0.0.1:%d is held by an older claude-code proxy that reports no pid; "+
			"find it with `ss -ltnp 'sport = :%d'` and kill it", port, port)
	}
	if comm := HolderComm(pid); comm != "claude-code" { // never signal a pid an unverified /healthz reply named
		return fmt.Errorf("%w (127.0.0.1:%d names pid %d, which is %q, not claude-code); pass --port",
			ErrForeignHolder, port, pid, comm)
	}
	return nil
}

// awaitHolderExit waits for pid to exit after it was told to stop.
func awaitHolderExit(pid, port int) error {
	if awaitExit(pid) {
		fmt.Fprintf(os.Stderr, "proxy: replaced older proxy pid %d on :%d\n", pid, port)
		return nil
	}
	return fmt.Errorf("proxy: older proxy pid %d on :%d is still draining", pid, port)
}

// stateSchema versions router-runtime.
const stateSchema = 1

var routerFormat = schemas.Register(schemas.Format{
	Name: "router-runtime", Owner: "internal/gateway", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "runtime state; removed when the daemon exits",
	Migrate: schemas.SameShape,
})

// WriteState publishes this daemon's state and returns its conditional cleanup; check and write share one lock.
func WriteState(st State) (func(), error) {
	st.Schema = stateSchema
	wrote := false
	err := paths.WithFileLock(stateFile(), func() error {
		if ownedElsewhere(st) {
			return nil
		}
		data, _ := json.Marshal(st)
		if err := paths.AtomicWrite(stateFile(), data, 0o600); err != nil {
			return err
		}
		_ = os.Remove(legacyStateFile()) // this daemon now owns proxy.json
		wrote = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !wrote {
		return func() {}, nil
	}
	return func() {
		if cur, err := ReadState(); err == nil && cur.PID == st.PID && cur.Port == st.Port {
			_ = os.Remove(stateFile())
		}
	}, nil
}

// AgentHeader is the optional launcher-set header (claude, agent, copilot, desktop) attributing a ledger entry.
const AgentHeader = "X-Claude-Code-Agent"

// IdentityHeader carries a launch's Databricks identity (providers.WithIdentity) to the proxy's credential mint.
const IdentityHeader = "X-Claude-Code-Identity"

// Status is the loopback proxy's status: the payload of `gateway proxy --format json` and bare `gateway`'s "proxy".
type Status struct {
	Running  bool     `json:"running"`
	PID      int      `json:"pid,omitempty"`
	URL      string   `json:"url,omitempty"`
	Started  string   `json:"started,omitempty"`
	Fallback []string `json:"fallback"`
	Log      string   `json:"-"`
}

// LoadStatus reads the running proxy and the registry's fallback chain; a load error returns the partial Status.
func LoadStatus(ctx context.Context) (Status, error) {
	ps := Status{Log: logFile()}
	if st, ok := Running(ctx); ok {
		ps.Running, ps.PID, ps.URL = true, st.PID, Base(st.Port)
		ps.Started = st.Started.Local().Format(time.RFC3339)
	}
	reg, err := providers.Load()
	if err != nil {
		return ps, err
	}
	ps.Fallback = append([]string{}, reg.Fallback...)
	return ps, nil
}
