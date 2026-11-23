package proc

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// vscodeCLI is the remote-cli code and a live client socket; "" when no VS Code window reaches this box.
func vscodeCLI() (code, sock string) {
	if code = remoteCode(); code == "" {
		return "", ""
	}
	if s := os.Getenv("VSCODE_IPC_HOOK_CLI"); s != "" && sockLive(s) {
		return code, s
	}
	for _, s := range newestFirst(vscodeSocks()) {
		if sockLive(s) {
			return code, s
		}
	}
	return "", ""
}

// vscodeSocks are VS Code's vscode-ipc-*.sock in $XDG_RUNTIME_DIR (else /run/user/<uid>) and TMPDIR.
func vscodeSocks() []string {
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		run = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	var socks []string
	for _, d := range []string{run, os.TempDir()} {
		m, _ := filepath.Glob(filepath.Join(d, "vscode-ipc-*.sock"))
		socks = append(socks, m...)
	}
	return socks
}

// remoteCode is the VS Code server's remote-cli code: PATH's in a VS Code terminal, else the newest installed.
func remoteCode() string {
	if p, err := exec.LookPath("code"); err == nil && strings.Contains(filepath.ToSlash(p), "/remote-cli/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var found []string
	for _, g := range []string{"bin/*/bin/remote-cli/code", "cli/servers/*/server/bin/remote-cli/code"} {
		m, _ := filepath.Glob(filepath.Join(home, ".vscode-server", g))
		found = append(found, m...)
	}
	if found = newestFirst(found); len(found) == 0 {
		return ""
	}
	return found[0]
}

// newestFirst sorts paths by modification time, newest first; unreadable ones drop out.
func newestFirst(paths []string) []string {
	at := map[string]int64{}
	var out []string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			at[p] = fi.ModTime().UnixNano()
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(at[b], at[a]) })
	return out
}

// sockDialTimeout bounds one liveness probe; a local socket answers in microseconds.
const sockDialTimeout = 300 * time.Millisecond

// sockLive is a socket that accepts a connection: a closed VS Code window leaves a dead file behind.
func sockLive(sock string) bool {
	d := net.Dialer{Timeout: sockDialTimeout}
	c, err := d.DialContext(context.Background(), "unix", sock)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// vscodeOpen runs `code <flag> <target>` against sock, so the client machine opens target; target is one argument.
func vscodeOpen(code, sock, flag, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, code, flag, target)
	cmd.Env = ValuesFromEnv(os.Environ()).With("VSCODE_IPC_HOOK_CLI", sock).Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("VS Code could not open it: %w: %s", err, line)
	}
	return nil
}
