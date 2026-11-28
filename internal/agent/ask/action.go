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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	acts "github.com/edotau/claude-code/internal/agent/actions"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/route"
)

// localAgent is the agent name of a no-model action's record and envelope.
const localAgent = "local"

// actionArgv is the action's verb argv, built from typed fields; gate and diff come from the actions table.
func actionArgv(action string, own []string, agentName, model, workdir string) []string {
	switch action {
	case "gate", "diff":
		argv, _, _ := acts.Argv(action, ownArgs(action, own, workdir))
		return argv
	case "scope":
		argv, _, err := acts.Argv("gate", ownArgs("gate", own, workdir))
		if err != nil {
			return nil
		}
		return append([]string{"worktree", "gate", "--format", "json"}, argv[3:]...)
	case "gateway":
		argv := []string{"gateway", "--format", "json", "--fallback", "none"}
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

// ownArgs is the brief's own list as the table's typed arguments for gate (scope) or diff (paths).
func ownArgs(action string, own []string, workdir string) json.RawMessage {
	key := map[string]string{"gate": "scope", "diff": "paths"}[action]
	cwd, err := os.Getwd()
	if workdir != "" {
		cwd, err = filepath.Abs(workdir)
	}
	if err == nil && action == "gate" { // match the child's cwd, not the dispatcher's
		own = slices.Clone(own)
		for i, p := range own {
			if !filepath.IsAbs(p) {
				continue
			}
			if rel, err := filepath.Rel(paths.CanonDeep(cwd), paths.CanonDeep(p)); err == nil && filepath.IsLocal(rel) {
				own[i] = rel
			}
		}
	}
	raw, _ := json.Marshal(map[string][]string{key: own})
	return raw
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
		return fmt.Errorf("brief action runs no model: not with %s", why)
	}
	action := f.action
	if action == "scope" {
		action = "gate"
	}
	if _, _, err := acts.Argv(action, ownArgs(action, f.actionOwn, f.workdir)); err != nil {
		return fmt.Errorf("brief own: %w", err)
	}
	if f.action == "gateway" && len(f.actionOwn) > 0 {
		return errors.New("brief own: the gateway action takes no own paths")
	}
	if f.action == "diff" && slices.ContainsFunc(f.actionOwn, func(p string) bool { return strings.Contains(p, ",") }) {
		return errors.New("brief own: a path with a comma cannot go to agents diff --path")
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
	buf, stderr, runErr := runActionChild(ctx, f, exe, actionArgv(f.action, f.actionOwn, agentName, f.model, f.workdir))
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
		Exit *int `json:"exit"`
	}
	if err := json.Unmarshal(out, &g); err != nil {
		return fmt.Errorf("gate output unreadable: %w", err)
	}
	if g.Exit == nil {
		return errors.New("gate output has no exit result")
	}
	if *g.Exit != 0 {
		return fmt.Errorf("gate exited %d", *g.Exit)
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

// Only one gate per server may run at a time; waiting also consumes the tools timeout.
var tableGateSlot = make(chan struct{}, 1)

// runTableAction runs a table action in this binary under the dispatch tools timeout; a failure carries the output.
func runTableAction(ctx context.Context, spec acts.Spec, raw json.RawMessage) (string, error) {
	argv, err := spec.Argv(raw)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return runTableCommand(ctx, spec.Name, exe, argv)
}

// acquireTableGate takes the single gate slot for a gate call; other tools hold none.
func acquireTableGate(ctx context.Context, name string) (func(), error) {
	if name != "gate" {
		return func() {}, nil
	}
	select {
	case tableGateSlot <- struct{}{}:
		return func() { <-tableGateSlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tableText joins stdout and stderr under the output cap, marking a cut.
func tableText(out, stderr *capBuffer) string {
	text := out.String() + stderr.String()
	cut := out.cut || stderr.cut || len(text) > actionOutCap
	if len(text) > actionOutCap {
		text = text[:actionOutCap]
	}
	if cut {
		text += "\n[output cut at 1 MiB]\n"
	}
	return text
}

// runTableCommand is the bounded process owner shared by the table's tools.
func runTableCommand(ctx context.Context, name, exe string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, agent.DefaultToolsTimeout)
	defer cancel()
	release, err := acquireTableGate(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	out, stderr := &capBuffer{}, &capBuffer{}
	cmd := exec.CommandContext(ctx, exe, argv...)
	cmd.Stdout, cmd.Stderr = out, stderr
	proc.TreeCancel(cmd, 0)
	cmd.WaitDelay = proc.ExecWaitDelay
	runErr := cmd.Run()
	cleanupErr := proc.TreeSweep(cmd)
	if ee, ok := errors.AsType[*exec.ExitError](runErr); ok && name == "diff" && ee.ExitCode() == 3 && ctx.Err() == nil {
		runErr = nil // outside --path is a finding in the JSON, not a failed gate
	}
	if runErr == nil && name == "gate" {
		runErr = redGate(out.Bytes()) // worktree gate reports failure in JSON, not its process exit
	}
	runErr = errors.Join(ctx.Err(), runErr, cleanupErr)
	text := tableText(out, stderr)
	if runErr != nil {
		return "", fmt.Errorf("%s failed: %v\n%s", name, runErr, text)
	}
	return text, nil
}
