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
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

const (
	agentDirEnv = "PI_CODING_AGENT_DIR"
	// guardToolsEnv names the tools the guard registers; there is no powershell: the guard maps no such tool.
	guardToolsEnv = "HARNESS_PI_GUARD_TOOLS"
	// stopGrace is how long Pi gets to kill its own bash groups after SIGTERM before the descendants are killed.
	stopGrace = 2 * time.Second
	// packageDirsEnv carries Wire's synced package dirs to After; After strips it, so Pi never sees it.
	packageDirsEnv = "HARNESS_PI_PACKAGE_DIRS"
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
func (e piEngine) Wire(ctx context.Context, r agent.WireRoute, h *agent.Headless) (env, argv []string, err error) {
	env, argv, err = e.wire(ctx, r, h)
	if err != nil {
		return nil, nil, err
	}
	return append(env, tools.PiNodePathEnv(ctx)...), argv, nil // the pi bin's `env node` must not pick an older PATH node
}

func (piEngine) wire(ctx context.Context, r agent.WireRoute, h *agent.Headless) (env, argv []string, err error) {
	if !r.Routed {
		return nil, nil, errors.New("pi runs only through the proxy; drop --direct")
	}
	if h == nil {
		return interactiveWire(ctx, r)
	}
	thinking, err := thinkingArgv(h.Effort)
	if err != nil {
		return nil, nil, err
	}
	dir, model, pkgDirs, remove, err := runSetup(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	env = []string{agentDirEnv + "=" + dir, keyEnv + "=" + r.Cred}
	if len(pkgDirs) > 0 { // After appends them behind the guard's -e, which it adds later
		env = append(env, packageDirsEnv+"="+strings.Join(pkgDirs, string(filepath.ListSeparator)))
	}
	if env, err = withGuard(env, dir, h.Tools); err != nil {
		remove()
		return nil, nil, err
	}
	argv = append(append([]string{"--mode", "json"}, lockdownArgv...), "--model", model)
	argv = append(argv, thinking...)
	if h.SessionID != "" {
		return env, append(argv, "--session", h.SessionID, "--session-dir", filepath.Join(paths.StateDir(), "pi", "sessions")), nil
	}
	return env, append(argv, "--no-session"), nil
}

// runSetup writes the run's private agent dir and returns it with Pi's model, the synced package dirs and a remover.
func runSetup(ctx context.Context, r agent.WireRoute) (dir, model string, pkgDirs []string, remove func(), err error) {
	mj, sj, menu, err := BuildConfig(r.Provider, cmp.Or(r.Target.Provider, r.Provider), piBases(r.Base, r.Dialect), r.Model)
	if err != nil {
		return "", "", nil, nil, err
	}
	prov, ok := menu[r.Model]
	if !ok {
		return "", "", nil, nil, fmt.Errorf("pi: model %q is not on the %s menu", r.Model, r.Provider.Name)
	}
	dir, remove, err = NewRunDir(r.Provider.Name)
	if err != nil {
		return "", "", nil, nil, err
	}
	if err = WriteConfig(dir, mj, sj); err != nil {
		remove()
		return "", "", nil, nil, err
	}
	if pkgDirs, err = recordPackages(ctx); err != nil {
		remove() // fail closed: never launch with a partial or unreviewed package set
		return "", "", nil, nil, err
	}
	return dir, prov + "/" + r.Model, pkgDirs, remove, nil
}

// recordPackages syncs the Pi record's pinned packages and returns the dirs to load; a preview installs nothing.
func recordPackages(ctx context.Context) ([]string, error) {
	rec, err := agentconfig.Settings(agentid.Pi)
	if err != nil {
		return nil, err
	}
	wanted, err := ParsePackages(rec.Extras[extraPackages])
	if err != nil || len(wanted) == 0 {
		return nil, err // nothing pinned reads no approval list and touches no store
	}
	approved, err := ParsePackages(rec.Extras[extraPackagesApproved])
	if err != nil {
		return nil, err
	}
	if agent.IsDryRun(ctx) {
		return previewPackages(wanted), nil
	}
	loaded, err := SyncPackages(ctx, agent.NoticeOut, paths.ConfigDir(), wanted, approved)
	if err != nil {
		return nil, err
	}
	return packageDirs(loaded), nil
}

// previewPackages is the dirs a launch would load, computed without installing, reviewing or pruning.
func previewPackages(wanted []Package) []string {
	var dirs []string
	for _, p := range wanted {
		dir := p.LocalPath(paths.ConfigDir())
		if p.Kind != KindLocal {
			dir = packageDir(filepath.Join(packageStore(), slotName(p)), p)
		}
		fmt.Fprintf(agent.NoticeOut, "claude-code: pi packages preview: would load %s from %s (nothing installed)\n", p.Spec, dir)
		dirs = append(dirs, dir)
	}
	return dirs
}

// packageDirs is the directory of each loaded package, in load order.
func packageDirs(loaded []Loaded) []string {
	dirs := make([]string, len(loaded))
	for i, l := range loaded {
		dirs[i] = l.Dir
	}
	return dirs
}

// loadedAt wraps dirs for PackageArgv, which reads only each package's Dir.
func loadedAt(dirs []string) []Loaded {
	out := make([]Loaded, len(dirs))
	for i, d := range dirs {
		out[i].Dir = d
	}
	return out
}

// takePackageDirs splits Wire's package dirs off env so Pi's own env never carries them.
func takePackageDirs(env []string) (rest, dirs []string) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, packageDirsEnv+"="); ok {
			dirs = filepath.SplitList(v)
			continue
		}
		rest = append(rest, kv)
	}
	return rest, dirs
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
	g, err := writeGuard(dir)
	if err != nil {
		return nil, err
	}
	return append(env, g.Env()...), nil
}

// RenderSettings adds the base settings and a cleanup that removes the run's private agent dir.
func (e piEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	if l := s.Record.Launch; l != nil && !s.Headless { // the record's launch args reach the TUI argv: same allow-list
		if err := checkPassArgs(l.Args); err != nil {
			return nil, nil, nil, func() {}, fmt.Errorf("pi launch.args: %w", err)
		}
	}
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
	var dirs []string
	p.Env, dirs = takePackageDirs(p.Env)
	if tools, err := toolsArgv(req, p.Env); err == nil {
		p.Argv = append(p.Argv, tools...)
		if req.Tools {
			p.Env = append(p.Env, guardToolsEnv+"="+strings.Join(guardTools(req), ","))
		}
		// the guard's -e stays first: Pi keeps the first registration of a tool name, so a package never shadows a guarded tool
		p.Argv = append(p.Argv, PackageArgv(loadedAt(dirs))...)
	}
	p.Argv = append(p.Argv, "--", promptArg(p.Prompt))
}

// promptArg goes after "--", where Pi reads text, never a flag (-a, -e); a leading @ would still read a file unguarded.
func promptArg(prompt string) string {
	if strings.HasPrefix(prompt, "@") {
		return " " + prompt
	}
	return prompt
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
	ti := newToolIdle(idle)
	parse := func(br *bufio.Reader, s wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseEvents(br, s, onMeaningful, func(ev piEvent) error { ti.beat(ev); return w.check() }, logs...)
	}
	return agent.RunStreamWith(ctx, e.Name(), cmd, tail, p.Result, sink, parse, agent.StreamOpts{Idle: idle, Cause: w.cause})
}

// childCmd is Pi's process; fail closed: no source (inherited, vault, overlay, --env) may hand it a HARNESS_HOOK_* switch.
func childCmd(ctx context.Context, bin string, p agent.CLIPrep, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer) {
	cmd, tail := agent.ChildCmd(ctx, bin, p.Argv, p.Env, req.WorkDir, "", req.KeepEnv, true)
	cmd.Env = withoutHookControl(cmd.Env)
	proc.TreeCancel(cmd, stopGrace) // Pi's bash runs in its own group: a group kill alone leaves it running
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

// toolIdle keeps the idle watch honest: any running tool holds it off, other progress restarts it.
type toolIdle struct {
	idle   *agent.IdleTimeout
	active map[string]bool // tool call ids between start and end; Pi runs calls in parallel
}

func newToolIdle(idle *agent.IdleTimeout) *toolIdle {
	return &toolIdle{idle: idle, active: map[string]bool{}}
}

func (t *toolIdle) beat(ev piEvent) {
	switch ev.Type {
	case "tool_execution_start":
		t.active[ev.ToolCallID] = true
		t.idle.Pause()
	case "tool_execution_end":
		delete(t.active, ev.ToolCallID) // an end with no start changes nothing
		if len(t.active) == 0 {
			t.idle.Beat(ev.Type)
		}
	case "message_update", "turn_start":
		if len(t.active) == 0 {
			t.idle.Beat(ev.Type)
		}
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
