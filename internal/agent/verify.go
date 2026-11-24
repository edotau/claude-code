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
	vr.Tail = verifyTail(tail.b)
	vr.Ms = time.Since(start).Milliseconds()
	return vr
}

// verifyTail is the last verifyTailMax bytes of window, masked before the cut so a straddling secret leaves no suffix.
func verifyTail(window []byte) string {
	masked := maskShapes(strings.ToValidUTF8(string(window), ""))
	return strings.ToValidUTF8(masked[max(0, len(masked)-verifyTailMax):], "")
}

// maskShapes masks credential-shaped runs, then vault values, in s.
func maskShapes(s string) string { return redact.Text(textutil.RedactSecrets(s)) }

// tailBuf keeps a window of the last output; shapes are masked over the whole window before its front is dropped.
type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4*verifyTailMax {
		masked := textutil.RedactSecrets(string(t.b)) // an unfinished secret at the end stays whole for the next pass
		t.b = []byte(masked[max(0, len(masked)-2*verifyTailMax):])
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
