package rtkwrap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

var forms sync.Map // binary, cwd and command -> model-facing form

// ForModel is cmd's `rtk rewrite` form (exit 3 too) for output a model reads; cmd when formSkipped or rtk declines.
func ForModel(ctx context.Context, cmd, cwd string) string {
	bin := paths.RTKBinary()
	if formSkipped(bin, cmd, cwd) {
		return cmd
	}
	key := bin + "\x00" + cwd + "\x00" + cmd
	if got, ok := forms.Load(key); ok {
		return got.(string)
	}
	out := rewriteForModel(ctx, bin, cwd, cmd)
	if _, same := paths.RTKOnPath(bin); out == "" || !same && BareCount(out) > BareCount(cmd) {
		out = cmd // a bare rtk PATH may not resolve to bin
	}
	forms.Store(key, out)
	return out
}

// formSkipped: code mode, no rtk, an empty or already-rtk command, a veto, or a PATH/HOME change keep cmd bare.
func formSkipped(bin, cmd, cwd string) bool {
	if CodeMode() || bin == "" || Vetoed(cmd, cwd) || EnvAltered(cmd) {
		return true
	}
	t := shell.Tokens(cmd)
	return len(t) == 0 || t[0] == rtkName
}

// ArgvForModel is [bin, argv...] when ForModel wraps argv's quoted line in exactly one leading rtk, else argv.
func ArgvForModel(ctx context.Context, argv []string, cwd string) []string {
	if len(argv) == 0 {
		return argv
	}
	line := quoteArgv(argv[0], argv[1:])
	if ForModel(ctx, line, cwd) != rtkName+" "+line {
		return argv
	}
	return append([]string{paths.RTKBinary()}, argv...)
}

// rewriteForModel is `rtk rewrite`'s output for cmd (exit 0 or 3), "" on any other result.
func rewriteForModel(ctx context.Context, bin, cwd, cmd string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "rewrite", "--", cmd)
	c.Dir, c.WaitDelay = cwd, waitDelay
	c.Env = append(os.Environ(), paths.RTKQuietEnv+"=1", paths.RTKTelemetryEnv+"=1")
	b, err := c.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 3) {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// bareWord matches `rtk` as a whole word not inside a path (after NAME=val, a wrapper word, or in a -c string).
var bareWord = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(^|[^\w./-])rtk($|[^\w.-])`) })

// BareCount is how many bare rtk words cmd holds; a rewrite that adds one needs PATH's rtk to be the pinned one.
func BareCount(cmd string) int { return len(bareWord().FindAllStringIndex(cmd, -1)) }
