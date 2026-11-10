// Package remote builds the ssh argv that runs a script on another host with stdin left free for the caller.
package remote

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BinEnv names the remote claude-code binary when --remote-bin is absent; the fallback is "claude-code" on the remote PATH.
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
	if rel, err := filepath.Rel(home, p); err == nil && filepath.IsAbs(p) && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.Join("~", rel)
	}
	return p
}

// ValidHost refuses an ssh destination that ssh would parse as an option or that carries whitespace/control bytes.
func ValidHost(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

// encodeScript wraps a POSIX sh script as a `sh -c` command: the script rides base64 (A-Za-z0-9+/=), so sh, bash
// and fish parse the wrapper identically whatever bytes the script holds.
func encodeScript(script string) string {
	return `sh -c 'eval "$(printf %s ` + base64.StdEncoding.EncodeToString([]byte(script)) + ` | base64 -d)"'`
}

// Argv is the ssh argv running script on host: ServerAliveInterval drops a dead link, batch forbids prompts, tty
// allocates a terminal, and "--" stops host from being read as an ssh option.
func Argv(host string, tty, batch bool, script string) ([]string, error) {
	if err := ValidHost(host); err != nil {
		return nil, err
	}
	argv := []string{"ssh", "-o", "ServerAliveInterval=15"}
	if batch {
		argv = append(argv, "-o", "BatchMode=yes")
	}
	if tty {
		argv = append(argv, "-t")
	}
	return append(argv, "--", host, encodeScript(script)), nil
}

// StreamArgs is the argv for running script on host with stdin left free for the caller; ssh may never prompt.
func StreamArgs(host, script string) ([]string, error) { return Argv(host, false, true, script) }
