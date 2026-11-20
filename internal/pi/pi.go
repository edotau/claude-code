// Package pi drives the Pi coding agent (`pi --mode json`) through the harness's loopback proxy.
package pi

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	readTools   = "read,grep,find,ls"
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
	var thinking []string
	if h.Effort != "" {
		native, _ := agentid.NativeEffort(agentid.Pi, agentid.Effort(h.Effort))
		if native == "" {
			return nil, nil, fmt.Errorf("pi: effort %q is not one of %s", h.Effort, strings.Join(agentid.NativeEfforts(agentid.Pi), ", "))
		}
		thinking = []string{"--thinking", native}
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
	argv = append(append([]string{"--mode", "json"}, lockdownArgv...), "--model", prov+"/"+r.Model)
	argv = append(argv, thinking...)
	if h.SessionID != "" {
		return env, append(argv, "--session", h.SessionID, "--session-dir", filepath.Join(paths.StateDir(), "pi", "sessions")), nil
	}
	return env, append(argv, "--no-session"), nil
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

// toolsArgv is Pi's tool selection; write tools wait for the stage 2 guard extension.
func toolsArgv(req agent.RunOptions) ([]string, error) {
	switch {
	case !req.Tools:
		return []string{"--no-tools"}, nil
	case req.ReadOnly:
		return []string{"--tools", readTools}, nil
	}
	return nil, errors.New("pi: write tools need the stage 2 guard extension (plan pi-agent, Phase 2); run with --read-only")
}

// After appends the system prompt flags, the tool selection and, last, the prompt; Exec re-reads toolsArgv's refusal.
func (piEngine) After(p *agent.CLIPrep, req agent.RunOptions) {
	if req.SystemPrompt != "" {
		p.Argv = append(p.Argv, "--system-prompt", req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		p.Argv = append(p.Argv, "--append-system-prompt", req.AppendSystemPrompt)
	}
	if tools, err := toolsArgv(req); err == nil {
		p.Argv = append(p.Argv, tools...)
	}
	p.Argv = append(p.Argv, p.Prompt)
}

func (e piEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	if err := agent.CheckCapabilities(e.Name(), req); err != nil {
		return p.Result, err
	}
	if _, err := toolsArgv(req); err != nil {
		return p.Result, retry.Fatal(e.Name(), err)
	}
	bin, err := e.Bin()
	if err != nil {
		return p.Result, err
	}
	cmd, tail := agent.ChildCmd(ctx, bin, p.Argv, p.Env, req.WorkDir, "", req.KeepEnv, true)
	idle := agent.NewIdleTimeout(e.Name(), agent.IdleLimitFor(e.Name()), nil)
	parse := func(br *bufio.Reader, s wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseEvents(br, s, onMeaningful, func(ev piEvent) { beat(idle, ev) }, logs...)
	}
	return agent.RunStreamWith(ctx, e.Name(), cmd, tail, p.Result, sink, parse, agent.StreamOpts{Idle: idle})
}

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
