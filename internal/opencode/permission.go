package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	"WebFetch": {"webfetch", false, false}, "Agent": {"task", true, false},
}

// opencodeEntry is one pattern/action pair; opencode is last-match-wins, so order is the policy.
type opencodeEntry struct{ pattern, action string }

// opencodePermission renders p as opencode's permission JSON (ordered keys: the order is the precedence).
// Unmatched calls are "ask" (mode auto: "allow") and ask goes to opencode's --auto judge (D3); plan denies unlisted.
func opencodePermission(p agent.Permissions) (json.RawMessage, error) {
	base := map[string]string{"auto": "allow", "plan": "deny"}[p.Mode]
	if base == "" {
		base = "ask"
	}
	byKey := planEntries(p.Mode)
	sets := []struct {
		rules  []agent.Rule
		action string
	}{{p.Allow, "allow"}, {p.Ask, "ask"}, {p.Deny, "deny"}}
	for _, set := range sets {
		for _, r := range set.rules {
			key, pats, err := opencodeRule(r, set.action)
			if err != nil {
				return nil, err
			}
			if p.Mode == "plan" && set.action != "deny" && planDenied[key] {
				continue // plan mode is read-only: an allow or ask never reopens a write tool
			}
			for _, pat := range pats {
				byKey[key] = appendEntry(byKey[key], opencodeEntry{pat, set.action})
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b bytes.Buffer
	b.WriteString(`{"*":`)
	b.WriteString(quote(base))
	for _, k := range keys {
		b.WriteString(",")
		b.WriteString(quote(k))
		b.WriteString(":")
		writeEntries(&b, byKey[k])
	}
	b.WriteString("}")
	return b.Bytes(), nil
}

// planDenied are the keys plan mode keeps closed; the rest of opencode's read-only tools are open unless a Deny matches.
var planDenied = map[string]bool{"edit": true, "bash": true, "task": true}

// planEntries seeds plan mode's read-only policy: read tools allow, write tools deny, other tools fall to "*".
func planEntries(mode string) map[string][]opencodeEntry {
	byKey := map[string][]opencodeEntry{}
	if mode != "plan" {
		return byKey
	}
	for _, k := range []string{"read", "glob", "grep", "list", "webfetch"} {
		byKey[k] = []opencodeEntry{{"*", "allow"}}
	}
	for k := range planDenied {
		byKey[k] = []opencodeEntry{{"*", "deny"}}
	}
	return byKey
}

// appendEntry adds e after dropping an earlier entry for the same pattern, so the later decision is the one that stands.
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

// opencodeCommand turns Claude's "git log:*" prefix form into opencode's "git log *".
func opencodeCommand(arg string) string {
	if prefix, ok := strings.CutSuffix(arg, ":*"); ok {
		return prefix + " *"
	}
	return arg
}

// opencodeApplyPermissions sets req's policy as the config's permission and pins OPENCODE_PERMISSION to it,
// so the global "permission":"allow" of ~/.config/opencode/opencode.json is not inherited. No policy leaves env as is.
func opencodeApplyPermissions(env []string, req agent.RunOptions) ([]string, error) {
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
	}
	if req.Permissions.IsZero() {
		return env, nil
	}
	if err := refusePureBashDeny(req); err != nil {
		return nil, err
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

// refusePureBashDeny: --pure loads no plugin, so nothing covers the `bash -c` bypass of a Bash Deny.
func refusePureBashDeny(req agent.RunOptions) error {
	if r, ok := agent.SafeModeDeny(req, func(r agent.Rule) bool { return r.Tool == "Bash" }); ok {
		return retry.Fatal("opencode",
			fmt.Errorf("cannot enforce deny %s under --safe-mode (--pure loads no plugin; bash -c bypasses its matcher)", r))
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
