package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// ErrListenersUnsupported: no listener inventory on this platform.
var ErrListenersUnsupported = errors.New("listener inventory unsupported on " + runtime.GOOS)

// Listener is one process holding the proxy port.
type Listener struct {
	PID       int
	Exe       string
	Started   string
	ConfigDir string // "unknown" when no proxy.json names this pid
	Installed bool   // Exe is the installed claude-code binary
	Current   bool   // proxy.json names this pid
}

// KillLine is the exact command that stops a stray holder; nothing here sends it.
func (l Listener) KillLine() string { return "kill " + strconv.Itoa(l.PID) }

// Listeners lists every process listening on port, read-only.
func Listeners(ctx context.Context, port int) ([]Listener, error) {
	var pids []int
	var err error
	switch runtime.GOOS {
	case "darwin":
		pids, err = lsofPIDs(ctx, port)
	case "linux":
		pids, err = procNetPIDs(port)
	default:
		return nil, ErrListenersUnsupported
	}
	if err != nil {
		return nil, err
	}
	st, _ := ReadState()
	installed := filepath.Join(paths.BinDir(), "claude-code")
	out := make([]Listener, 0, len(pids))
	for _, pid := range pids {
		l := Listener{PID: pid, Exe: proc.Exe(pid), ConfigDir: "unknown", Started: psStart(ctx, pid)}
		if l.Exe == "" {
			l.Exe = proc.Comm(pid)
		}
		l.Installed = sameFile(l.Exe, installed)
		if l.Current = st.PID == pid && st.Port == port; l.Current {
			l.ConfigDir = paths.ConfigDir()
		}
		out = append(out, l)
	}
	return out, nil
}

func lsofPIDs(ctx context.Context, port int) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpn").Output()
	if ee, isExit := errors.AsType[*exec.ExitError](err); err != nil && (!isExit || ee.ExitCode() != 1 || ctx.Err() != nil) {
		return nil, fmt.Errorf("lsof: %w", err) // exit 1 alone is no match
	}
	return parseLsofPIDs(string(out)), nil
}

// parseLsofPIDs reads lsof -Fpn output: a "p<pid>" line starts each process, "n<addr>" lines follow.
func parseLsofPIDs(out string) []int {
	var pids []int
	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "p"); ok {
			if pid, err := strconv.Atoi(v); err == nil && !slices.Contains(pids, pid) {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func psStart(ctx context.Context, pid int) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if s := strings.TrimSpace(string(out)); err == nil && s != "" {
		return s
	}
	return "unknown"
}

func procNetPIDs(port int) ([]int, error) {
	inodes, err := listenInodes(port)
	if err != nil || len(inodes) == 0 {
		return nil, err
	}
	fds, err := filepath.Glob("/proc/[0-9]*/fd/*")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, fd := range fds {
		target, err := os.Readlink(fd)
		ino, ok := strings.CutPrefix(target, "socket:[")
		if err != nil || !ok || !inodes[strings.TrimSuffix(ino, "]")] {
			continue
		}
		if pid, err := strconv.Atoi(strings.Split(fd, "/")[2]); err == nil && !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func listenInodes(port int) (map[string]bool, error) {
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ino := range parseProcNetInodes(string(raw), port) {
			inodes[ino] = true
		}
	}
	return inodes, nil
}

// parseProcNetInodes returns the socket inodes in LISTEN state (0A) on port from /proc/net/tcp[6] text.
func parseProcNetInodes(text string, port int) []string {
	var inodes []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[3] != "0A" {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if p, err := strconv.ParseInt(hexPort, 16, 32); ok && err == nil && int(p) == port {
			inodes = append(inodes, f[9])
		}
	}
	return inodes
}

// sameFile compares paths after resolving symlinks.
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ra == rb
}
