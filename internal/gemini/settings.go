package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/settings"
)

// renderGeminiSettings merges the spec's exact-match guard denies into agy's
// ~/.gemini/antigravity-cli/settings.json permissions.deny, preserving every other key.
func renderGeminiSettings(home string, o render.RenderOptions, out io.Writer) (bool, error) {
	cfg, err := agentconfig.LoadConfig()
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: load agentconfig: %w", err)
	}
	if !cfg.Agent("gemini").Permission.UseGuardRules {
		return false, nil
	}
	rules := agyGuardRules(cfg.Guard().Deny, out)
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	return render.CommitJSONObject(path, o, out, "gemini-settings", func(obj map[string]any, existed bool) bool {
		if len(rules) == 0 && !existed {
			return false // nothing to deny: don't create agy's settings.json just to write an empty list
		}
		perms, _ := obj["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		existing, _ := perms["deny"].([]any)
		perms["deny"] = mergeAgyDeny(existing, rules)
		obj["permissions"] = perms
		return true
	})
}

// agyGuardRules converts exact-match Bash(...) denies to agy command(...) rules, noting the rest.
func agyGuardRules(deny []string, out io.Writer) []string {
	var rules []string
	for _, r := range deny {
		cmd, ok := strings.CutPrefix(r, "Bash(")
		cmd, ok2 := strings.CutSuffix(cmd, ")")
		if !ok || !ok2 || strings.HasSuffix(cmd, ":*") {
			fmt.Fprintf(out, "gemini-settings: note: %q is not exact-match; agy denies exact commands only, skipped\n", r)
			continue
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

// agyPermissionSettings renders p as agy's per-run --settings JSON; guard denies always ride along, since a --settings
// permissions block may replace the global one. Ask runs as Allow (agy has no headless judge). A Deny agy can't hold
// natively or through the permission-gate hook is an error (Deny is the hard boundary); unmapped Allow/Ask go to Emulated.
func agyPermissionSettings(p agent.Permissions, tools bool, cwd string, out io.Writer) (settings string, unmapped []string, err error) {
	if p.IsZero() {
		return "", nil, nil
	}
	if p.Mode == "plan" && tools { // plan denies unmatched calls; agy has no deny-all, so --settings would run every tool
		return "", nil, &retry.Error{Kind: retry.KindFatal, Agent: "gemini", Err: errors.New("permission mode plan denies unmatched calls, which agy cannot enforce with --tools")}
	}
	cfg, err := loadAgentConfig()
	if err != nil {
		return "", nil, fmt.Errorf("agy: guard denies for --settings: %w", err)
	}
	var deny []string
	if cfg.Agent("gemini").Permission.UseGuardRules {
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
		return "", nil, fmt.Errorf("agy cannot enforce deny rule(s) %s: the permission-gate hook does not cover them in %s; run `claude-code configure --restore --agents --agent gemini`: dispatch refused",
			strings.Join(agent.RuleStrings(unenforced), ", "), agyHooksPath())
	}
	allow, unAllow := appendAgyRules(nil, slices.Concat(p.Allow, p.Ask), cwd)
	for _, r := range p.Ask {
		fmt.Fprintf(out, "agy: ask rule %s runs as allow (no --mode auto judge)\n", r)
	}
	perms := map[string][]string{"deny": mergeAgyDeny(nil, deny)}
	if len(allow) > 0 {
		perms["allow"] = mergeAgyDeny(nil, allow)
	}
	b, err := json.Marshal(map[string]any{"permissions": perms})
	if err != nil {
		return "", nil, fmt.Errorf("agy: encode --settings: %w", err)
	}
	return string(b), append([]string{}, agent.RuleStrings(unAllow)...), nil
}

// appendAgyRules appends each rule agy can express natively to dst and names the rest.
func appendAgyRules(dst []string, rules []agent.Rule, cwd string) ([]string, []agent.Rule) {
	var unmapped []agent.Rule
	for _, r := range rules {
		if s, ok := agyRule(r, cwd); ok {
			dst = append(dst, s)
		} else {
			unmapped = append(unmapped, r)
		}
	}
	return dst, unmapped
}

// agyRule maps an exact Bash command or a glob-free Read/Write/Edit path; agy's `*` did not match live, so no patterns.
func agyRule(r agent.Rule, cwd string) (string, bool) {
	if r.Arg == "" || strings.ContainsAny(r.Arg, "*?[") {
		return "", false
	}
	if r.Tool == "Bash" {
		return "command(" + strings.TrimSpace(r.Arg) + ")", true
	}
	fn := map[string]string{"Read": "read_file", "Write": "write_file", "Edit": "write_file"}[r.Tool]
	if fn == "" {
		return "", false
	}
	p := r.Arg
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return "", false
		}
		p = filepath.Join(cwd, p)
	}
	return fn + "(" + filepath.Clean(p) + ")", true
}

// agyHooksPath is agy's global hooks.json, where renderGeminiHooks installs the permission gate.
func agyHooksPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "config", "hooks.json")
}

// agyGateTools are the agy tools the permission gate must judge to enforce a Deny on each rule tool (none: unenforceable).
var agyGateTools = map[string][]string{"Bash": {"run_command"}, "Read": {"read_file"}, "Write": {"write_file", "edit_file"}, "Edit": {"write_file", "edit_file"}}

// agyGateCovers reports whether the global or the workspace's .agents/hooks.json runs the permission gate on every tool.
func agyGateCovers(cwd string, tools ...string) bool {
	if len(tools) == 0 {
		return false
	}
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	for _, p := range []string{agyHooksPath(), filepath.Join(cwd, ".agents", "hooks.json")} {
		if !slices.ContainsFunc(tools, func(tool string) bool { return !agyFileRunsGate(p, command, tool) }) {
			return true
		}
	}
	return false
}

// agyFileRunsGate reports whether the hooks.json at path has a blocking PreToolUse group on tool running the gate.
func agyFileRunsGate(path, command, tool string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var sets map[string]struct {
		PreToolUse []settings.MatcherGroup `json:"PreToolUse"`
	}
	if json.Unmarshal(b, &sets) != nil {
		return false
	}
	for _, set := range sets {
		for _, group := range set.PreToolUse {
			if agyMatchesTool(group.Matcher, tool) && slices.ContainsFunc(group.Hooks, func(h settings.Hook) bool { return isAgyGateHook(h, command) }) {
				return true
			}
		}
	}
	return false
}

// agyMatchesTool reports whether a hook group's matcher selects tool (empty selects all).
func agyMatchesTool(matcher, tool string) bool {
	if matcher == "" {
		return true
	}
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	return err == nil && re.MatchString(tool)
}

// isAgyGateHook reports whether hook is the synchronous permission-gate command agy runs.
func isAgyGateHook(hook settings.Hook, command string) bool {
	cmd := strings.TrimSpace(hook.Command)
	return hook.Type == "command" && !hook.Async && (cmd == command || cmd == settings.AgentEnv+"=gemini "+command)
}
