//go:build !windows

// Command claude-code-tsnet is the harness's userspace tailnet node for hosts with no reachable tailscaled.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

const (
	upTimeout   = 60 * time.Second
	peerWait    = 5 * time.Second
	peerPoll    = 100 * time.Millisecond
	maxKeyBytes = 512
	maxPort     = 65535
	usage       = "usage: claude-code-tsnet init (auth key on stdin) | dial <host> <port> | status --json | logout"
)

var (
	hostRe       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	errNotInited = errors.New("tsnet node not initialized: run `claude-code-tsnet init` with an auth key on stdin")
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code-tsnet:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	harden()
	verbs := map[string]struct {
		argc int
		fn   func(args []string) error
	}{
		"dial":   {3, dial},
		"status": {2, status},
		"init":   {1, func([]string) error { return provision() }},
		"logout": {1, func([]string) error { return logout() }},
	}
	if len(args) == 0 {
		return errors.New(usage)
	}
	v, ok := verbs[args[0]]
	if !ok || len(args) != v.argc {
		return errors.New(usage)
	}
	return v.fn(args)
}

func dial(args []string) error {
	addr, err := dialAddr(args[1], args[2])
	if err != nil {
		return err
	}
	return withNode(false, func(ctx context.Context, srv *tsnet.Server) error { return bridge(ctx, srv, addr) })
}

func status(args []string) error {
	if args[1] != "--json" {
		return errors.New(usage)
	}
	return withNode(false, printStatus)
}

// harden keeps key material out of core dumps, tracebacks and log.tailscale.com uploads.
func harden() {
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
	debug.SetTraceback("none")
	envknob.SetNoLogsNoSupport()
}

// dialAddr validates the ssh %h %p pair; ProxyCommand runs under /bin/sh, so the host charset is strict.
func dialAddr(host, port string) (string, error) {
	if _, err := netip.ParseAddr(host); err != nil && !hostRe.MatchString(host) {
		return "", fmt.Errorf("invalid host %q: want a DNS label or an IP address", host)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > maxPort {
		return "", fmt.Errorf("invalid port %q: want 1-%d", port, maxPort)
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// stateDir is $CLAUDE_CONFIG_DIR (default ~/.claude)/state/tailscale/tsnet, kept off the WSL /mnt Windows mounts.
func stateDir() (string, error) {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".claude")
	}
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	dir := filepath.Join(base, "state", "tailscale", "tsnet")
	if isWSL() && (dir == "/mnt" || strings.HasPrefix(dir, "/mnt/")) {
		return "", fmt.Errorf("refusing state dir %s on a Windows mount: keep ~/.claude on the Linux filesystem", dir)
	}
	return dir, nil
}

func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && bytes.Contains(bytes.ToLower(b), []byte("microsoft"))
}

// checkPerms refuses a state dir that is a symlink, group/world accessible, or not owned by this user.
func checkPerms(dir string) error {
	uid := uint32(os.Getuid())
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("refusing %s: symlink in the state dir", path)
		case !ok || st.Uid != uid:
			return fmt.Errorf("refusing %s: not owned by uid %d", path, uid)
		case fi.Mode().Perm()&0o077 != 0:
			return fmt.Errorf("refusing %s: mode %o, want 0700 dirs and 0600 files", path, fi.Mode().Perm())
		}
		return nil
	})
}

// initialized reports whether Dir holds node state; a fresh FileStore writes just "{}".
func initialized(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "tailscaled.state"))
	return err == nil && len(bytes.TrimSpace(b)) > 2
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "host"
	}
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	if isWSL() {
		h += "-wsl"
	}
	return h + "-cc"
}

// keyLogf drops any log line that could carry an auth key.
func keyLogf(w io.Writer) func(string, ...any) {
	return func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		low := strings.ToLower(line)
		if strings.Contains(low, "tskey") || strings.Contains(low, "authkey") || strings.Contains(low, "auth key") {
			return
		}
		fmt.Fprintln(w, strings.TrimRight(line, "\n"))
	}
}

func newServer(dir string, verbose bool) *tsnet.Server {
	backend := io.Discard
	if verbose {
		backend = os.Stderr
	}
	return &tsnet.Server{
		Dir:       dir,
		Hostname:  hostname(),
		Ephemeral: false,
		Logf:      keyLogf(backend),
		UserLogf:  keyLogf(backend),
	}
}

// withNode runs fn on an already provisioned node; it never provisions, so stdin stays the ssh bridge.
func withNode(verbose bool, fn func(context.Context, *tsnet.Server) error) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) || (err == nil && !initialized(dir)) {
		return fmt.Errorf("%w (state dir %s)", errNotInited, dir)
	} else if err != nil {
		return err
	}
	if err := checkPerms(dir); err != nil {
		return err
	}
	srv := newServer(dir, verbose)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), upTimeout)
	defer cancel()
	if _, err := srv.Up(ctx); err != nil {
		return fmt.Errorf("bring up tsnet node: %w", err)
	}
	if err := shieldsUp(ctx, srv); err != nil {
		return err
	}
	return fn(context.Background(), srv)
}

// shieldsUp makes the node refuse every inbound connection, whatever the tailnet policy grants; it is persisted.
func shieldsUp(ctx context.Context, srv *tsnet.Server) error {
	lc, err := srv.LocalClient()
	if err != nil {
		return err
	}
	if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{Prefs: ipn.Prefs{ShieldsUp: true}, ShieldsUpSet: true}); err != nil {
		return fmt.Errorf("shields up: %w", err)
	}
	return nil
}

func printStatus(ctx context.Context, srv *tsnet.Server) error {
	st, err := netStatus(ctx, srv)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

// netStatus waits briefly for a fresh node's netmap, which lands just after Up, so peers are known.
func netStatus(ctx context.Context, srv *tsnet.Server) (*ipnstate.Status, error) {
	lc, err := srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	for deadline := time.Now().Add(peerWait); err == nil && len(st.Peer) == 0 && time.Now().Before(deadline); {
		time.Sleep(peerPoll)
		st, err = lc.Status(ctx)
	}
	return st, err
}

// bridge copies stdin to the tailnet connection and the connection to stdout until either side ends.
func bridge(ctx context.Context, srv *tsnet.Server, addr string) error {
	if _, err := netStatus(ctx, srv); err != nil {
		return err
	}
	conn, err := srv.Dial(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}

// readKey reads the auth key from stdin into a buffer the caller zeroes.
func readKey() ([]byte, error) {
	buf := make([]byte, maxKeyBytes+1)
	n, err := io.ReadFull(bufio.NewReader(os.Stdin), buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return buf, fmt.Errorf("read auth key from stdin: %w", err)
	}
	if n > maxKeyBytes {
		return buf, fmt.Errorf("auth key on stdin is over %d bytes", maxKeyBytes)
	}
	key := bytes.TrimSpace(buf[:n])
	if !bytes.HasPrefix(key, []byte("tskey-")) {
		return buf, errors.New("stdin holds no tskey- auth key")
	}
	return buf[:n], nil
}

// provision registers the node once with the stdin key; the string copy tsnet needs is unavoidable.
func provision() error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	if initialized(dir) {
		return fmt.Errorf("already initialized (state dir %s); run logout first to re-provision", dir)
	}
	buf, err := readKey()
	defer clear(buf)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkPerms(dir); err != nil {
		return err
	}
	srv := newServer(dir, true)
	srv.AuthKey = string(bytes.TrimSpace(buf))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), upTimeout)
	defer cancel()
	st, err := srv.Up(ctx)
	srv.AuthKey = ""
	if err != nil {
		return fmt.Errorf("provision tsnet node: %w", err)
	}
	if err := shieldsUp(ctx, srv); err != nil {
		return err
	}
	fmt.Printf("provisioned %s %v\n", hostname(), st.TailscaleIPs)
	return nil
}

// logout revokes the node key on the control server, then deletes the local state so dial needs a new init.
func logout() error {
	err := withNode(true, func(ctx context.Context, srv *tsnet.Server) error {
		lc, err := srv.LocalClient()
		if err != nil {
			return err
		}
		return lc.Logout(ctx)
	})
	if err != nil {
		return err
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
