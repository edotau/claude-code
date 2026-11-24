package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	// piVerifyTimeout must end well before the 5-minute idle watch of the Pi run (a test pins it).
	piVerifyTimeout = 3 * time.Minute
	piVerifyShown   = 2000 // the most bytes of masked output the fixing turn carries
	piVerifyKeep    = 4 * 2048
	// piProxyKeyEnv is the proxy session token's env (internal/pi KeyRef): verify steps run edited code, so never it.
	piProxyKeyEnv = "HARNESS_PROXY_API_KEY"
)

// judgePiVerify rebuilds the run's verify line as argv steps and judges each as the bash line it is.
func judgePiVerify(c piCall) ([]plans.VerifyStep, *piVerdict) {
	line, _ := c.Input["verify"].(string)
	if len(c.Input) != 1 || line == "" {
		return nil, &piVerdict{Usage: "verify: the only argument is a non-empty verify line"}
	}
	steps, err := plans.VerifyArgv(line)
	if err == nil {
		err = plans.VerifyShape(line)
	}
	if err != nil {
		return nil, &piVerdict{Usage: "verify: " + err.Error()}
	}
	for _, s := range steps {
		if reason := piBashBlock(c, textutil.ShellJoin(append(slices.Clone(s.Env), s.Argv...))); reason != "" {
			return nil, &piVerdict{Block: reason}
		}
	}
	return steps, nil
}

// piVerifyEnv is the post-run verify's scrubbed env without the proxy token.
func piVerifyEnv() []string {
	kept, _ := proc.ScrubAgentCredentials(os.Environ(), nil, "")
	return slices.DeleteFunc(kept, func(kv string) bool { return strings.HasPrefix(kv, piProxyKeyEnv+"=") })
}

// runPiVerify judges, runs and redacts the verify line; alive's EOF means Pi is gone and kills the run.
func runPiVerify(c piCall, alive io.Reader) piVerdict {
	ctx, cancel := context.WithTimeout(context.Background(), piVerifyTimeout)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, alive); cancel() }()
	steps, bad := judgePiVerify(c)
	if bad != nil {
		return *bad
	}
	_, cause, _ := redact.SharedWithin(5 * time.Second) // fail closed: loaded before any output exists
	withheld := cause != ""
	env := piVerifyEnv()
	tail := &piTail{}
	var out io.WriteCloser = piDiscard{}
	if !withheld { // Writer reads Shared(), which a stalled vault load would block
		out = redact.Writer(tail)
	}
	start := time.Now()
	for _, s := range steps {
		code, err := runPiStep(ctx, c.CWD, append(slices.Clone(env), s.Env...), s, out)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		switch {
		case err != nil:
			_ = out.Close()
			return piVerdict{Usage: "verify before settle skipped: cannot launch " + s.Argv[0]}
		case ctx.Err() != nil && !timedOut:
			return piVerdict{Allow: true} // Pi is gone: nobody reads the reply
		case code != 0 || timedOut:
			_ = out.Close() // flushes the held tail into the window
			line, _ := c.Input["verify"].(string)
			return piVerdict{Allow: true, Text: piVerifyText(line, code, timedOut, time.Since(start), tail, withheld)}
		}
	}
	return piVerdict{Allow: true}
}

// piDiscard drops withheld output: with no redaction, none of it is kept.
type piDiscard struct{}

func (piDiscard) Write(p []byte) (int, error) { return len(p), nil }
func (piDiscard) Close() error                { return nil }

// runPiStep runs one step in its own process tree and returns its exit code (-1: signalled or never exited).
func runPiStep(ctx context.Context, dir string, env []string, s plans.VerifyStep, out io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, out
	cmd.WaitDelay = proc.ExecWaitDelay // a backgrounded child holding the pipe must not hang Wait
	tree, err := startStep(cmd)
	if err != nil {
		return -1, err
	}
	_ = cmd.Wait()
	tree.reap()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode(), nil
	}
	return -1, nil
}

// piVerifyText is the fixing turn's message; withheld output is never replaced by raw output, not even the line.
func piVerifyText(line string, code int, timedOut bool, elapsed time.Duration, t *piTail, withheld bool) string {
	what := fmt.Sprintf("failed (exit %d)", code)
	if timedOut {
		what = fmt.Sprintf("timed out after %d s", int(elapsed.Seconds()+0.5))
	}
	head := "The run's verify check " + what + ". Fix the cause without changing the check, then finish.\n"
	if withheld {
		return head + "verify output withheld: redaction unavailable"
	}
	window := strings.ToValidUTF8(string(t.b), "")
	if t.cut { // the window starts mid-output
		window = dropFirstLine(afterOrphanPEM(window))
	}
	body := piMask(window) // masked before the cut so no secret leaves a suffix
	return head + "$ " + piMask(line) + "\n" + strings.ToValidUTF8(body[max(0, len(body)-piVerifyShown):], "")
}

// piMask masks the proxy token, credential shapes and vault values in s.
func piMask(s string) string {
	if key := os.Getenv(piProxyKeyEnv); key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	return redact.Text(textutil.RedactSecrets(s))
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

// dropFirstLine is s past its first newline: the cut window's first line may be a secret's tail without its marker.
func dropFirstLine(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	return rest
}

// piTail keeps the last piVerifyKeep to 2*piVerifyKeep raw bytes; masking happens once, over all of them.
type piTail struct {
	b   []byte
	cut bool // the window's front was dropped
}

func (t *piTail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*piVerifyKeep {
		t.b, t.cut = append(t.b[:0], t.b[len(t.b)-piVerifyKeep:]...), true
	}
	return len(p), nil
}
