// Package pi drives the Pi coding agent (`pi --mode json`) through the harness's loopback proxy.
package pi

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

const (
	agentDirEnv = "PI_CODING_AGENT_DIR"
	// guardToolsEnv names the tools the guard registers; there is no powershell: the guard maps no such tool.
	guardToolsEnv = "HARNESS_PI_GUARD_TOOLS"
)

// lockdownArgv loads no project code: --no-approve ignores the work dir's .pi (extensions, packages, settings) and
// the rest turn off every other discovery path; the agent dir is our own empty one.
var lockdownArgv = []string{"--no-mcp", "--no-context-files", "--no-approve", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes"}

// keyEnv is the variable the generated models.json interpolates as every provider's apiKey.
var keyEnv = strings.TrimPrefix(KeyRef, "$")

type piEngine struct{ agent.BaseEngine }

var _ agent.HeadlessDriver = piEngine{}

// piBases re-derives the three wire bases from the route's base by swapping the dialect's router suffix.
func piBases(base, dialect string) Bases {
	root := strings.TrimSuffix(base, row.CLI.RouterSuffix[dialect])
	suf := row.CLI.RouterSuffix
	return Bases{Claude: root + suf[providers.ProtocolAnthropic], Responses: root + suf[providers.ProtocolResponses],
		Chat: root + suf[providers.ProtocolOpenAI]}
}

// Wire writes the run's private agent dir and builds Pi's env and argv; r.Cred is the proxy session token, never upstream's.
func (piEngine) Wire(_ context.Context, r agent.WireRoute, h *agent.Headless) (env, argv []string, err error) {
	if !r.Routed {
		return nil, nil, errors.New("pi runs only through the proxy; drop --direct")
	}
	if h == nil {
		return nil, nil, errors.New("pi: no interactive launch yet; use agents ask")
	}
	thinking, err := thinkingArgv(h.Effort)
	if err != nil {
		return nil, nil, err
	}
	mj, sj, menu, err := BuildConfig(r.Provider, cmp.Or(r.Target.Provider, r.Provider), piBases(r.Base, r.Dialect), r.Model)
	if err != nil {
		return nil, nil, err
	}
	prov, ok := menu[r.Model]
	if !ok {
		return nil, nil, fmt.Errorf("pi: model %q is not on the %s menu", r.Model, r.Provider.Name)
	}
	dir, remove, err := NewRunDir(r.Provider.Name)
	if err != nil {
		return nil, nil, err
	}
	if err = WriteConfig(dir, mj, sj); err != nil {
		remove()
		return nil, nil, err
	}
	env = []string{agentDirEnv + "=" + dir, keyEnv + "=" + r.Cred}
	if env, err = withGuard(env, dir, h.Tools); err != nil {
		remove()
		return nil, nil, err
	}
	argv = append(append([]string{"--mode", "json"}, lockdownArgv...), "--model", prov+"/"+r.Model)
	argv = append(argv, thinking...)
	if h.SessionID != "" {
		return env, append(argv, "--session", h.SessionID, "--session-dir", filepath.Join(paths.StateDir(), "pi", "sessions")), nil
	}
	return env, append(argv, "--no-session"), nil
}

// thinkingArgv maps --effort to Pi's --thinking level; no effort adds nothing.
func thinkingArgv(effort string) ([]string, error) {
	if effort == "" {
		return nil, nil
	}
	native, _ := agentid.NativeEffort(agentid.Pi, agentid.Effort(effort))
	if native == "" {
		return nil, fmt.Errorf("pi: effort %q is not one of %s", effort, strings.Join(agentid.NativeEfforts(agentid.Pi), ", "))
	}
	return []string{"--thinking", native}, nil
}

// withGuard adds the guard's env for a tools run; fail closed: no guard written, no tools run.
func withGuard(env []string, dir string, tools bool) ([]string, error) {
	if !tools {
		return env, nil
	}
	g, err := WriteGuard(dir)
	if err != nil {
		return nil, err
	}
	return append(env, g.Env()...), nil
}

// RenderSettings adds the base settings and a cleanup that removes the run's private agent dir.
func (e piEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	env, argv, emu, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	for _, kv := range env {
		if dir, ok := strings.CutPrefix(kv, agentDirEnv+"="); ok && filepath.Base(filepath.Dir(dir)) == "runs" {
			return env, argv, emu, func() { cleanup(); _ = os.RemoveAll(dir) }, err
		}
	}
	return env, argv, emu, cleanup, err
}

// guardFromEnv rebuilds the run's guard from the env Wire wrote; it is the only source of a tools run's argv.
func guardFromEnv(env []string) (Guard, bool) {
	get := func(name string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				return v
			}
		}
		return ""
	}
	dir, file, nonce := get(agentDirEnv), get(nonceFileEnv), get(nonceValueEnv)
	if dir == "" || file == "" || nonce == "" {
		return Guard{}, false
	}
	return Guard{Path: filepath.Join(dir, guardFile), NonceFile: file, Nonce: nonce}, true
}

// toolsArgv is Pi's whole tool selection: the guard's -e and no builtin tools, so the only tools are the guard's own.
func toolsArgv(req agent.RunOptions, env []string) ([]string, error) {
	if !req.Tools {
		return []string{"--no-tools"}, nil
	}
	g, ok := guardFromEnv(env)
	if !ok {
		return nil, errors.New("pi: no guard extension for this run; tools refused")
	}
	return append(g.Argv(), "--no-builtin-tools"), nil
}

// guardTools is the tools the guard registers for req: read-only and plan runs drop the writers, and each file deny
// drops what the gate cannot enforce on it: bash for any file deny (`echo x > f`), grep for a Read, Glob or Grep deny
// (it reads every file under a path the gate judged once), find and ls too for a Glob or Grep deny.
func guardTools(req agent.RunOptions) []string {
	drop := []string{}
	if req.ReadOnly || req.Permissions.Mode == agent.PermPlan {
		drop = append(drop, "bash", "edit", "write")
	}
	for _, r := range req.Permissions.Deny {
		switch r.Tool {
		case "Edit", "Write", "MultiEdit":
			drop = append(drop, "bash")
		case "Read":
			drop = append(drop, "bash", "grep") // find and ls show names, which Read denies do not hide for any agent
		case "Glob", "Grep":
			drop = append(drop, "bash", "grep", "find", "ls")
		}
	}
	return slices.DeleteFunc([]string{"read", "bash", "edit", "write", "grep", "find", "ls"}, func(t string) bool { return slices.Contains(drop, t) })
}

// After appends the system prompt flags, the tool selection and, last, the prompt; Exec re-reads toolsArgv's refusal.
func (piEngine) After(p *agent.CLIPrep, req agent.RunOptions) {
	if req.SystemPrompt != "" {
		p.Argv = append(p.Argv, "--system-prompt", req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		p.Argv = append(p.Argv, "--append-system-prompt", req.AppendSystemPrompt)
	}
	if tools, err := toolsArgv(req, p.Env); err == nil {
		p.Argv = append(p.Argv, tools...)
		if req.Tools {
			p.Env = append(p.Env, guardToolsEnv+"="+strings.Join(guardTools(req), ","))
		}
	}
	p.Argv = append(p.Argv, p.Prompt)
}

func (e piEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	if err := agent.CheckCapabilities(e.Name(), req); err != nil {
		return p.Result, err
	}
	if err := refuseRequest(req); err != nil {
		return p.Result, retry.Fatal(e.Name(), err)
	}
	if _, err := toolsArgv(req, p.Env); err != nil {
		return p.Result, retry.Fatal(e.Name(), err)
	}
	bin, err := e.Bin()
	if err != nil {
		return p.Result, err
	}
	cmd, tail := childCmd(ctx, bin, p, req)
	idle := agent.NewIdleTimeout(e.Name(), agent.IdleLimitFor(e.Name()), nil)
	w := newGuardWatch(e.Name(), req.Tools, p.Env, cmd)
	parse := func(br *bufio.Reader, s wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseEvents(br, s, onMeaningful, func(ev piEvent) error { beat(idle, ev); return w.check() }, logs...)
	}
	return agent.RunStreamWith(ctx, e.Name(), cmd, tail, p.Result, sink, parse, agent.StreamOpts{Idle: idle, Cause: w.cause})
}

// childCmd is Pi's process; fail closed: no source (inherited, vault, overlay, --env) may hand it a HARNESS_HOOK_* switch.
func childCmd(ctx context.Context, bin string, p agent.CLIPrep, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer) {
	cmd, tail := agent.ChildCmd(ctx, bin, p.Argv, p.Env, req.WorkDir, "", req.KeepEnv, true)
	cmd.Env = withoutHookControl(cmd.Env)
	return cmd, tail
}

// guardWatch proves the guard loaded on the run's first event and kills the child when it did not.
type guardWatch struct {
	name    string
	g       Guard
	cmd     *exec.Cmd
	pending bool // a tools run whose first event is not yet checked
	err     error
}

func newGuardWatch(name string, tools bool, env []string, cmd *exec.Cmd) *guardWatch {
	g, _ := guardFromEnv(env) // Exec already ran toolsArgv, so a tools run has its guard
	return &guardWatch{name: name, g: g, cmd: cmd, pending: tools}
}

// check fails closed: before any other event is forwarded, a guard that is not ready ends the run.
func (w *guardWatch) check() error {
	if !w.pending {
		return nil
	}
	w.pending = false
	if err := w.g.Ready(); err != nil {
		w.err = retry.Fatal(w.name, fmt.Errorf("pi guard did not load; run refused (builtin tools are off, so no tool could run unguarded): %w", err))
		_ = w.cmd.Cancel()
		return w.err
	}
	return nil
}

// cause outranks the idle and exit errors the kill produces.
func (w *guardWatch) cause() error { return w.err }

// beat keeps the idle watch honest: a running tool holds it off, any other progress restarts it.
func beat(idle *agent.IdleTimeout, ev piEvent) {
	switch ev.Type {
	case "tool_execution_start":
		idle.Pause()
	case "tool_execution_end", "message_update", "turn_start":
		idle.Beat(ev.Type)
	}
}

// RouteDetail is the doctor detail: the private agent dir, never a credential.
func (piEngine) RouteDetail(ctx context.Context) (string, bool) {
	pin, err := agent.PinnedProfile(ctx, agentid.Pi)
	if err != nil {
		return err.Error(), true
	}
	if pin == "" {
		return "provider=databricks (pool: no profile pinned, each run picks one) runs=" + RunsDir("<profile>"), true
	}
	return fmt.Sprintf("provider=%s runs=%s", pin, RunsDir(pin)), true
}
