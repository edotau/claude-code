// Package remote builds the ssh argv that runs a script on another host with stdin left free for the caller.
package remote

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
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

// encodeScript wraps script as `sh -c` with a base64 body, so sh, bash and fish parse it identically.
func encodeScript(script string) string {
	return `sh -c 'eval "$(printf %s ` + base64.StdEncoding.EncodeToString([]byte(script)) + ` | base64 -d)"'`
}

// reuseEnv set to 0 or off turns off ssh connection reuse (ControlMaster) for every remote call.
const reuseEnv = "HARNESS_SSH_REUSE"

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
	if v := strings.ToLower(os.Getenv(reuseEnv)); v == "0" || v == "off" {
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
	if err := textutil.ValidHost(host); err != nil {
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

// Seams: tests swap the process runner and the terminal check.
var (
	runCommand = func(c *exec.Cmd) error { return c.Run() }
	stdinTTY   = func() bool { return proc.IsTerminal(os.Stdin) }
)

// Run is `claude-code --host <host> <verb …>`: the verb runs on host over ssh; a browser login opens on that host.
func Run(args []string) int {
	host, rest, err := splitHost(args)
	if err != nil {
		return helptext.FailUsage("%v", err)
	}
	argv, err := hostArgv(host, rest, stdinTTY())
	if err != nil {
		return helptext.FailUsage("--host: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), "ssh", argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = runCommand(cmd)
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}
	if err != nil {
		return helptext.Fail("--host: %v", err)
	}
	return 0
}

// splitHost takes `--host H` or `--host=H` off the front; the rest is the remote verb and its args.
func splitHost(args []string) (string, []string, error) {
	if len(args) == 0 || args[0] == "--host" && len(args) == 1 {
		return "", nil, errors.New("--host needs a host and a command: claude-code --host <host> <command> [args]")
	}
	host, rest := "", args[1:]
	if h, ok := strings.CutPrefix(args[0], "--host="); ok {
		host = h
	} else if args[0] == "--host" {
		host, rest = args[1], args[2:]
	}
	if err := textutil.ValidHost(host); err != nil {
		return "", nil, fmt.Errorf("--host: %w", err)
	}
	if len(rest) == 0 {
		return "", nil, fmt.Errorf("--host %s needs a command: claude-code --host %s <command> [args]", host, host)
	}
	return host, rest, nil
}

// hostArgv is the ssh argv: -t for an interactive caller, then the encoded script running the verb.
func hostArgv(host string, args []string, tty bool) ([]string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = textutil.ShellQuote(a)
	}
	script := PathPrefix + " exec " + textutil.QuotePath(HomePath(Bin(""))) + " " + strings.Join(quoted, " ")
	return sshArgv(host, tty, false, script)
}
