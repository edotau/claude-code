// Package remote builds the ssh argv that runs a script on another host with stdin left free for the caller.
package remote

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// BinEnv names the remote claude-code binary when --remote-bin is absent; else "claude-code" on the remote PATH.
const BinEnv = "HARNESS_REMOTE_BIN"

// PathPrefix is `make install`'s login PATH: ssh's non-login shell lacks it, so claude-code and auth commands fail.
const PathPrefix = `export PATH="$HOME/.claude/bin:$HOME/.local/bin:$HOME/.local/go/bin:$HOME/go/bin:$PATH";`

// Bin picks the remote binary: the flag, then HARNESS_REMOTE_BIN, then "claude-code".
func Bin(flagValue string) string { return cmp.Or(flagValue, os.Getenv(BinEnv), "claude-code") }

// HomePath rewrites a path under the local home to ~/…: the remote's home differs (/Users/x vs /home/x).
func HomePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || p == "" {
		return p
	}
	rel, err := filepath.Rel(home, p)
	if err == nil && filepath.IsAbs(p) && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.Join("~", rel)
	}
	return p
}

// ValidHost refuses an ssh destination that ssh would parse as an option or that carries whitespace/control bytes.
func ValidHost(host string) error {
	bad := func(r rune) bool { return r <= ' ' || r == 0x7f }
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsFunc(host, bad) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

// encodeScript wraps script as `sh -c` with a base64 body, so sh, bash and fish parse it identically.
func encodeScript(script string) string {
	return `sh -c 'eval "$(printf %s ` + base64.StdEncoding.EncodeToString([]byte(script)) + ` | base64 -d)"'`
}

// ReuseEnv set to 0 or off turns off ssh connection reuse (ControlMaster) for every remote call.
const ReuseEnv = "HARNESS_SSH_REUSE"

// controlPathMax keeps dir/%C (40 hex) plus ssh's 17-byte temp suffix under macOS's 104-byte socket path limit.
const controlPathMax = 104 - 1 - 40 - 17

// controlDir is where reused ssh master sockets live, "" when none is usable; tests swap it.
var controlDir = defaultControlDir

// defaultControlDir picks the first short-enough private dir: the harness state dir, then ~/.ssh/cc.
func defaultControlDir() string {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(paths.StateDir(), "ssh"), filepath.Join(home, ".ssh", "cc")} {
		if home == "" || len(dir) > controlPathMax {
			continue
		}
		if os.MkdirAll(dir, 0o700) == nil && os.Chmod(dir, 0o700) == nil {
			return dir
		}
	}
	return ""
}

// reuseOpts multiplexes ssh calls to one host over a master kept 10 minutes; nil when opted out or no dir.
func reuseOpts() []string {
	if v := strings.ToLower(os.Getenv(ReuseEnv)); v == "0" || v == "off" {
		return nil
	}
	dir := controlDir()
	if dir == "" {
		return nil
	}
	return []string{"-o", "ControlMaster=auto", "-o", "ControlPersist=10m",
		"-o", "ControlPath=" + filepath.Join(dir, "%C")}
}

// sshArgv runs script on host: keepalive, reuse, batch forbids prompts, tty allocates one, "--" ends ssh options.
func sshArgv(host string, tty, batch bool, script string) ([]string, error) {
	if err := ValidHost(host); err != nil {
		return nil, err
	}
	argv := append([]string{"ssh", "-o", "ServerAliveInterval=15"}, reuseOpts()...)
	if batch {
		argv = append(argv, "-o", "BatchMode=yes")
	}
	if tty {
		argv = append(argv, "-t")
	}
	return append(argv, "--", host, encodeScript(script)), nil
}

// StreamArgs is the argv for running script on host with stdin left free for the caller; ssh may never prompt.
func StreamArgs(host, script string) ([]string, error) { return sshArgv(host, false, true, script) }
