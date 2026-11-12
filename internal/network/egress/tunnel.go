package egress

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// Tunnel is one recorded ssh SOCKS5 tunnel.
type Tunnel struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	PID  int    `json:"pid"`
}

func (t Tunnel) addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port)) }

func dir() string { return filepath.Join(paths.StateDir(), "ssh") }

// recordPath is host's tunnel record; the name is a hash so any valid host is a safe file name.
func recordPath(host string) string {
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir(), "egress-"+hex.EncodeToString(sum[:8])+".json")
}

func readRecord(path string) (Tunnel, bool) {
	var t Tunnel
	b, err := os.ReadFile(path)
	return t, err == nil && json.Unmarshal(b, &t) == nil && t.Port > 0
}

// Ensure returns the loopback SOCKS5 address of host's tunnel, starting ssh when none answers.
func Ensure(ctx context.Context, host string) (string, error) {
	if err := Valid(host); err != nil {
		return "", fmt.Errorf("egress host %s: %w", host, err)
	}
	path := recordPath(host)
	if t, ok := readRecord(path); ok && answers(ctx, t.addr()) {
		return t.addr(), nil
	}
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return "", fmt.Errorf("egress host %s: %w", host, err)
	}
	var addr string
	err := paths.WithFileLock(path, func() error {
		if t, ok := readRecord(path); ok && answers(ctx, t.addr()) { // another process started it while we waited
			addr = t.addr()
			return nil
		}
		t, err := start(ctx, host, strings.TrimSuffix(path, ".json")+".log")
		if err != nil {
			return err
		}
		addr = t.addr()
		if err := writeRecord(path, t); err != nil {
			kill(t) // an unrecorded tunnel could never be stopped
			return err
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("egress host %s: %w", host, err)
	}
	return addr, nil
}

func writeRecord(path string, t Tunnel) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(path, b, 0o600)
}

// answers reports a SOCKS5 server at addr: it accepts the no-auth greeting.
func answers(ctx context.Context, addr string) bool {
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return false
	}
	reply := make([]byte, 2)
	_, err = io.ReadFull(conn, reply)
	return err == nil && reply[0] == 5 && reply[1] == 0
}

// start spawns the tunnel on a free loopback port and waits until it answers, ssh exits, or startWait passes.
func start(ctx context.Context, host, logPath string) (Tunnel, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return Tunnel{}, err
	}
	t := Tunnel{Host: host, Port: l.Addr().(*net.TCPAddr).Port}
	_ = l.Close()
	pid, exited, err := spawn(host, t.Port, logPath)
	if err != nil {
		return Tunnel{}, err
	}
	t.PID = pid
	deadline := time.NewTimer(startWait)
	defer deadline.Stop()
	for {
		if answers(ctx, t.addr()) {
			return t, nil
		}
		select {
		case <-exited:
			return Tunnel{}, fmt.Errorf("ssh tunnel failed: %s", sshCause(logPath))
		case <-ctx.Done():
			kill(t)
			return Tunnel{}, ctx.Err()
		case <-deadline.C:
			kill(t)
			return Tunnel{}, fmt.Errorf("ssh tunnel did not come up within %s", startWait)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// sshArgs is the tunnel's ssh argv after "ssh": no prompt, no shared master, dead within 30s of a lost link.
func sshArgs(host string, port int, logPath string) []string {
	return []string{"-N", "-D", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "-E", logPath,
		"-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2",
		"-o", "ControlMaster=no", "-o", "ControlPath=none", "--", host}
}

// spawnSSH starts the detached tunnel; exited closes when ssh ends, and ssh writes its own errors to logPath.
func spawnSSH(host string, port int, logPath string) (int, <-chan struct{}, error) {
	_ = os.Remove(logPath)
	cmd := exec.Command("ssh", sshArgs(host, port, logPath)...) //nolint:noctx // the tunnel outlives its caller
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return cmd.Process.Pid, exited, nil
}

// sshCause is the last line ssh logged, the reason a tunnel did not start.
func sshCause(logPath string) string {
	b, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return cmp.Or(strings.TrimSpace(lines[len(lines)-1]), "ssh exited before the tunnel came up")
}

// isTunnel reports pid t.PID still is t's ssh process: a recycled pid is not.
func isTunnel(t Tunnel) bool {
	if t.PID <= 0 {
		return false
	}
	argv, err := proc.Argv(t.PID)
	return err == nil && len(argv) > 0 && filepath.Base(argv[0]) == "ssh" && slices.Contains(argv, t.addr())
}

// kill ends t's ssh process, only when the pid still is that tunnel: a recycled pid is left alone.
func kill(t Tunnel) bool {
	if !isTunnel(t) {
		return false
	}
	p, err := os.FindProcess(t.PID)
	return err == nil && p.Kill() == nil
}

// Tunnels lists the recorded tunnels.
func Tunnels() []Tunnel {
	records, _ := filepath.Glob(filepath.Join(dir(), "egress-*.json"))
	var out []Tunnel
	for _, p := range records {
		if t, ok := readRecord(p); ok {
			out = append(out, t)
		}
	}
	return out
}

// Stop kills host's recorded tunnel and drops its record; a missing record is not an error.
func Stop(host string) error {
	path := recordPath(host)
	t, ok := readRecord(path)
	if !ok {
		return nil
	}
	if !kill(t) && isTunnel(t) {
		return fmt.Errorf("egress host %s: could not kill ssh pid %d", host, t.PID)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("egress host %s: %w", host, err)
	}
	return nil
}
