package agent

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	verifyTimeout = 10 * time.Minute
	verifyTailMax = 2048
)

// verified adds the verify result to an answered run before its result event; a failed verify keeps res.Status (D3).
func (p *Prepared) verified(ctx context.Context, res wire.Result, err error, notify func(string)) wire.Result {
	cmd := p.Opts.Verify
	if cmd == "" || err != nil || res.Status != wire.StatusOK || res.Verify != nil {
		return res
	}
	res.Verify = VerifyRun(ctx, p.Opts, p.remote != nil)
	if notify != nil {
		notify(res.Verify.Line())
	}
	return res
}

// VerifyRun is the one place a spec's verify runs: opts.Verify in opts.WorkDir, or not run when the work is remote.
func VerifyRun(ctx context.Context, opts RunOptions, remote bool) *wire.VerifyResult {
	if remote { // the work is on another host's tree: running here would check the wrong files
		return &wire.VerifyResult{Command: maskShapes(opts.Verify), Exit: -1, Reason: wire.VerifyNotRunRemote}
	}
	ctx = valuesContext(ctx, opts.Values)
	env := slices.DeleteFunc(runEnviron(ctx), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(opts.Unset, name)
	})
	env = withRTKQuiet(append(env, opts.Env...))
	env, _ = proc.ScrubAgentCredentials(env, keepSet(ctx, opts.KeepEnv), "") // go test runs edits
	return runVerify(ctx, opts.WorkDir, opts.Verify, env)
}

// runVerify runs each && step of verify as argv in workdir with env, stopping at the first failure.
func runVerify(ctx context.Context, workdir, verify string, env []string) *wire.VerifyResult {
	start := time.Now()
	vr := &wire.VerifyResult{Command: maskShapes(verify)}
	steps, err := plans.VerifyArgv(verify)
	if err == nil {
		err = plans.VerifyShape(verify)
	}
	if err != nil {
		vr.Exit, vr.Reason = -1, wire.VerifyNotRunShape
		return vr
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	tail := &tailBuf{}
	out := redact.Writer(tail) // vault values are masked while streaming
	for _, s := range steps {
		cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
		cmd.WaitDelay = proc.ExecWaitDelay
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = workdir, append(slices.Clone(env), s.Env...), out, out
		proc.TreeCancel(cmd, proc.ExecWaitDelay) // after cmd.Env: it tags the env, so a timeout kills descendants too
		if err := cmd.Run(); err != nil {
			vr.Exit = exitCode(err, out)
			break
		}
	}
	_ = out.Close()
	vr.Tail = verifyTail(tail)
	vr.Ms = time.Since(start).Milliseconds()
	return vr
}

// verifyTail is the last verifyTailMax bytes of the window, masked before the cut so no secret leaves a suffix.
func verifyTail(t *tailBuf) string {
	s := strings.ToValidUTF8(string(t.b), "")
	if t.cut { // the window starts mid-output
		s = dropFirstLine(afterOrphanPEM(s))
	}
	masked := maskShapes(s)
	return strings.ToValidUTF8(masked[max(0, len(masked)-verifyTailMax):], "")
}

// afterOrphanPEM is s past the END line of a PEM block whose BEGIN the cut window lost.
func afterOrphanPEM(s string) string {
	end := strings.Index(s, "-----END ")
	if end < 0 || strings.Contains(s[:end], "-----BEGIN ") {
		return s
	}
	if nl := strings.IndexByte(s[end:], '\n'); nl >= 0 {
		return s[end+nl:]
	}
	return ""
}

// dropFirstLine is s from the first line past the cut that is not key-shaped: a cut window may open inside a key.
func dropFirstLine(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	for header := false; rest != ""; {
		line, next, _ := strings.Cut(rest, "\n")
		line = strings.TrimSpace(line)
		isHeader := strings.HasPrefix(line, "Proc-Type:") || strings.HasPrefix(line, "DEK-Info:")
		if !isHeader && !(line == "" && header) && !base64Line(line) {
			break
		}
		header, rest = isHeader || (line == "" && header), next
	}
	return rest
}

const b64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

// base64Line reports a line of only base64 characters (no space or other punctuation).
func base64Line(s string) bool { return s != "" && strings.Trim(s, b64Chars) == "" }

// maskShapes masks credential-shaped runs, then vault values, in s.
func maskShapes(s string) string { return redact.Text(textutil.RedactSecrets(s)) }

// verifyKeep is the raw window kept: more than the tail plus a 4096-bit PEM key (about 3.3 KiB).
const verifyKeep = 4 * verifyTailMax

// tailBuf keeps the last verifyKeep to 2*verifyKeep raw bytes; masking happens once, in verifyTail, over all of them.
type tailBuf struct {
	b   []byte
	cut bool // the window's front was dropped
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*verifyKeep {
		t.b, t.cut = append(t.b[:0], t.b[len(t.b)-verifyKeep:]...), true
	}
	return len(p), nil
}

// exitCode is the child's exit status, or -1 with the start error written to w.
func exitCode(err error, w interface{ Write([]byte) (int, error) }) int {
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return ee.ExitCode()
	}
	_, _ = w.Write([]byte(err.Error()))
	return -1
}
