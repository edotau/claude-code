package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shell"
)

// codexWriteTools are the tools the read-only sandbox already blocks.
var codexWriteTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// codexPolicy is a dispatch's permissions in codex's form: the .rules body, the approval flag, and rules left out.
type codexPolicy struct {
	rules    string
	approve  bool     // --approve-for-me
	unmapped []string // Allow/Ask rules codex cannot express; logged, not enforced
}

// notPrefixError is a Bash rule that is no command prefix; with the permission-gate hook installed that Deny is the hook's.
type notPrefixError struct{ msg string }

func (e notPrefixError) Error() string { return e.msg }

// codexGateInstalled reports whether the codex home's hooks.json runs the permission gate.
func codexGateInstalled() bool {
	b, err := os.ReadFile(filepath.Join(codexHome(), "hooks.json"))
	if err != nil {
		return false
	}
	var config struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if json.Unmarshal(b, &config) != nil {
		return false
	}
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	for _, group := range config.Hooks[settings.EventPreToolUse] {
		if matchesBash(group.Matcher) && slices.ContainsFunc(group.Hooks, func(h settings.Hook) bool { return isGateHook(h, command) }) {
			return true
		}
	}
	return false
}

// matchesBash reports whether a hook group's matcher selects the Bash tool (empty selects all).
func matchesBash(matcher string) bool {
	if matcher == "" {
		return true
	}
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	return err == nil && re.MatchString("Bash")
}

// isGateHook reports whether hook is the synchronous permission-gate command.
func isGateHook(hook settings.Hook, command string) bool {
	cmd := strings.TrimSpace(hook.Command)
	return hook.Type == "command" && !hook.Async && (cmd == command || cmd == settings.AgentEnv+"=codex "+command)
}

// codexRender maps p onto codex; a Deny it cannot enforce is an error naming the rule, so nothing runs unguarded.
// gate means the permission-gate hook judges every Bash call, so a Bash Deny that is no command prefix is left to it.
func codexRender(p agent.Permissions, tools, gate bool) (codexPolicy, error) {
	var pol codexPolicy
	if p.Mode == "plan" && tools {
		return pol, errors.New("codex: permission mode plan denies unmatched calls, which codex cannot enforce with --tools")
	}
	rules, unmapped, err := codexRenderRules(p, tools, gate)
	if err != nil {
		return pol, err
	}
	pol.unmapped = unmapped
	pol.rules = rules
	pol.approve = p.Mode == "auto" || len(p.Ask) > 0
	return pol, nil
}

// codexRenderRules renders each rule of p to its codex line, collecting the rules codex cannot express.
func codexRenderRules(p agent.Permissions, tools, gate bool) (rules string, unmapped []string, err error) {
	var b strings.Builder
	p.Each(func(decision agent.Decision, r agent.Rule) {
		if err != nil || (!tools && decision != agent.Deny) {
			return // no tools: an allow/prompt rule could auto-approve what the dispatch should not run
		}
		line, lerr := codexRule(r, [...]string{"forbidden", "prompt", "allow"}[decision], tools)
		if errors.As(lerr, &notPrefixError{}) && decision == agent.Deny && gate {
			return
		}
		if lerr != nil && decision == agent.Deny {
			err = lerr
			return
		}
		if lerr != nil {
			unmapped = append(unmapped, r.String())
		}
		b.WriteString(line)
	})
	return b.String(), unmapped, err
}

// codexRule is one rules line; "" with nil error means the read-only sandbox already covers a write-tool deny.
func codexRule(r agent.Rule, decision string, tools bool) (string, error) {
	if r.Tool != "Bash" {
		if decision == "forbidden" && !tools && slices.Contains(codexWriteTools, r.Tool) {
			return "", nil
		}
		return "", fmt.Errorf("codex: cannot enforce %s %s (codex has only Bash prefix rules and a read-only sandbox)", decision, r)
	}
	toks, wild, err := bashPrefixTokens(r, decision)
	if err != nil {
		return "", err
	}
	if !wild && decision != "forbidden" {
		return "", fmt.Errorf("codex: %s is an exact command; a prefix rule would match more", r) // only Deny may over-match
	}
	pat := make([]string, len(toks))
	for i, t := range toks {
		pat[i] = strconv.Quote(t)
	}
	return fmt.Sprintf("prefix_rule(pattern=[%s], decision=%q, justification=%s)\n", strings.Join(pat, ", "), decision,
		strconv.Quote("dispatch permission "+r.String())), nil
}

// bashPrefixTokens splits a Bash rule into command-prefix tokens; wild reports a trailing wildcard.
func bashPrefixTokens(r agent.Rule, decision string) (toks []string, wild bool, err error) {
	prefix, wild := strings.CutSuffix(r.Arg, ":*")
	if !wild {
		prefix, wild = strings.CutSuffix(r.Arg, " *") // Claude's trailing wildcard, same as ":*"
	}
	if strings.Contains(prefix, "*") {
		return nil, false, notPrefixError{fmt.Sprintf("codex: cannot enforce %s %s (a mid-pattern * is not a prefix rule)", decision, r)}
	}
	toks = shell.Tokens(prefix)
	if r.Arg == "" || len(toks) == 0 || len(shell.Segments(prefix)) != 1 || strings.ContainsAny(prefix, "<>`$()|&;") {
		return nil, false, notPrefixError{fmt.Sprintf("codex: cannot render %s as a command prefix", r)}
	}
	return toks, wild, nil
}

// codexSwapSandbox swaps `--sandbox workspace-write` for --approve-for-me (they conflict; it implies that sandbox).
func codexSwapSandbox(argv []string) []string {
	i := slices.Index(argv, "--sandbox")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != "workspace-write" {
		return argv
	}
	return slices.Concat(argv[:i], []string{"--approve-for-me"}, argv[i+2:])
}

// codexRulesHome makes a CODEX_HOME under the state dir linking the real home's entries, with its own rules/.
func codexRulesHome(rules string) (home string, remove func(), err error) {
	real := codexHome()
	root := filepath.Join(paths.StateDir(), "codex")
	if err = os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	if home, err = os.MkdirTemp(root, "home-"); err != nil {
		return "", nil, err
	}
	remove = func() {
		if !saveRefreshedAuth(real, home) {
			return // keep home: its auth.json is the only copy of the refreshed login
		}
		_ = os.RemoveAll(home)
	}
	if err = linkHome(real, home, rules); err != nil {
		remove()
		return "", nil, err
	}
	return home, remove, nil
}

// saveRefreshedAuth copies a codex-refreshed auth.json from home back to real; false means the copy failed.
func saveRefreshedAuth(real, home string) bool {
	auth := filepath.Join(home, "auth.json")
	if isLink(auth) {
		return true
	}
	fresh, err := os.ReadFile(auth)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if err := paths.AtomicWrite(filepath.Join(real, "auth.json"), fresh, 0o600); err != nil { // codex refreshed the tokens in place
		fmt.Fprintf(os.Stderr, "codex: refreshed auth.json not saved to %s (kept in %s): %v\n", real, home, err)
		return false
	}
	return true
}

// linkHome links every entry of real into home except rules/, then writes home/rules/ with the user's files and rules.
func linkHome(real, home, rules string) error {
	entries, err := os.ReadDir(real)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	user, err := filepath.Glob(filepath.Join(real, "rules", "*.rules"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if slices.Contains([]string{"rules", ".tmp", "tmp", "config.toml"}, e.Name()) {
			continue
		}
		if err := os.Symlink(filepath.Join(real, e.Name()), filepath.Join(home, e.Name())); err != nil {
			return err
		}
	}
	if err := writeRetargetedConfig(real, home); err != nil {
		return err
	}
	return writeRulesDir(home, user, rules)
}

// writeRulesDir creates home/rules/ holding links to the user's rule files plus dispatch.rules.
func writeRulesDir(home string, user []string, rules string) error {
	if err := os.Mkdir(filepath.Join(home, "rules"), 0o700); err != nil {
		return err
	}
	for _, f := range user {
		if err := os.Symlink(f, filepath.Join(home, "rules", filepath.Base(f))); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(home, "rules", "dispatch.rules"), []byte(rules), 0o600)
}

// writeRetargetedConfig writes home/config.toml from real's; a missing real config is not an error.
func writeRetargetedConfig(real, home string) error {
	cfg, err := retargetedConfig(real, home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "config.toml"), cfg, 0o600)
}

// retargetedConfig is real's config.toml with each hook-trust key moved to home/hooks.json: codex keys trust by the
// hooks.json path under CODEX_HOME, so without it the linked hooks.json is untrusted and none of its hooks run.
func retargetedConfig(real, home string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(real, "config.toml"))
	if err != nil {
		return nil, err
	}
	from, to := CodexHookSourcePath(filepath.Join(real, "hooks.json"))+":", CodexHookSourcePath(filepath.Join(home, "hooks.json"))+":"
	return []byte(strings.ReplaceAll(string(raw), from, to)), nil
}

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// requireFileAuth errors when neither a provider key in env nor auth.json would survive a per-dispatch CODEX_HOME.
func requireFileAuth(env []string) error {
	if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, CodexKeyEnv+"=") }) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(codexHome(), "auth.json")); err != nil {
		return errors.New("codex: Bash permission rules need a per-dispatch CODEX_HOME, which loses the keyring-backed ChatGPT sign-in; use a provider route")
	}
	return nil
}

// codexCheckSafeMode rejects Deny rules under --safe-mode and logs the rules codex cannot enforce.
func codexCheckSafeMode(req agent.RunOptions, pol codexPolicy) error {
	if req.SafeMode && len(req.Permissions.Deny) > 0 && pol.rules != "" {
		return errors.New("codex: --safe-mode disables permission rules and hooks; remove --safe-mode to enforce --deny")
	}
	for _, r := range pol.unmapped {
		fmt.Fprintf(os.Stderr, "ask: codex cannot enforce permission rule %s\n", r)
	}
	return nil
}

// codexApplyPermissions renders req.Permissions onto the exec argv/env. The zero Permissions returns both untouched.
func codexApplyPermissions(req agent.RunOptions, argv, env []string) (outArgv, outEnv []string, remove func(), err error) {
	remove = func() {}
	p := req.Permissions
	if p.IsZero() {
		return argv, env, remove, nil
	}
	pol, err := codexRender(p, req.Tools, !req.SafeMode && codexGateInstalled())
	if err != nil {
		return nil, nil, remove, err
	}
	if err := codexCheckSafeMode(req, pol); err != nil {
		return nil, nil, remove, err
	}
	if pol.approve {
		argv = codexSwapSandbox(argv)
	}
	if pol.rules == "" {
		return argv, env, remove, nil
	}
	if err := requireFileAuth(env); err != nil {
		return nil, nil, remove, err
	}
	home, remove, err := codexRulesHome(pol.rules)
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("codex: permission rules: %w", err)
	}
	return argv, append(slices.Clone(env), "CODEX_HOME="+home), remove, nil
}
