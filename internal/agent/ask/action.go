package ask

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// localAgent is the agent name of a no-model action's record and envelope.
const localAgent = "local"

// actionArgv is the verb argv for the spec's action; Go builds it from typed fields, never from free text.
func actionArgv(action string, own []string, agentName, model string) []string {
	scope := func(flag string) []string {
		if len(own) == 0 {
			return nil
		}
		return []string{flag, strings.Join(own, ",")}
	}
	switch action {
	case "gate":
		return append([]string{"worktree", "gate", "--run"}, scope("--scope")...)
	case "scope":
		return append([]string{"worktree", "gate", "--format", "json"}, scope("--scope")...)
	case "diff":
		argv := []string{"agents", "diff", "--format", "json"}
		for _, p := range own {
			argv = append(argv, "--path", p)
		}
		return argv
	case "gateway":
		argv := []string{"gateway", "--format", "json"}
		if agentName != "" {
			argv = append(argv, "--agent", agentName)
		}
		if model != "" {
			argv = append(argv, "--model", model)
		}
		return argv
	}
	return nil
}

// actionOutCap bounds the child output kept in the envelope and the log.
const actionOutCap = 1 << 20

// capBuffer keeps the first actionOutCap bytes and notes the cut.
type capBuffer struct {
	b   bytes.Buffer // named, so exec's io.Copy cannot reach bytes.Buffer.ReadFrom around the cap
	cut bool
}

func (b *capBuffer) Bytes() []byte  { return b.b.Bytes() }
func (b *capBuffer) String() string { return b.b.String() }

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := actionOutCap - b.b.Len(); len(p) > room {
		b.b.Write(p[:max(room, 0)])
		b.cut = true
		return len(p), nil
	}
	return b.b.Write(p)
}

// actionRefusal names the run-shaping flag an action cannot honor, else "".
func actionRefusal(f *askFlags) string {
	switch {
	case f.dryRun:
		return "--dry-run"
	case f.host != "", f.batchSrc != "", f.live, f.loop, f.background, f.carry, f.waves != "", f.retry != "", f.job != "":
		return "--host, --batch, --live, --loop, --background, --carry, --waves, --retry and --job"
	case f.tier != "" || f.effort != "" || f.overlay != "" || f.session != "":
		return "--tier, --effort, --overlay and --session"
	case f.verify != "" || f.outputSchema != "" || f.planTask != "" || f.save != "":
		return "--verify, --output-schema, --plan-task and --save"
	case f.format != helptext.JSON && f.format != helptext.Text:
		return "--format " + f.format
	}
	return ""
}

// actionCheck is the usage error for an action the harness cannot run as asked, else nil.
func actionCheck(f *askFlags) error {
	if why := actionRefusal(f); why != "" {
		return fmt.Errorf("spec action runs no model: not with %s", why)
	}
	if f.action == "gateway" && len(f.actionOwn) > 0 {
		return errors.New("spec own: the gateway action takes no own paths")
	}
	if f.action == "diff" && slices.ContainsFunc(f.actionOwn, func(p string) bool { return strings.Contains(p, ",") }) {
		return errors.New("spec own: a path with a comma cannot go to agents diff --path")
	}
	return nil
}

// claimAction is the saved running record of an action, or the exit code of its refusal.
func claimAction(ctx context.Context, f *askFlags) (agent.Record, int) {
	rec, err := agent.ClaimAskRecord(ctx, localAgent, agent.RunOptions{Agent: localAgent, Prompt: "action " + f.action, WorkDir: f.workdir}, f.workdir, false)
	if err == nil {
		err = rec.Save() // running, with this pid: a long action stays visible and counted
	}
	if err == nil {
		return rec, 0
	}
	if _, usage := errors.AsType[*agent.UsageError](err); usage {
		return rec, failAskCode(f.format, localAgent, err, 2)
	}
	return rec, failAskCode(f.format, localAgent, err, 1)
}

// runAction runs the spec's action in a plain child with no model and reports it as a dispatch on "local".
func runAction(ctx context.Context, fs *flag.FlagSet, f *askFlags, out io.Writer) int {
	if err := actionCheck(f); err != nil {
		return failAskCode(f.format, localAgent, err, 2)
	}
	agentName := ""
	if helptext.Explicit(fs, "agent") {
		agentName = f.name
	}
	exe, err := os.Executable()
	if err != nil {
		return failAskCode(f.format, localAgent, err, 1)
	}
	rec, code := claimAction(ctx, f)
	if code != 0 {
		return code
	}
	begin := time.Now()
	buf, stderr, runErr := runActionChild(ctx, f, exe, actionArgv(f.action, f.actionOwn, agentName, f.model))
	runErr = errors.Join(runErr, finishAction(&rec, buf, stderr, runErr, time.Since(begin)))
	return reportAction(out, f, rec, buf.String(), runErr)
}

// reportAction prints the envelope (json) or the output, and returns the exit code.
func reportAction(out io.Writer, f *askFlags, rec agent.Record, answer string, runErr error) int {
	if runErr != nil {
		rec.Status = wire.StatusError
	}
	res := wire.Result{DispatchID: rec.ID, Agent: localAgent, Status: rec.Status, Answer: answer,
		DurationMs: rec.DurationMs, Hops: []wire.Hop{{Agent: localAgent}}}
	env := wire.NewEnvelope(res, runErr, rec.ID)
	env.RanOn, env.Route = "ran on "+localAgent+" · "+f.action, &rec.RanRoute
	var err error
	if f.format == helptext.JSON {
		err = writeEnvelope(out, env)
	} else {
		_, err = io.WriteString(out, answer)
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "ask: action:", runErr)
		}
	}
	if err != nil || runErr != nil {
		return 1
	}
	return 0
}

// runActionChild runs argv under --timeout; a gate run that reports a red gate in its JSON is an error too.
func runActionChild(ctx context.Context, f *askFlags, exe string, argv []string) (stdout, stderr *capBuffer, err error) {
	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}
	stdout, stderr = &capBuffer{}, &capBuffer{}
	cmd := exec.CommandContext(ctx, exe, argv...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = f.workdir, stdout, stderr
	proc.TreeCancel(cmd, 0) // a timeout kills go test and other descendants too
	cmd.WaitDelay = proc.ExecWaitDelay
	err = cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
		return stdout, stderr, fmt.Errorf("%s exited %d", strings.Join(argv[:min(2, len(argv))], " "), ee.ExitCode())
	}
	if err == nil && f.action == "gate" {
		err = redGate(stdout.Bytes())
	}
	return stdout, stderr, cmp.Or(ctx.Err(), err)
}

// redGate is an error when `worktree gate --run`'s JSON reports a nonzero exit; the verb itself always exits 0.
func redGate(out []byte) error {
	var g struct {
		Exit int `json:"exit"`
	}
	if err := json.Unmarshal(out, &g); err != nil {
		return fmt.Errorf("gate output unreadable: %w", err)
	}
	if g.Exit != 0 {
		return fmt.Errorf("gate exited %d", g.Exit)
	}
	return nil
}

// finishAction stamps the local run on rec, logs stdout then stderr and saves it; a save failure is returned.
func finishAction(rec *agent.Record, buf, stderr *capBuffer, runErr error, took time.Duration) error {
	rec.Status, rec.DurationMs, rec.PID, rec.Role = wire.StatusOK, took.Milliseconds(), 0, ""
	rec.RanRoute = route.Route{Provider: localAgent, Profile: "none"}
	if runErr != nil {
		rec.Status, rec.Error, rec.FailKind = wire.StatusError, runErr.Error(), agent.FailQuality
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
			rec.FailKind = agent.FailCancelled
		}
	}
	logBytes := slices.Concat(buf.Bytes(), stderr.Bytes())
	if buf.cut || stderr.cut {
		logBytes = append(logBytes, "\n[output cut at 1 MiB]\n"...)
	}
	var logErr, saveErr error
	if err := os.WriteFile(rec.LogPath(), logBytes, 0o600); err != nil {
		logErr = fmt.Errorf("action log %s: %w", rec.LogPath(), err)
	}
	if err := rec.Save(); err != nil {
		saveErr = fmt.Errorf("action record %s not saved: %w", rec.ID, err)
	}
	return errors.Join(logErr, saveErr)
}
