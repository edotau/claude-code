package rtkwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

var forms sync.Map // formKey -> model-facing form; only answers rtk gave are kept

// ForModel is cmd's `rtk rewrite` form (exit 3 too) with every rtk pinned by path, so a child's PATH cannot swap it.
func ForModel(ctx context.Context, cmd, cwd string) string {
	bin := paths.RTKBinary()
	if CodeMode() || bin == "" || Vetoed(cmd, cwd) || EnvAltered(cmd) { // code mode, no rtk, a veto or a PATH/HOME change
		return cmd
	}
	t := shell.Tokens(cmd)
	if len(t) == 0 {
		return cmd
	}
	if t[0] == rtkName { // already wrapped: pin it so a child's PATH cannot swap the binary
		return PinRTK(cmd, textutil.QuoteSH(bin))
	}
	if out, decided := FastPath(cmd); decided {
		return pinnedForm(out, cmd, bin)
	}
	key := formKey(bin, cwd, cmd)
	if got, ok := forms.Load(key); ok {
		return got.(string)
	}
	out, ok := rewriteForModel(ctx, bin, cwd, cmd)
	if !ok {
		return cmd // a failure or timeout is retried on the next call
	}
	out = pinnedForm(out, cmd, bin)
	forms.Store(key, out)
	return out
}

// formKey changes with PATH, the rtk binary's identity and rtk's config file, so an edit to any re-asks rtk.
func formKey(bin, cwd, cmd string) string {
	return strings.Join([]string{bin, statKey(bin), statKey(paths.RTKConfig()), os.Getenv("PATH"), cwd, cmd}, "\x00")
}

func statKey(p string) string {
	fi, err := os.Stat(p)
	if p == "" || err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
}

// pinnedForm is rewritten with every command-position rtk spelled as bin; cmd when unchanged or a bare rtk remains.
func pinnedForm(rewritten, cmd, bin string) string {
	if rewritten == "" || rewritten == cmd {
		return cmd
	}
	pinned := PinRTK(rewritten, textutil.QuoteSH(bin))
	if BareCount(pinned) > BareCount(cmd) {
		return cmd
	}
	return pinned
}

// ArgvForModel is [bin, argv...] when ForModel wraps argv's quoted line in exactly one leading rtk, else argv.
func ArgvForModel(ctx context.Context, argv []string, cwd string) []string {
	if len(argv) == 0 {
		return argv
	}
	bin := paths.RTKBinary()
	line := quoteArgv(argv[0], argv[1:])
	if ForModel(ctx, line, cwd) != textutil.QuoteSH(bin)+" "+line {
		return argv
	}
	return append([]string{bin}, argv...)
}

// rewriteForModel is `rtk rewrite`'s output (exit 0 or 3; "" on 1 or 2); ok is false on any failure or timeout.
func rewriteForModel(ctx context.Context, bin, cwd, cmd string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "rewrite", "--", cmd)
	c.Dir, c.WaitDelay = cwd, waitDelay
	c.Env = append(os.Environ(), paths.RTKQuietEnv+"=1", paths.RTKTelemetryEnv+"=1")
	b, err := c.Output()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return "", false
	case err == nil || errors.As(err, &ee) && ee.ExitCode() == 3:
		return strings.TrimSpace(string(b)), true
	case ee != nil && (ee.ExitCode() == 1 || ee.ExitCode() == 2):
		return "", true // rtk's own "run as typed"
	}
	return "", false
}

// bareWord matches `rtk` as a whole word not inside a path (after NAME=val, a wrapper word, or in a -c string).
var bareWord = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(^|[^\w./-])rtk($|[^\w.-])`) })

// BareCount is how many bare rtk words cmd holds; a rewrite that adds one needs PATH's rtk to be the pinned one.
func BareCount(cmd string) int { return len(bareWord().FindAllStringIndex(cmd, -1)) }

// commandWord matches a bare or quoted `rtk` at command position (line start or after `&&`, `||`, `;`, `|`, `(`).
var commandWord = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|&&|\|\||[;|\n(\x60])(\s*)("rtk"|'rtk'|rtk)(\s|$)`)
})

// PinRTK points every command-position `rtk` in cmd at word, so the pinned binary always runs.
func PinRTK(cmd, word string) string {
	out := pinPass(cmd, word)
	if strings.Count(cmd, "rtk") < 2 { // a match eats its trailing separator, so only a second rtk needs a second pass
		return out
	}
	return pinPass(out, word)
}

// pinPass rewrites only matches whose prefix the shell runs; quoted text like '(rtk x)' stays as data.
func pinPass(cmd, word string) string {
	quote := shell.QuoteAt(cmd)
	var b strings.Builder
	last := 0
	for _, m := range commandWord().FindAllStringSubmatchIndex(cmd, -1) {
		if p := m[2]; p < m[3] && !prefixRuns(cmd, quote, p) {
			continue
		}
		b.WriteString(cmd[last:m[5]])
		b.WriteString(word)
		b.WriteString(cmd[m[8]:m[9]])
		last = m[1]
	}
	return b.String() + cmd[last:]
}

// prefixRuns reports whether the separator at cmd[p] is live shell syntax rather than quoted text.
func prefixRuns(cmd string, quote []byte, p int) bool {
	switch quote[p] {
	case 0:
		return true
	case '"':
		return cmd[p] == '`' || cmd[p] == '(' && p > 0 && cmd[p-1] == '$'
	}
	return false
}
