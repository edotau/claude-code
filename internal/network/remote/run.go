package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// Seams: tests swap the process runner and the terminal check.
var (
	runCommand = func(c *exec.Cmd) error { return c.Run() }
	stdinTTY   = func() bool { return ansi.IsTerminal(os.Stdin) }
)

// RunVerb is `claude-code --host <host> <verb …>`: the verb runs on host over ssh; a browser login opens on that host.
func RunVerb(args []string) int {
	host, rest, err := splitHost(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code:", err)
		return 2
	}
	cmd := exec.CommandContext(context.Background(), "ssh", hostArgv(host, rest, stdinTTY())[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = runCommand(cmd)
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: --host:", err)
		return 1
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
	if err := ValidHost(host); err != nil {
		return "", nil, fmt.Errorf("--host: %w", err)
	}
	if len(rest) == 0 {
		return "", nil, fmt.Errorf("--host %s needs a command: claude-code --host %s <command> [args]", host, host)
	}
	return host, rest, nil
}

// hostArgv is the ssh argv: -t for an interactive caller, then the encoded script running the verb.
func hostArgv(host string, args []string, tty bool) []string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = textutil.ShellQuote(a)
	}
	script := PathPrefix + " exec " + textutil.QuotePath(HomePath(Bin(""))) + " " + strings.Join(quoted, " ")
	argv := []string{"ssh", "-o", "ServerAliveInterval=15"}
	if tty {
		argv = append(argv, "-t")
	}
	return append(argv, "--", host, encodeScript(script))
}

// wslExplorer opens a URL in the Windows default browser; absolute because ssh sessions lack the Windows PATH.
const wslExplorer = "/mnt/c/Windows/explorer.exe"

// goos is runtime.GOOS; a test swaps it to reach the Linux and WSL openers from macOS.
var goos = runtime.GOOS

// isWSL reports a WSL guest with Windows interop; a test swaps it.
var isWSL = func() bool {
	m, _ := filepath.Glob("/proc/sys/fs/binfmt_misc/WSLInterop*")
	return len(m) > 0
}

// OpenURL starts the desktop opener without waiting: explorer.exe exits 1 even when it opens the page.
func OpenURL(url string) error {
	return exec.CommandContext(context.Background(), opener(), url).Start() // the opener outlives this call
}

// opener is the URL-opening binary for this machine: wslview when installed on WSL, else explorer.exe there.
func opener() string {
	switch {
	case goos == "darwin":
		return "open"
	case isWSL():
		if p, err := exec.LookPath("wslview"); err == nil {
			return p
		}
		return wslExplorer
	}
	return "xdg-open"
}
