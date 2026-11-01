// Package remote builds the ssh argv that runs a script on another host with stdin left free for the caller.
package remote

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// ValidHost refuses an ssh destination that ssh would parse as an option or that carries whitespace/control bytes.
func ValidHost(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

// EncodeScript wraps a POSIX sh script as a `sh -c` command: the script rides base64 (A-Za-z0-9+/=), so sh, bash
// and fish parse the wrapper identically whatever bytes the script holds.
func EncodeScript(script string) string {
	return `sh -c 'eval "$(printf %s ` + base64.StdEncoding.EncodeToString([]byte(script)) + ` | base64 -d)"'`
}

// StreamArgs is the argv for running script on host with stdin left free for the caller: BatchMode forbids prompts,
// ServerAliveInterval drops a dead link, and "--" stops host from being read as an ssh option.
func StreamArgs(host, script string) ([]string, error) {
	if err := ValidHost(host); err != nil {
		return nil, err
	}
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "--", host, EncodeScript(script)}, nil
}
