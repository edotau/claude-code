package vscode

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
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

// DefaultPort is the first port tried; freePort walks 10 more if it is busy.
const DefaultPort = 7780

// maxLogBytes truncates vscode-serve.log past this size, mirroring router's own log cap.
const maxLogBytes = 5 << 20

// ErrNotRunning means no healthy vscode-serve daemon answers for vscode-serve.json.
var ErrNotRunning = errors.New("vscode serve: daemon is not running")

// State is vscode-serve.json: written once the daemon (and any tailscale mapping) is up.
type State struct {
	Schema    int       `json:"schema"`
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Started   time.Time `json:"started"`
	Tailscale bool      `json:"tailscale"`
	Funnel    bool      `json:"funnel"`
	URL       string    `json:"url"`
}

// Options configures Start; Port 0 means DefaultPort.
type Options struct {
	Port        int
	NoTailscale bool
	Funnel      bool
	Folder      string // opened by default in the browser (absolute path); "" leaves the Welcome page
}

// ServeResult is what Start reports: the resulting state plus any non-fatal notes (tailscale fallbacks).
type ServeResult struct {
	State State
	Notes []string
}

func stateFile() string  { return filepath.Join(paths.StateDir(), "vscode-serve.json") }
func secretFile() string { return filepath.Join(paths.StateDir(), "vscode-serve.secret") }
func dataDir() string    { return filepath.Join(paths.StateDir(), "vscode-serve") }

// logFile receives the spawned `code serve-web` daemon's stdio.
func logFile() string { return filepath.Join(paths.StateDir(), "vscode-serve.log") }

// macFallback is the app bundle's CLI; a var so tests can point it at a missing path.
var macFallback = "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"

// resolveCodeBinary resolves the `code` CLI: PATH first, then the macOS app bundle's bin/code.
func resolveCodeBinary() (string, error) {
	if bin, err := paths.LookPathReal("code"); err == nil {
		return bin, nil
	}
	if fi, err := os.Stat(macFallback); err == nil && !fi.IsDir() {
		return macFallback, nil
	}
	return "", errors.New("VS Code CLI not found (install 'code' in PATH: Shell Command: Install 'code' command)")
}

// clientSecret returns the per-install token `code serve-web` requires (created 0600 on first use).
func clientSecret() (string, error) { return paths.ClientSecret(secretFile()) }

// readState reads vscode-serve.json.
func readState() (State, error) {
	var st State
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return st, err
	}
	_, err = schemas.Decode(serveFormat, b, &st)
	return st, err
}

func writeState(st State) error {
	st.Schema = stateSchema
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(stateFile(), b, 0o600)
}

func removeState() { _ = os.Remove(stateFile()) }

// Running reports the state of a live, healthy daemon (our serve-web pid alive and the port answering).
func Running() (State, bool) {
	st, err := readState()
	if err != nil || !proc.Alive(st.PID) || !isOurDaemon(st) {
		return st, false
	}
	return st, healthy(context.Background(), st.Port)
}

// healthy is true once 127.0.0.1:port answers anything (serve-web 403s without the token, which still counts).
func healthy(ctx context.Context, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// freePort returns the first of want..want+10 that currently has nothing bound to it.
func freePort(want int) (int, error) {
	for p := want; p < want+11; p++ {
		if portFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %d-%d", want, want+10)
}

func portFree(port int) bool {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// serveWebArgv builds the full `code serve-web` argv (argv[0] is bin itself, for easy comparison in tests).
func serveWebArgv(bin, secretPath string, port int, dataDir, folder string) []string {
	argv := []string{
		bin, "serve-web", "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--accept-server-license-terms", "--connection-token-file", secretPath,
		"--server-data-dir", dataDir,
	}
	if folder != "" {
		argv = append(argv, "--default-folder", folder)
	}
	return argv
}

// desktopExtensionsDir is the desktop install's extensions dir the web server shares; a var for tests.
var desktopExtensionsDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".vscode", "extensions")
}

// shareExtensions points <dataDir>/extensions at the desktop extensions dir so the phone sees the same
// extensions (Agent Dashboard included); a populated server-owned dir is left alone.
func shareExtensions(dataDir string) (bool, error) {
	src := desktopExtensionsDir()
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return false, nil
	}
	dst := filepath.Join(dataDir, "extensions")
	if cur, err := os.Readlink(dst); err == nil {
		return cur == src, nil
	}
	if entries, err := os.ReadDir(dst); err == nil {
		for _, e := range entries {
			if e.Name() != "extensions.json" {
				return false, nil // the server installed its own extensions; keep them
			}
		}
		if err := os.RemoveAll(dst); err != nil {
			return false, err
		}
	}
	return true, symlinkOrSame(src, dst)
}

// machineSettingsPath is the web server's Machine settings.json, where the harness template is merged.
func machineSettingsPath() string {
	return filepath.Join(dataDir(), "data", "Machine", "settings.json")
}

// tailscaleServeArgv builds the argv (without the `tailscale` binary) that maps port onto the
// tailnet (funnel=false) or the public internet via Funnel (funnel=true).
func tailscaleServeArgv(port int, funnel bool) []string {
	sub := "serve"
	if funnel {
		sub = "funnel"
	}
	return []string{sub, "--bg", "--https=443", fmt.Sprintf("http://127.0.0.1:%d", port)}
}

// TailscaleOffArgv builds the argv that removes the https=443 serve mapping this package sets.
func TailscaleOffArgv() []string { return []string{"serve", "--https=443", "off"} }

// PublicURL builds the phone-facing URL: the tailnet/funnel DNS name over https when present,
// otherwise ip:port over plain http — both carrying the serve-web connection token.
func PublicURL(dnsName, ip string, port int, token string) string {
	if dnsName != "" {
		return fmt.Sprintf("https://%s/?tkn=%s", strings.TrimSuffix(dnsName, "."), token)
	}
	return fmt.Sprintf("http://%s:%d/?tkn=%s", ip, port, token)
}

// tsServeStatus mirrors the subset of `tailscale serve status --json` ParseTailscaleStatus reads.
type tsServeStatus struct {
	Web map[string]struct {
		Handlers map[string]struct {
			Proxy string `json:"Proxy"`
		} `json:"Handlers"`
	} `json:"Web"`
}

// ParseTailscaleStatus scans `tailscale serve status --json` for a Web entry whose "/" handler
// proxies to our own 127.0.0.1:localPort, returning that entry's DNS name (the "<dnsname>:443" key,
// port suffix stripped).
func ParseTailscaleStatus(data []byte, localPort int) (dnsName string, ok bool) {
	var st tsServeStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return "", false
	}
	want := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	for key, web := range st.Web {
		h, present := web.Handlers["/"]
		if !present || h.Proxy != want {
			continue
		}
		name, _, _ := strings.Cut(key, ":443")
		return name, true
	}
	return "", false
}

// Start ensures the daemon is up (idempotent: a healthy daemon is reused, never respawned) and
// applies the requested tailscale mapping, returning the resulting state and any fallback notes.
func Start(ctx context.Context, opts Options) (res ServeResult, err error) {
	err = paths.WithFileLock(stateFile(), func() error {
		res, err = start(ctx, opts)
		return err
	})
	return res, err
}

// start is Start's body, run under the cross-process lock so concurrent starts cannot both spawn.
func start(ctx context.Context, opts Options) (ServeResult, error) {
	st, ok := Running()
	if !ok {
		bin, err := resolveCodeBinary()
		if err != nil {
			return ServeResult{}, err
		}
		if _, err := clientSecret(); err != nil { // ensures the secret file spawn's --connection-token-file reads
			return ServeResult{}, err
		}
		want := opts.Port
		if want == 0 {
			want = DefaultPort
		}
		port, err := freePort(want)
		if err != nil {
			return ServeResult{}, err
		}
		notes := prepareDataDir()
		pid, err := spawn(bin, port, opts.Folder)
		if err != nil {
			return ServeResult{}, err
		}
		if err := waitHealthy(ctx, port, pid); err != nil {
			_ = terminate(pid) // don't leave an unrecorded orphan holding the port
			return ServeResult{}, err
		}
		st = State{PID: pid, Port: port, Started: time.Now().UTC()}
		res, err := applyTailscale(ctx, st, opts)
		if err != nil {
			_ = terminate(pid) // no state file was written, so Stop could never reach this daemon
			return ServeResult{}, err
		}
		res.Notes = append(notes, res.Notes...)
		return res, nil
	}
	if opts.Folder != "" {
		return applyTailscaleNoting(ctx, st, opts, "already running; --folder applies on the next start (use --stop first)")
	}
	return applyTailscale(ctx, st, opts)
}

// applyTailscaleNoting is applyTailscale with one note prepended.
func applyTailscaleNoting(ctx context.Context, st State, opts Options, note string) (ServeResult, error) {
	res, err := applyTailscale(ctx, st, opts)
	res.Notes = append([]string{note}, res.Notes...)
	return res, err
}

// prepareDataDir shares the desktop extensions and merges the harness VS Code template into the web
// server's Machine settings; failures become notes, never a refused start.
func prepareDataDir() []string {
	var notes []string
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return []string{"data dir: " + err.Error()}
	}
	if shared, err := shareExtensions(dataDir()); err != nil {
		notes = append(notes, "extensions: "+err.Error())
	} else if !shared {
		notes = append(notes, "extensions: server-owned dir kept (desktop extensions not shared)")
	}
	if err := os.MkdirAll(filepath.Dir(machineSettingsPath()), 0o700); err != nil {
		return append(notes, "settings: "+err.Error())
	}
	if _, err := os.Stat(machineSettingsPath()); err != nil && !os.IsNotExist(err) {
		return append(notes, "settings: "+err.Error())
	} else if err != nil {
		// First start: the web server only shows this machine's own repos, so skip the trust banner.
		seed := []byte("{\n  \"security.workspace.trust.enabled\": false\n}\n")
		if err := paths.AtomicWrite(machineSettingsPath(), seed, 0o600); err != nil {
			notes = append(notes, "settings: "+err.Error())
		}
	}
	if _, err := Sync("", machineSettingsPath(), paths.ConfigDir(), paths.BinDir(), false); err != nil {
		notes = append(notes, "settings: "+err.Error())
	}
	return notes
}

// waitHealthy waits for port to answer while the spawned pid is still alive (a foreign responder must not pass).
func waitHealthy(ctx context.Context, port, pid int) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !proc.Alive(pid) {
			return fmt.Errorf("code serve-web exited before becoming healthy (see %s)", logFile())
		}
		if healthy(ctx, port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("code serve-web did not become healthy within 10s (see %s)", logFile())
}

func spawn(bin string, port int, folder string) (int, error) {
	lf, err := openLogFile()
	if err != nil {
		return 0, err
	}
	defer lf.Close()
	argv := serveWebArgv(bin, secretFile(), port, dataDir(), folder)
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx // The detached daemon must outlive its caller.
	cmd.Dir = paths.StateDir()
	cmd.Stdout, cmd.Stderr = lf, lf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }() // reap so an early exit reads as dead, not a zombie
	return cmd.Process.Pid, nil
}

func openLogFile() (*os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(logFile()); err == nil && fi.Size() > maxLogBytes {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(logFile(), flags, 0o600)
}

// Stop tears down any tailscale mapping this package set, then SIGTERMs the daemon whether or not it answers,
// but only when the pid's command line is our `code serve-web` (a recycled pid is left alone, its state dropped).
// The state file is removed only once the daemon is gone, so a failed or slow stop stays reachable.
func Stop() error {
	st, err := readState()
	if err != nil {
		return ErrNotRunning
	}
	if !proc.Alive(st.PID) || !isOurDaemon(st) {
		removeState()
		return ErrNotRunning
	}
	if st.Tailscale {
		stopTailscaleMapping(st.Funnel)
	}
	if err := terminate(st.PID); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && proc.Alive(st.PID) {
		time.Sleep(100 * time.Millisecond)
	}
	if proc.Alive(st.PID) {
		return fmt.Errorf("vscode serve: pid %d still running after SIGTERM; state kept", st.PID)
	}
	removeState()
	return nil
}

// procCmdline reads a pid's command line; a seam so tests can fake a daemon's argv.
var procCmdline = func(pid int) (string, error) {
	argv, err := proc.Argv(pid)
	return strings.Join(argv, " "), err
}

// isOurDaemon is true when pid's command line is a `serve-web` on this state's port; unreadable counts as no.
func isOurDaemon(st State) bool {
	line, err := procCmdline(st.PID)
	return err == nil && strings.Contains(line, " serve-web ") && strings.Contains(line, " --port "+strconv.Itoa(st.Port)+" ")
}

// tailscaleCmd builds a bounded exec.Cmd for the `tailscale` CLI. `tailscale` is a thin shell
// wrapper around the GUI app binary (no `exec`), so a cancelled context only kills the wrapper; a
// still-running grandchild then keeps stdout/stderr's pipe open forever. WaitDelay forces Wait() to
// give up and close those pipes a moment after cancellation instead of blocking on that orphan.
func tailscaleCmd(ctx context.Context, timeout time.Duration, bin string, args ...string) (*exec.Cmd, context.CancelFunc) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	return cmd, cancel
}

func stopTailscaleMapping(funnel bool) {
	bin, err := paths.LookPathReal("tailscale")
	if err != nil {
		return
	}
	args := TailscaleOffArgv()
	if funnel {
		args[0] = "funnel"
	}
	cmd, cancel := tailscaleCmd(context.Background(), 5*time.Second, bin, args...)
	defer cancel()
	_ = cmd.Run() // best-effort teardown
}

// TailscaleMapping re-queries `tailscale serve status --json` for port's current mapping — the
// live source of truth `vscode serve --status` reports against, independent of our own state file.
func TailscaleMapping(ctx context.Context, port int) (dnsName string, ok bool) {
	bin, err := paths.LookPathReal("tailscale")
	if err != nil {
		return "", false
	}
	return queryTailscaleServeStatus(ctx, bin, port)
}

func queryTailscaleServeStatus(ctx context.Context, bin string, port int) (string, bool) {
	cmd, cancel := tailscaleCmd(ctx, 5*time.Second, bin, "serve", "status", "--json")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return ParseTailscaleStatus(out, port)
}

// applyTailscale sets (or skips) the tailscale/funnel mapping, builds st.URL, persists state, and
// returns any fallback notes (missing CLI, mapping failure) instead of failing Start outright.
func applyTailscale(ctx context.Context, st State, opts Options) (ServeResult, error) {
	token, err := clientSecret()
	if err != nil {
		return ServeResult{}, err
	}
	if opts.NoTailscale {
		if st.Tailscale {
			stopTailscaleMapping(st.Funnel) // a rerun must not orphan the mapping an earlier run set
		}
		st.Tailscale, st.Funnel = false, false
		st.URL = PublicURL("", "127.0.0.1", st.Port, token)
		return finish(st, nil)
	}
	bin, err := paths.LookPathReal("tailscale")
	if err != nil {
		st.Tailscale, st.Funnel = false, false
		st.URL = PublicURL("", lanHost(ctx), st.Port, token)
		return finish(st, []string{"tailscale CLI not found on PATH; serving on the LAN only"})
	}
	args := tailscaleServeArgv(st.Port, opts.Funnel)
	cmd, cancel := tailscaleCmd(ctx, 5*time.Second, bin, args...)
	out, runErr := cmd.CombinedOutput()
	cancel()
	if runErr != nil {
		st.Tailscale, st.Funnel = false, false
		st.URL = PublicURL("", "127.0.0.1", st.Port, token)
		return finish(st, []string{fmt.Sprintf("tailscale %s failed: %v: %s", args[0], runErr, strings.TrimSpace(string(out)))})
	}
	st.Tailscale, st.Funnel = true, opts.Funnel
	var notes []string
	dnsName, mapped := queryTailscaleServeStatus(ctx, bin, st.Port)
	if !mapped {
		notes = append(notes, "tailscale serve is up, but its status didn't confirm the mapping yet; use the printed IP instead")
		st.URL = PublicURL("", lanHost(ctx), st.Port, token)
	} else {
		st.URL = PublicURL(dnsName, "", st.Port, token)
	}
	if opts.Funnel {
		notes = append(notes, "WARNING: --funnel makes this URL reachable from the public internet (still token-gated)")
	}
	return finish(st, notes)
}

func finish(st State, notes []string) (ServeResult, error) {
	if err := writeState(st); err != nil {
		return ServeResult{}, err
	}
	return ServeResult{State: st, Notes: notes}, nil
}

// lanHost is this node's first tailnet IPv4, falling back to loopback when status can't be read.
func lanHost(ctx context.Context) string {
	if local, err := tailscale.Status(ctx); err == nil && len(local.IPs) > 0 {
		return local.IPs[0]
	}
	return "127.0.0.1"
}
