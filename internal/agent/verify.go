package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
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
		return &wire.VerifyResult{Command: redact.Text(opts.Verify), Exit: -1, Reason: wire.VerifyNotRunRemote}
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(opts.Unset, name)
	})
	env, _ = proc.ScrubAgentCredentials(env, keepSet(ctx, opts.KeepEnv), "") // go test runs the agent's edits
	return runVerify(ctx, opts.WorkDir, opts.Verify, append(env, opts.Env...))
}

// runVerify runs each && step of verify as argv in workdir with env, stopping at the first failure.
func runVerify(ctx context.Context, workdir, verify string, env []string) *wire.VerifyResult {
	start := time.Now()
	vr := &wire.VerifyResult{Command: redact.Text(verify)}
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
	out := redact.Writer(tail) // masks while reading, so the cut below never splits a secret
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
	vr.Tail = strings.ToValidUTF8(string(tail.b[max(0, len(tail.b)-verifyTailMax):]), "")
	vr.Ms = time.Since(start).Milliseconds()
	return vr
}

// tailBuf keeps only the last verifyTailMax bytes written to it.
type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*verifyTailMax {
		t.b = append(t.b[:0], t.b[len(t.b)-verifyTailMax:]...)
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
