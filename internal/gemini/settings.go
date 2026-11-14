package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// renderGeminiSettings merges the spec's exact-match guard denies into agy's
// ~/.gemini/antigravity-cli/settings.json permissions.deny, preserving every other key.
func renderGeminiSettings(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	rec, err := agentconfig.Settings(agentid.Gemini)
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: agent settings: %w", err)
	}
	if rec.Extras[extraUseGuardRules] == "false" {
		return false, nil
	}
	cfg, err := loadAgentConfig()
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: load agentconfig: %w", err)
	}
	rules := agyGuardRules(cfg.Guard().Deny, out)
	path := agySettingsPath(home)
	return agentconfig.CommitJSONObject(path, o, out, "gemini-settings", func(obj map[string]any, existed bool) bool {
		if len(rules) == 0 && !existed {
			return false // nothing to deny: don't create agy's settings.json just to write an empty list
		}
		perms, existing, ok := agyPermissions(obj)
		if !ok {
			fmt.Fprintf(out, "gemini-settings: skipped: %s has a non-object permissions or non-array permissions.deny\n", path)
			return false
		}
		merged := []any{}
		for _, d := range mergeAgyDeny(existing, rules) {
			merged = append(merged, d)
		}
		for _, e := range existing { // keep entries agy may define that are not plain strings
			if _, ok := e.(string); !ok {
				merged = append(merged, e)
			}
		}
		perms["deny"] = merged
		obj["permissions"] = perms
		return true
	})
}

// agyPermissions returns obj's permissions map and deny list; ok is false when either holds another JSON type.
func agyPermissions(obj map[string]any) (perms map[string]any, deny []any, ok bool) {
	perms = map[string]any{}
	if v, has := obj["permissions"]; has {
		if perms, ok = v.(map[string]any); !ok {
			return nil, nil, false
		}
	}
	if v, has := perms["deny"]; has {
		if deny, ok = v.([]any); !ok {
			return nil, nil, false
		}
	}
	return perms, deny, true
}

// agyGuardRules converts Bash(...) denies to agy command(...) rules (a glob keeps its literal prefix), noting the rest.
func agyGuardRules(deny []string, out io.Writer) []string {
	var rules []string
	for _, r := range deny {
		cmd, ok := strings.CutPrefix(r, "Bash(")
		cmd, ok2 := strings.CutSuffix(cmd, ")")
		if !ok || !ok2 || strings.HasSuffix(cmd, ":*") {
			fmt.Fprintf(out, "gemini-settings: note: %q is not exact-match; agy denies exact commands only, skipped\n", r)
			continue
		}
		if i := strings.IndexAny(cmd, "*?["); i >= 0 { // agy matches command rules by token prefix, never by glob
			if strings.Trim(cmd[i:], "*?") != "" || strings.TrimSpace(cmd[:i]) == "" { // only a trailing glob keeps its prefix
				fmt.Fprintf(out, "gemini-settings: note: %q is not a trailing glob; an agy prefix rule would widen it, skipped\n", r)
				continue
			}
			cmd = strings.TrimSpace(cmd[:i])
			fmt.Fprintf(out, "gemini-settings: note: %q written as the agy prefix rule command(%s)\n", r, cmd)
		}
		rules = append(rules, "command("+cmd+")")
	}
	return rules
}

// mergeAgyDeny returns the string entries of existing, then rules, without duplicates.
func mergeAgyDeny(existing []any, rules []string) []string {
	seen := map[string]bool{}
	merged := make([]string, 0, len(existing)+len(rules))
	for _, e := range existing {
		s, ok := e.(string)
		if !ok || seen[s] {
			continue
		}
		seen[s] = true
		merged = append(merged, s)
	}
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		merged = append(merged, r)
	}
	return merged
}

// loadAgentConfig is the agents.json loader; a package var so tests can fail it.
var loadAgentConfig = agentconfig.LoadConfig

// RenderSettings adds --effort and, headless, the per-run --settings after the default launch env and args.
func (e geminiEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	env, argv, emulated, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	if argv, emulated, err = agyEffortArgv(s, argv, emulated); err != nil || !s.Headless {
		return env, argv, emulated, cleanup, err
	}
	settings, notes, err := agyPermissionSettings(s.Permissions, s.Record.Extras[extraUseGuardRules] != "false", s.Tools, s.WorkDir)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	if settings != "" {
		argv = append(argv, "--settings", settings)
	}
	for _, n := range notes {
		emulated = append(emulated, "permissions:"+n)
	}
	return env, argv, emulated, cleanup, nil
}

// agyEffortArgv sets --effort to s.Effort; beside an effort-suffixed --model (agy refuses both) it is emulated.
func agyEffortArgv(s agent.Settings, argv, emulated []string) ([]string, []string, error) {
	if s.Effort == "" {
		return argv, emulated, nil
	}
	if err := agentid.ValidEffort(agentid.Gemini, s.Effort); err != nil {
		return argv, emulated, fmt.Errorf("gemini: %w", err)
	}
	model := s.Target.Model
	if i := slices.Index(argv, "--model"); i >= 0 && i+1 < len(argv) {
		model = argv[i+1]
	}
	if effortSuffixed(model) {
		return argv, append(emulated, "effort "+s.Effort+": model "+model+" carries its own"), nil
	}
	if i := slices.Index(argv, "--effort"); i >= 0 && i+1 < len(argv) { // an interactive gateway pin's suffix: s.Effort wins
		argv = slices.Clone(argv)
		argv[i+1] = s.Effort
		return argv, emulated, nil
	}
	return append(argv, "--effort", s.Effort), emulated, nil
}

// agyPermissionSettings renders p as agy's per-run --settings JSON (plus guard denies when guard); notes name each
// Ask that runs as Allow (O1: agy has no headless judge) and each unmapped rule; a Deny agy cannot hold is an error.
func agyPermissionSettings(p agent.Permissions, guard, tools bool, cwd string) (settings string, notes []string, err error) {
	if p.IsZero() {
		return "", nil, nil
	}
	if err := agyPlanRefusal(p, tools, cwd); err != nil {
		return "", nil, err
	}
	cfg, err := loadAgentConfig()
	if err != nil {
		return "", nil, fmt.Errorf("agy: guard denies for --settings: %w", err)
	}
	var deny []string
	if guard {
		deny = agyGuardRules(cfg.Guard().Deny, io.Discard)
	}
	// A Bash Deny, or a Read/Write/Edit Deny agy's exact rules cannot express (a pattern, a bare tool), is the gate hook's.
	deny, unDeny := appendAgyRules(deny, p.Deny, cwd)
	var unenforced []agent.Rule
	for _, r := range p.Deny {
		if (r.Tool == "Bash" || slices.Contains(unDeny, r)) && !agyGateCovers(cwd, agyGateTools[r.Tool]...) {
			unenforced = append(unenforced, r)
		}
	}
	if len(unenforced) > 0 {
		home, _ := agentconfig.HomeOr("")
		return "", nil, fmt.Errorf("agy cannot enforce deny rule(s) %s: the permission-gate hook does not cover them in %s; run `claude-code configure --restore --agents --agent gemini`: dispatch refused",
			strings.Join(agent.RuleStrings(unenforced), ", "), agyHooksPath(home))
	}
	allow, unAllow := appendAgyRules(nil, slices.Concat(p.Allow, p.Ask), cwd)
	for _, r := range p.Ask {
		notes = append(notes, "ask "+r.String()+" runs as allow (agy has no headless judge)")
	}
	perms := map[string][]string{"deny": mergeAgyDeny(nil, deny)}
	if len(allow) > 0 {
		perms["allow"] = mergeAgyDeny(nil, allow)
	}
	b, err := json.Marshal(map[string]any{"permissions": perms})
	if err != nil {
		return "", nil, fmt.Errorf("agy: encode --settings: %w", err)
	}
	return string(b), append(notes, agent.RuleStrings(unAllow)...), nil
}

// agyPlanRefusal refuses plan mode where agy cannot deny unmatched calls: with --tools every tool runs, and without
// them reads in the workspace still run unless the permission-gate hook judges read_file.
func agyPlanRefusal(p agent.Permissions, tools bool, cwd string) error {
	switch {
	case p.Mode != "plan":
		return nil
	case tools:
		return retry.Fatal("gemini", errors.New("permission mode plan denies unmatched calls, which agy cannot enforce with --tools; drop --tools or --mode plan"))
	case !agyGateCovers(cwd, "read_file"):
		home, _ := agentconfig.HomeOr("")
		return retry.Fatal("gemini", fmt.Errorf("permission mode plan needs the permission-gate hook on read_file in %s, which agy otherwise runs; run `claude-code configure --restore --agents --agent gemini`", agyHooksPath(home)))
	}
	return nil
}

// agyDenier explains agy's refusals of calls the run's policy did not grant natively.
type agyDenier struct {
	p   agent.Permissions
	cwd string
}

// agyRefusedCall is the call a tool step agy refused for want of a grant; a deny rule's refusal reads differently.
func agyRefusedCall(ev agyEvent) (agent.Rule, bool) {
	u := ev.StepUpdate
	if u.StepType != agyToolStep || u.State != "ERROR" || !strings.Contains(u.ToolInfo.Error.Message, "user denied permission") {
		return agent.Rule{}, false
	}
	var args struct{ CommandLine, Path string }
	if json.Unmarshal(u.ToolInfo.Parameters, &args) != nil {
		return agent.Rule{}, false
	}
	switch u.ToolName {
	case "run_command":
		return agent.Rule{Tool: "Bash", Arg: args.CommandLine}, args.CommandLine != ""
	case "read_file":
		return agent.Rule{Tool: "Read", Arg: args.Path}, args.Path != ""
	case "write_file":
		return agent.Rule{Tool: "Write", Arg: args.Path}, args.Path != ""
	case "edit_file":
		return agent.Rule{Tool: "Edit", Arg: args.Path}, args.Path != ""
	}
	return agent.Rule{}, false
}

// report prints notes to stderr, where agy's own denial line goes.
func (d *agyDenier) report(calls []agent.Rule) {
	for _, n := range d.notes(calls) {
		fmt.Fprintln(os.Stderr, n)
	}
}

// notes names, per refused call, the exact --allow that repairs it; a denied call or one agy has no native rule for gets none.
func (d *agyDenier) notes(calls []agent.Rule) []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, c := range calls {
		switch {
		case d.p.Explain(c.Tool, c.Arg).Source == "permissions.deny":
			out = append(out, "agy: refused "+c.String()+": a permission deny rule matches it; no allow offered")
		case agyRules(c, d.cwd) == nil:
			out = append(out, "agy: refused "+c.String()+": agy has no native rule for it; no allow offered")
		default:
			out = append(out, "agy: refused "+c.String()+": retry with --allow "+textutil.ShellQuote(c.String()))
		}
	}
	return out
}

// appendAgyRules appends each rule agy can express natively to dst and names the rest.
func appendAgyRules(dst []string, rules []agent.Rule, cwd string) ([]string, []agent.Rule) {
	var unmapped []agent.Rule
	for _, r := range rules {
		if s := agyRules(r, cwd); len(s) > 0 {
			dst = append(dst, s...)
		} else {
			unmapped = append(unmapped, r)
		}
	}
	return dst, unmapped
}

// agyRules maps an exact Bash command or glob-free path; Write and Edit give write_file and edit_file; nil if no map.
func agyRules(r agent.Rule, cwd string) []string {
	if r.Arg == "" || strings.ContainsAny(r.Arg, "*?[") {
		return nil
	}
	if r.Tool == "Bash" {
		return []string{"command(" + strings.TrimSpace(r.Arg) + ")"}
	}
	fns := map[string][]string{"Read": {"read_file"}, "Write": {"write_file", "edit_file"}, "Edit": {"write_file", "edit_file"}}[r.Tool]
	p := r.Arg
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return nil
		}
		p = filepath.Join(cwd, p)
	}
	var out []string
	for _, fn := range fns {
		out = append(out, fn+"("+filepath.Clean(p)+")")
	}
	return out
}

// agyHooksPath is agy's global hooks.json, where renderGeminiHooks installs the permission gate.
func agyHooksPath(home string) string { return filepath.Join(home, ".gemini", "config", "hooks.json") }

// agySettingsPath is agy's settings.json, where renderGeminiSettings writes the guard denies.
func agySettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// agyGateTools are the agy tools the permission gate must judge to enforce a Deny on each rule tool (none: unenforceable).
var agyGateTools = map[string][]string{"Bash": {"run_command"}, "Read": {"read_file"}, "Write": {"write_file", "edit_file"}, "Edit": {"write_file", "edit_file"}}

// agyGateCovers reports whether the global or the workspace's .agents/hooks.json runs the permission gate on every tool.
func agyGateCovers(cwd string, tools ...string) bool {
	if len(tools) == 0 {
		return false
	}
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	var files []string
	if home, err := agentconfig.HomeOr(""); err == nil { // no home: never fall back to a CWD-relative hooks.json
		files = append(files, agyHooksPath(home))
	}
	if cwd != "" {
		files = append(files, filepath.Join(cwd, ".agents", "hooks.json"))
	}
	for _, p := range files {
		groups := agyPreToolGroups(p)
		if !slices.ContainsFunc(tools, func(tool string) bool { return !agyGroupsRunGate(groups, command, tool) }) {
			return true
		}
	}
	return false
}

// agyPreToolGroups is every PreToolUse group in the hooks.json at path; nil when it cannot be read or decoded.
func agyPreToolGroups(path string) []settings.MatcherGroup {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sets map[string]struct {
		Enabled    *bool                   `json:"enabled"` // a set with enabled false does not run; a non-boolean fails the decode
		PreToolUse []settings.MatcherGroup `json:"PreToolUse"`
	}
	if json.Unmarshal(b, &sets) != nil {
		return nil
	}
	var groups []settings.MatcherGroup
	for _, set := range sets {
		if set.Enabled != nil && !*set.Enabled {
			continue
		}
		groups = append(groups, set.PreToolUse...)
	}
	return groups
}

// agyGroupsRunGate reports whether a blocking group on tool runs the gate.
func agyGroupsRunGate(groups []settings.MatcherGroup, command, tool string) bool {
	return slices.ContainsFunc(groups, func(g settings.MatcherGroup) bool { return g.RunsGate("gemini", command, tool) })
}

// agyHookName is the one named hook in agy's hooks.json the harness owns; every render replaces it.
const agyHookName = "claude-code"

// agyGateMatcher is the agy tool names the permission gate judges (run_command, read_file, write_file, edit_file).
const agyGateMatcher = "run_command|read_file|write_file|edit_file"

// agyShellMatcher is agy's shell tool name, the only call safety-scan judges.
const agyShellMatcher = "run_command"

// renderGeminiHooks merges the harness's agy PreToolUse hooks into ~/.gemini/config/hooks.json, keeping every other hook.
func renderGeminiHooks(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	var groups []any
	for _, s := range settings.SpecsFor("gemini", "native") {
		matcher := agyGateMatcher
		if s.Matcher == "Bash" { // a shell-only spec runs on run_command alone
			matcher = agyShellMatcher
		}
		groups = append(groups, map[string]any{"matcher": matcher, "hooks": []any{map[string]any{
			"type": "command", "command": settings.AgentCommand("gemini", s), "timeout": s.Seconds(),
		}}})
	}
	path := agyHooksPath(home)
	return agentconfig.CommitJSONObject(path, o, out, "gemini-hooks", func(obj map[string]any, _ bool) bool {
		obj[agyHookName] = map[string]any{"PreToolUse": groups}
		return true
	})
}
