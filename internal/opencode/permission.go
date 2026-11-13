package opencode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agent"
)

// opencodeTools maps a rule's tool to opencode's permission key; pathed tools take path patterns.
var opencodeTools = map[string]struct {
	key   string
	args  bool // the tool's patterns carry an argument (a command, path or subagent name)
	pathy bool
}{
	"Bash": {"bash", true, false}, "Edit": {"edit", true, true}, "Write": {"edit", true, true},
	"Read": {"read", true, true}, "Glob": {"glob", false, false}, "Grep": {"grep", false, false},
	"WebFetch": {"webfetch", false, false}, "WebSearch": {"websearch", false, false}, "Agent": {"task", true, false},
}

// opencodeEntry is one pattern/action pair; opencode is last-match-wins, so order is the policy.
type opencodeEntry struct{ pattern, action string }

// opencodePermission preserves rule precedence: unmatched ask uses --auto; auto allows and plan denies unlisted.
func opencodePermission(p agent.Permissions) (json.RawMessage, error) {
	base := cmp.Or(map[string]string{"auto": "allow", "plan": "deny"}[p.Mode], "ask")
	byKey := planEntries(p.Mode)
	sets := []struct {
		rules  []agent.Rule
		action string
	}{{p.Allow, "allow"}, {p.Ask, "ask"}, {p.Deny, "deny"}}
	for _, set := range sets {
		for _, r := range set.rules {
			key, pats, err := opencodeRuleOrList(r, set.action)
			if err != nil {
				return nil, err
			}
			if reopensPlanWrite(p.Mode, set.action, key, r) {
				continue // plan mode is read-only: an allow or ask never reopens a write tool
			}
			for _, pat := range pats {
				byKey[key] = appendEntry(byKey[key], opencodeEntry{pat, set.action})
			}
		}
	}
	return renderOpencodePermission(base, byKey), nil
}

// reopensPlanWrite reports a non-deny rule that would open a write tool plan mode keeps closed.
func reopensPlanWrite(mode, action, key string, r agent.Rule) bool {
	return mode == "plan" && action != "deny" && planDenied[key] && r != agent.ReadOnlyAllow
}

func renderOpencodePermission(base string, byKey map[string][]opencodeEntry) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"*":`)
	b.WriteString(quote(base))
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		b.WriteString(",")
		b.WriteString(quote(k))
		b.WriteString(":")
		writeEntries(&b, byKey[k])
	}
	b.WriteString("}")
	return b.Bytes()
}

// planDenied keeps write tools closed in plan mode; read-only tools allow unless a Deny matches.
var planDenied = map[string]bool{"edit": true, "bash": true, "task": true}

// planEntries seeds plan mode's read-only policy: read tools allow, write tools deny, other tools fall to "*".
func planEntries(mode string) map[string][]opencodeEntry {
	byKey := map[string][]opencodeEntry{}
	if mode != "plan" {
		return byKey
	}
	for _, k := range []string{"read", "glob", "grep", "list", "webfetch", "websearch"} {
		byKey[k] = []opencodeEntry{{"*", "allow"}}
	}
	for k := range planDenied {
		byKey[k] = []opencodeEntry{{"*", "deny"}}
	}
	return byKey
}

// appendEntry replaces an earlier matching pattern so the later decision wins.
func appendEntry(list []opencodeEntry, e opencodeEntry) []opencodeEntry {
	list = slices.DeleteFunc(list, func(x opencodeEntry) bool { return x.pattern == e.pattern })
	return append(list, e)
}

func writeEntries(b *bytes.Buffer, list []opencodeEntry) {
	if len(list) == 1 && list[0].pattern == "*" {
		b.WriteString(quote(list[0].action))
		return
	}
	b.WriteString("{")
	for i, e := range list {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(quote(e.pattern))
		b.WriteString(":")
		b.WriteString(quote(e.action))
	}
	b.WriteString("}")
}

func quote(s string) string {
	out, _ := json.Marshal(s) // a string always marshals
	return string(out)
}

// opencodeRule maps r to opencode's permission key and patterns; a rule it cannot express is an error naming it.
func opencodeRule(r agent.Rule, action string) (key string, patterns []string, err error) {
	t, ok := opencodeTools[r.Tool]
	if !ok {
		return "", nil, fmt.Errorf("opencode: cannot express %s %s (no opencode permission for tool %s)", action, r, r.Tool)
	}
	// NOTE: `bash -c "..."` bypasses this matcher; the harness plugin's permission-gate enforces Bash Deny via Decide.
	if r.Arg == "" {
		return t.key, []string{"*"}, nil
	}
	if !t.args {
		return "", nil, fmt.Errorf("opencode: cannot express %s %s (opencode %s permission has no argument patterns)", action, r, t.key)
	}
	if !t.pathy {
		if r.Tool == "Bash" {
			r.Arg = opencodeCommand(r.Arg)
		}
		return t.key, []string{r.Arg}, nil
	}
	// opencode's * also crosses directories: that over-matches a deny, but would widen an allow.
	if action == "allow" && strings.ContainsAny(r.Arg, "*?") {
		return "", nil, fmt.Errorf("opencode: cannot express %s %s (a path wildcard would match more directories than Claude's)", action, r)
	}
	arg := strings.NewReplacer("**/", "*", "**", "*").Replace(strings.TrimPrefix(r.Arg, "./"))
	return t.key, opencodePaths(arg, action), nil
}

// opencodePaths spells a path pattern the ways opencode may see it: it matches the project-relative path, which is
// the absolute path minus its slash outside a git project, so a deny on a relative path also covers "*/arg".
func opencodePaths(arg, action string) []string {
	switch {
	case strings.HasPrefix(arg, "/"):
		return []string{arg, arg[1:]}
	case action == "deny" && !strings.HasPrefix(arg, "*"):
		return []string{arg, "*/" + arg}
	}
	return []string{arg}
}

// opencodeRuleOrList is opencodeRule, but the read-only shell rules become the list's heads (the plugin judges exactly).
func opencodeRuleOrList(r agent.Rule, action string) (string, []string, error) {
	switch {
	case r == agent.ReadOnlyAllow:
		return "bash", readOnlyPatterns(), nil
	case r.Tool == "Bash" && r.Arg == agent.NotReadOnlyShellArg:
		return "bash", nil, nil
	}
	return opencodeRule(r, action)
}

// readOnlyPatterns are the native bash patterns of the shared read-only list.
func readOnlyPatterns() []string {
	var out []string
	for _, h := range agent.ReadOnlyBashHeads() {
		out = append(out, h, h+" *")
	}
	return out
}

// opencodeCommand turns Claude's "git log:*" prefix form into opencode's "git log *".
func opencodeCommand(arg string) string {
	if prefix, ok := strings.CutSuffix(arg, ":*"); ok {
		return prefix + " *"
	}
	return arg
}

// opencodeApplyPermissions pins config and permission env over global policy; no policy leaves env unchanged.
func opencodeApplyPermissions(env []string, req agent.RunOptions) ([]string, error) {
	if err := refusePureGates(req); err != nil {
		return nil, err
	}
	if !req.Tools {
		return env, nil
	}
	if req.ReadOnly {
		req.Permissions.Deny = slices.Clone(req.Permissions.Deny)
		for _, r := range agent.ReadOnlyDeny {
			if !slices.Contains(req.Permissions.Deny, r) {
				req.Permissions.Deny = append(req.Permissions.Deny, r)
			}
		}
		req.Permissions.Allow = append(slices.Clone(req.Permissions.Allow), agent.ReadOnlyAllow)
	}
	if req.Permissions.IsZero() {
		return env, nil
	}
	if opencodeReadOnly(req) {
		req.Permissions.Mode = "plan"
	}
	perm, err := opencodePermission(req.Permissions)
	if err != nil {
		return nil, err
	}
	env, err = mutateOpencodeConfigEnv(env, false, func(cfg map[string]any) { cfg["permission"] = perm })
	if err != nil {
		return nil, err
	}
	return append(env, opencodePermissionEnv+"="+string(perm)), nil
}

// refusePolicyEnv refuses --env setting a name the permission policy owns: --env goes last and would replace it.
func refusePolicyEnv(req agent.RunOptions) error {
	if req.Tools && !req.ReadOnly && !req.SafeMode && req.Permissions.IsZero() {
		return nil
	}
	for _, kv := range req.Env {
		if k, _, _ := strings.Cut(kv, "="); k == opencodePermissionEnv || k == opencodeConfigEnv {
			return retry.Fatal("opencode", fmt.Errorf("--env %s: the permission policy owns it; drop it from --env", k))
		}
	}
	return nil
}

// refusePureGates: --pure loads no plugin, so a gate hook or a Bash Deny could not stop a call.
func refusePureGates(req agent.RunOptions) error {
	if !req.SafeMode {
		return nil
	}
	if gates := pureGates(); len(gates) > 0 {
		return retry.Fatal("opencode", fmt.Errorf("cannot run under --safe-mode: --pure loads no plugin, so gate hook(s) %s could not stop a call; run without --safe-mode",
			strings.Join(gates, ", ")))
	}
	if r, ok := agent.SafeModeDeny(req, func(r agent.Rule) bool { return r.Tool == "Bash" }); ok {
		return retry.Fatal("opencode",
			fmt.Errorf("cannot enforce deny %s under --safe-mode (--pure loads no plugin; bash -c bypasses its matcher); run without --safe-mode", r))
	}
	return nil
}

// opencodeReadOnly also recognizes emulated denies when RenderSettings carries permissions without the ReadOnly flag.
func opencodeReadOnly(req agent.RunOptions) bool {
	if req.ReadOnly {
		return true
	}
	for _, r := range agent.ReadOnlyDeny {
		if !slices.Contains(req.Permissions.Deny, r) {
			return false
		}
	}
	return true
}

// GatePolicy is the policy the plugin's permission-gate judges for req, plan mode included when read-only.
func GatePolicy(req agent.RunOptions) agent.Permissions {
	if opencodeReadOnly(req) {
		req.Permissions.Mode = "plan"
	}
	return req.Permissions
}

// opencodePolicyArgv keeps read-only off --auto; other policies send "ask" to opencode's auto judge.
func opencodePolicyArgv(argv []string, req agent.RunOptions) []string {
	if opencodeReadOnly(req) {
		n := len(flagsOnly(argv))
		return append(slices.DeleteFunc(slices.Clone(argv[:n]), func(arg string) bool { return arg == "--auto" }), argv[n:]...)
	}
	if !req.Tools || req.Permissions.IsZero() || slices.Contains(flagsOnly(argv), "--auto") {
		return argv
	}
	return slices.Insert(slices.Clone(argv), min(1, len(argv)), "--auto")
}

// flagsOnly is argv up to the "--" separator: a prompt after it is data, never a flag.
func flagsOnly(argv []string) []string {
	if i := slices.Index(argv, "--"); i >= 0 {
		return argv[:i]
	}
	return argv
}
