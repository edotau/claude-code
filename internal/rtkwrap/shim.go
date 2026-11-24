package rtkwrap

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
	codemode       string // CodemodeEnv: Pi's scripts run, so nothing wraps
	pipe           bool   // stdout is a pipe or socket, never a TTY, file or device
	pid, ppid, sid int
	cwd            string
}

// gate is the Pi tool-shell command c runs within and "" to wrap it, else the reason code that keeps c raw.
func (c call) gate() (cmd, reason string) {
	if c.codemode != "" {
		return "", "codemode"
	}
	if !c.pipe {
		return "", "not-pipe"
	}
	if c.sid == c.pid && procComm(c.ppid) == piComm { // the shell exec'd its tail: the rest of the line is gone
		return "", "line-unseen"
	}
	cmd, ok := c.toolCommand()
	if !ok {
		return "", "not-tool-shell"
	}
	if reason := lineRaw(cmd, c.name); reason != "" {
		return "", reason
	}
	return cmd, c.veto(cmd)
}

// lineRaw is the reason the tool line keeps name raw, else "".
func lineRaw(cmd, name string) string {
	switch {
	case strings.Contains(cmd, ">&") || strings.Contains(cmd, "<&"): // `exec >&2`: a pipe stdout may not reach the model
		return "fd-redirect"
	case feedsProgram(cmd, name):
		return "pipeline-output"
	case substitutes(cmd):
		return "substitution"
	case sourced(cmd):
		return "source-in-line"
	case EnvAltered(cmd):
		return "env-altered"
	}
	return ""
}

// veto is the "veto:<rule>" code when the tool line or this call's own argv trips one, else "".
func (c call) veto(cmd string) string {
	rule := cmp.Or(VetoRule(cmd, c.cwd), VetoRule(quoteArgv(c.name, c.args), c.cwd))
	if c.name == "git" && gitRTKSubcommand(c.args) == "commit" {
		rule = cmp.Or(rule, "commit")
	}
	if rule == "" {
		return ""
	}
	return "veto:" + rule
}

// toolCommand is the command line when c is the tool shell's direct child; $(...) and make never are.
func (c call) toolCommand() (string, bool) {
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
	_, subs := shell.ExtractSubstitutions(cmd)
	return len(subs) > 0
}

// sourced reports a segment running source, `.` or eval: it runs more commands in the tool shell, unseen by the line.
func sourced(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		rest, _ := shell.Unwrap(shell.Tokens(seg), "")
		if len(rest) > 0 && slices.Contains([]string{"source", ".", "eval"}, rest[0]) {
			return true
		}
	}
	return false
}

// feedsProgram reports name piping into a program, or any pipe fed by a compound group (its members are unattributed).
func feedsProgram(cmd, name string) bool {
	segs, seps := shell.SegmentsSep(cmd)
	for i, seg := range segs {
		if seps[i] != "|" && seps[i] != "|&" {
			continue
		}
		if f := strings.Fields(seg); len(f) > 0 && endsCompound(f[len(f)-1]) {
			return true
		}
		rest, _ := shell.Unwrap(shell.Tokens(seg), "")
		if len(rest) > 0 && filepath.Base(rest[0]) == name {
			return true
		}
	}
	return false
}

func endsCompound(word string) bool {
	return strings.HasSuffix(word, ")") || slices.Contains([]string{"}", "done", "fi", "esac"}, word)
}

// rewriteWraps reports whether rtk's rewrite puts this very invocation behind rtk and leaves no name segment bare.
func rewriteWraps(rewritten, name string, args []string) bool {
	wrapped := false
	own := shell.Tokens(quoteArgv(name, args))
	segs, seps := shell.SegmentsSep(rewritten)
	for i, seg := range segs {
		t := shell.Tokens(seg)
		if len(t) > 1 && t[0] == "rtk" && t[1] == name {
			if seps[i] == "|" || seps[i] == "|&" {
				return false
			}
			wrapped = wrapped || slices.Equal(t[1:], own)
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
	c := exec.CommandContext(ctx, rtk, "rewrite", "--", cmd)
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
	if name == rtkName {
		return rtkMain(args)
	}
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

// wrapWith is the rtk to exec for this call, or "" when it runs raw; the decision is logged first.
func wrapWith(name string, args, dirs []string, self os.FileInfo, env []string) string {
	cwd, _ := os.Getwd()
	c := call{name: name, args: args, codemode: os.Getenv(CodemodeEnv), pipe: stdoutPipe(), pid: os.Getpid(), ppid: os.Getppid(), sid: sessionID(), cwd: cwd}
	rtk := rtkBinary(dirs, self)
	reason := c.wraps(rtk, env)
	logDecision(name, reason, args)
	if reason != "" {
		return ""
	}
	return rtk
}

// wraps is "" when every gate passes and rtk's rewrite of the tool line wraps this call, else the reason it stays raw.
func (c call) wraps(rtk string, env []string) string {
	cmd, reason := c.gate()
	switch {
	case reason != "":
		return reason
	case rtk == "":
		return "rtk-missing"
	case !rewriteWraps(rtkRewrite(rtk, cmd, env), c.name, c.args):
		return "rtk-declined"
	}
	return ""
}

func stdoutPipe() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0
}

// selfInfo is this executable's file (symlinks followed), else the installed shim's, so resolve never picks a shim.
func selfInfo() os.FileInfo {
	exe, err := os.Executable()
	if err != nil {
		exe = Binary()
	}
	fi, err := os.Stat(exe)
	if err != nil {
		fi, _ = os.Stat(Binary())
	}
	return fi
}

// resolve is the first executable named name on dirs that is not the shim, and dirs minus Dir and every shim dir.
func resolve(dirs []string, name string, self os.FileInfo) (real string, kept []string) {
	for _, d := range withoutDir(dirs, Dir()) {
		fi, err := os.Stat(filepath.Join(d, name)) // a relative entry resolves against the cwd, as the shell does
		if err == nil && self != nil && os.SameFile(fi, self) {
			continue // another spelling of a shim dir: drop it so nothing recurses
		}
		if real == "" && err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			real = filepath.Join(d, name)
		}
		kept = append(kept, d)
	}
	return real, kept
}

// rtkBinary is the real rtk the hook would run (HARNESS_RTK_BIN, else the pinned install, else dirs), never the shim.
func rtkBinary(dirs []string, self os.FileInfo) string {
	candidates := []string{paths.RTKPinned()}
	if env := os.Getenv(paths.RTKBinaryEnv); env != "" {
		candidates = []string{env}
	}
	for _, name := range candidates {
		p, err := exec.LookPath(name)
		if fi, serr := os.Stat(p); err == nil && serr == nil && (self == nil || !os.SameFile(fi, self)) {
			return p
		}
	}
	rtk, _ := resolve(dirs, rtkName, self)
	return rtk
}

// withPath is env with PATH set to dirs, so the command's own children never meet a shim.
func withPath(env, dirs []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	return append(out, "PATH="+strings.Join(dirs, string(os.PathListSeparator)))
}

const rtkName = "rtk"

// stripRtk is the command `rtk args...` becomes under code mode: a shim command, or `bypass <cmd>`; ok false runs rtk.
func stripRtk(args []string) (name string, rest []string, ok bool) {
	switch {
	case len(args) == 0:
		return "", nil, false
	case args[0] == "bypass" && len(args) > 1:
		return args[1], args[2:], true
	case slices.Contains(Names, args[0]):
		return args[0], args[1:], true
	}
	return "", nil, false
}

// rtkMain is the rtk stand-in: under code mode it drops the rtk prefix from a shim command, else it runs the real rtk.
func rtkMain(args []string) int {
	self := selfInfo()
	_, dirs := resolve(filepath.SplitList(os.Getenv("PATH")), rtkName, self)
	real := rtkBinary(dirs, self)
	env := withPath(os.Environ(), dirs)
	name, rest, strip := stripRtk(args[1:])
	if os.Getenv(CodemodeEnv) == "" {
		strip = false
	}
	if strip {
		cmd, _ := resolve(dirs, name, self)
		logVerdict(filepath.Base(name), "stripped", "", rest)
		if cmd == "" {
			fmt.Fprintf(os.Stderr, "rtkwrap: %s: not found on PATH outside the shim dir\n", name)
			return exitNoExec
		}
		err := execve(cmd, append([]string{name}, rest...), env)
		fmt.Fprintf(os.Stderr, "rtkwrap: %s: %v\n", name, err)
		return exitNoExec
	}
	if os.Getenv(CodemodeEnv) != "" {
		logVerdict(rtkName, "raw", "rtk-verb", args[1:])
	}
	if real == "" {
		fmt.Fprintf(os.Stderr, "rtkwrap: rtk: not found on PATH outside the shim dir\n")
		return exitNoExec
	}
	err := execve(real, append([]string{"rtk"}, args[1:]...), env)
	fmt.Fprintf(os.Stderr, "rtkwrap: rtk: %v\n", err)
	return exitNoExec
}
