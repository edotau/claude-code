package rtkwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// piComm is the process name Pi sets (process.title), as proc.Comm reads it on macOS and Linux.
const piComm = "pi"

// toolShells are the shells Pi's bash tool may spawn as `<shell> -c <command>`.
var toolShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// Process-table and rtk seams; tests stub them.
var (
	procComm   = proc.Comm
	procParent = proc.Parent
	procArgv   = argvOf
	rtkRewrite = rewrite
)

// rewriteTimeout caps `rtk rewrite`; past it the command runs raw.
var rewriteTimeout = 2 * time.Second

const (
	waitDelay  = 200 * time.Millisecond // a grandchild holding rewrite's stdout must not hold the shim
	exitNoExec = 127                    // the shell's "command not found" status
)

// call is one shim invocation and the facts its wrap decision reads.
type call struct {
	name           string
	args           []string
	mode           string
	pipe           bool // stdout is a pipe or socket, never a TTY, file or device
	pid, ppid, sid int
	cwd            string
}

// command is the Pi tool-shell command c runs within, or false when any gate keeps c raw.
func (c call) command() (string, bool) {
	if c.mode != "on" || !c.pipe {
		return "", false
	}
	cmd, ok := c.toolCommand()
	if !ok || substitutes(cmd) || EnvAltered(cmd) || Vetoed(cmd, c.cwd) {
		return "", false
	}
	return cmd, c.name != "git" || gitRTKSubcommand(c.args) != "commit"
}

// toolCommand is the command line when c is the tool shell itself or its direct child; $(...) and make never are.
func (c call) toolCommand() (string, bool) {
	if c.sid == c.pid && procComm(c.ppid) == piComm { // `bash -c` exec'd its last command: c is the tool shell
		return quoteArgv(c.name, c.args), true
	}
	if c.sid != c.ppid || !toolShells[procComm(c.ppid)] || procComm(procParent(c.ppid)) != piComm {
		return "", false
	}
	argv := procArgv(c.ppid)
	if len(argv) != 3 || argv[1] != "-c" {
		return "", false
	}
	return argv[2], true
}

func quoteArgv(name string, args []string) string {
	words := []string{name}
	for _, a := range args {
		words = append(words, textutil.QuoteSH(a))
	}
	return strings.Join(words, " ")
}

// substitutes reports command or process substitution anywhere in cmd: its output feeds the shell, not the model.
func substitutes(cmd string) bool {
	return strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") || strings.Contains(cmd, "<(") || strings.Contains(cmd, ">(")
}

// rewriteWraps reports whether rtk's rewrite puts every name segment behind rtk and leaves none bare.
func rewriteWraps(rewritten, name string) bool {
	wrapped := false
	for _, seg := range shell.Segments(rewritten) {
		t := shell.Tokens(seg)
		if len(t) > 1 && t[0] == "rtk" && t[1] == name {
			wrapped = true
			continue
		}
		if rest, _ := shell.Unwrap(t, ""); len(rest) > 0 && filepath.Base(rest[0]) == name {
			return false
		}
	}
	return wrapped
}

// rewrite is `rtk rewrite cmd` (exit 0 or 3); "" on any failure.
func rewrite(rtk, cmd string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), rewriteTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, rtk, "rewrite", cmd)
	c.Env = env
	c.WaitDelay = waitDelay
	out, err := c.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 3) {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Main runs the shim args[0] names: rtk when every gate passes, else the real binary; it returns only on failure.
func Main(args []string) int {
	name := filepath.Base(args[0])
	if !slices.Contains(Names, name) {
		fmt.Fprintf(os.Stderr, "rtkwrap: %s is not a shimmed command\n", name)
		return exitNoExec
	}
	self := selfInfo()
	real, dirs := resolve(filepath.SplitList(os.Getenv("PATH")), name, self)
	env := withPath(os.Environ(), dirs)
	if real == "" {
		fmt.Fprintf(os.Stderr, "rtkwrap: %s: not found on PATH outside the shim dir\n", name)
		return exitNoExec
	}
	if rtk := wrapWith(name, args[1:], dirs, self, env); rtk != "" {
		_ = execve(rtk, append([]string{"rtk", name}, args[1:]...), env) // returns only on failure: run raw
	}
	err := execve(real, append([]string{args[0]}, args[1:]...), env)
	fmt.Fprintf(os.Stderr, "rtkwrap: %s: %v\n", name, err)
	return exitNoExec
}

// wrapWith is the rtk to exec for this call, or "" when it runs raw.
func wrapWith(name string, args, dirs []string, self os.FileInfo, env []string) string {
	cwd, _ := os.Getwd()
	pid := os.Getpid()
	c := call{name: name, args: args, mode: os.Getenv(ModeEnv), pipe: stdoutPipe(), pid: pid, ppid: os.Getppid(), sid: sessionID(), cwd: cwd}
	cmd, ok := c.command()
	if !ok {
		return ""
	}
	rtk, _ := resolve(dirs, "rtk", self)
	if rtk == "" || !rewriteWraps(rtkRewrite(rtk, cmd, env), name) {
		return ""
	}
	return rtk
}

func stdoutPipe() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0
}

// selfInfo is this executable's file (symlinks followed); nil when unknown.
func selfInfo() os.FileInfo {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	return fi
}

// resolve is the first executable named name on dirs that is not the shim, and dirs minus Dir and each shim dir met.
func resolve(dirs []string, name string, self os.FileInfo) (real string, kept []string) {
	for _, d := range withoutDir(dirs, Dir()) {
		// a relative PATH entry depends on the cwd: never resolve a real binary through it
		if real == "" && filepath.IsAbs(d) {
			fi, err := os.Stat(filepath.Join(d, name))
			if err == nil && self != nil && os.SameFile(fi, self) {
				continue // another spelling of a shim dir: drop it so nothing recurses
			}
			if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				real = filepath.Join(d, name)
			}
		}
		kept = append(kept, d)
	}
	return real, kept
}

// withPath is env with PATH set to dirs, so the command's own children never meet a shim.
func withPath(env, dirs []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	return append(out, "PATH="+strings.Join(dirs, string(os.PathListSeparator)))
}
