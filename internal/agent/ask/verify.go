package ask

import (
	"bytes"
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
	vr := &wire.VerifyResult{Command: verify}
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
	var buf bytes.Buffer
	for _, s := range steps {
		cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = workdir, append(os.Environ(), s.Env...), &buf, &buf
		if err := cmd.Run(); err != nil {
			vr.Exit = exitCode(err, &buf)
			break
		}
	}
	tail := buf.Bytes()
	vr.Tail = redact.Text(strings.ToValidUTF8(string(tail[max(0, len(tail)-verifyTailMax):]), ""))
	vr.Ms = time.Since(start).Milliseconds()
	return vr
}

// exitCode is the child's exit status, or -1 with the start error written to buf.
func exitCode(err error, buf *bytes.Buffer) int {
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return ee.ExitCode()
	}
	buf.WriteString(err.Error())
	return -1
}
