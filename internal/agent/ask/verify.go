package ask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
)

const (
	verifyTimeout = 10 * time.Minute
	verifyTailMax = 2048
)

// verifyKey carries the spec's verify command to askPrepare.
type verifyKey struct{}

func withVerify(ctx context.Context, verify string) context.Context {
	return context.WithValue(ctx, verifyKey{}, verify)
}

// runAskDispatch is runAskAndReport, plus the spec's verify run after an answered dispatch; the status is unchanged.
func runAskDispatch(ctx context.Context, cl *agent.Client, p *agent.Prepared, workdir string, isJob bool,
	format, save string, out io.Writer) int {
	verify, _ := ctx.Value(verifyKey{}).(string)
	if verify == "" {
		return runAskAndReport(ctx, cl, p, isJob, format, save, out)
	}
	res, err := cl.Exec(ctx, p, sinkTo(sinkOut(out, format, save), format, p.Record.ID), jobOpts(isJob, format, out))
	if err == nil && res.Status == wire.StatusOK {
		res.Verify = runVerify(ctx, workdir, verify)
		fmt.Fprintf(os.Stderr, "ask: verify: exit %d (%s)\n", res.Verify.Exit, res.Verify.Command)
	}
	return reportAsk(out, format, save, p, res, err)
}

// runVerify runs each && step of verify as argv in workdir, stopping at the first failure.
func runVerify(ctx context.Context, workdir, verify string) *wire.VerifyResult {
	start := time.Now()
	vr := &wire.VerifyResult{Command: redact.Text(verify)}
	steps, err := plans.VerifyArgv(verify)
	if err == nil {
		err = plans.VerifyShape(verify)
	}
	if err != nil {
		vr.Exit, vr.Tail = -1, err.Error()
		return vr
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	buf := &tailBuf{}
	for _, s := range steps {
		cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
		cmd.WaitDelay = proc.ExecWaitDelay
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = workdir, append(os.Environ(), s.Env...), buf, buf
		if err := cmd.Run(); err != nil {
			vr.Exit = exitCode(err, buf)
			break
		}
	}
	masked := redact.Text(strings.ToValidUTF8(string(buf.b), "")) // before the cut, so a trimmed secret is not left half
	vr.Tail = masked[max(0, len(masked)-verifyTailMax):]
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

// exitCode is the child's exit status, or -1 with the start error written to buf.
func exitCode(err error, buf *tailBuf) int {
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return ee.ExitCode()
	}
	_, _ = buf.Write([]byte(err.Error()))
	return -1
}
