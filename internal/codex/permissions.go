package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
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
	return err == nil && strings.Contains(string(b), "--permission-gate")
}

// codexRender maps p onto codex; a Deny it cannot enforce is an error naming the rule, so nothing runs unguarded.
// gate means the permission-gate hook judges every Bash call, so a Bash Deny that is no command prefix is left to it.
func codexRender(p agent.Permissions, tools, gate bool) (codexPolicy, error) {
	var pol codexPolicy
	if p.Mode == "plan" && tools {
		return pol, errors.New("codex: permission mode plan denies unmatched calls, which codex cannot enforce with --tools")
	}
	var b strings.Builder
	sets := []struct {
		rules    []agent.Rule
		decision string
	}{{p.Deny, "forbidden"}, {p.Ask, "prompt"}, {p.Allow, "allow"}}
	if !tools {
		sets = sets[:1] // no tools: an allow/prompt rule could auto-approve what the dispatch should not run
	}
	for _, set := range sets {
		for _, r := range set.rules {
			line, err := codexRule(r, set.decision, tools)
			if errors.As(err, &notPrefixError{}) && set.decision == "forbidden" && gate {
				continue
			}
			if err != nil && set.decision == "forbidden" {
				return pol, err
			}
			if err != nil {
				pol.unmapped = append(pol.unmapped, r.String())
			}
			b.WriteString(line)
		}
	}
	pol.rules = b.String()
	pol.approve = p.Mode == "auto" || len(p.Ask) > 0
	return pol, nil
}

// codexRule is one rules line; "" with nil error means the read-only sandbox already covers a write-tool deny.
func codexRule(r agent.Rule, decision string, tools bool) (string, error) {
	if r.Tool != "Bash" {
		if decision == "forbidden" && !tools && slices.Contains(codexWriteTools, r.Tool) {
			return "", nil
		}
		return "", fmt.Errorf("codex: cannot enforce %s %s (codex has only Bash prefix rules and a read-only sandbox)", decision, r)
	}
	prefix, wild := strings.CutSuffix(r.Arg, ":*")
	if !wild {
		prefix, wild = strings.CutSuffix(r.Arg, " *") // Claude's trailing wildcard, same as ":*"
	}
	if strings.Contains(prefix, "*") {
		return "", notPrefixError{fmt.Sprintf("codex: cannot enforce %s %s (a mid-pattern * is not a prefix rule)", decision, r)}
	}
	toks := shell.Tokens(prefix)
	if r.Arg == "" || len(toks) == 0 || len(shell.Segments(prefix)) != 1 || strings.ContainsAny(prefix, "<>`$()|&;") {
		return "", notPrefixError{fmt.Sprintf("codex: cannot render %s as a command prefix", r)}
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
		auth := filepath.Join(home, "auth.json")
		if fresh, err := os.ReadFile(auth); err == nil && !isLink(auth) {
			if err := paths.AtomicWrite(filepath.Join(real, "auth.json"), fresh, 0o600); err != nil { // codex refreshed the tokens in place
				fmt.Fprintf(os.Stderr, "codex: refreshed auth.json not saved to %s: %v\n", real, err)
			}
		}
		_ = os.RemoveAll(home)
	}
	if err = linkHome(real, home, rules); err != nil {
		remove()
		return "", nil, err
	}
	return home, remove, nil
}

// linkHome links every entry of real into home except rules/, then writes home/rules/ with the user's files and rules.
func linkHome(real, home, rules string) error {
	entries, _ := os.ReadDir(real)
	user, _ := filepath.Glob(filepath.Join(real, "rules", "*.rules"))
	links := make([]string, 0, len(entries)+len(user))
	for _, e := range entries {
		if !slices.Contains([]string{"rules", ".tmp", "tmp", "config.toml"}, e.Name()) {
			links = append(links, e.Name())
		}
	}
	for _, name := range links {
		if err := os.Symlink(filepath.Join(real, name), filepath.Join(home, name)); err != nil {
			return err
		}
	}
	cfg, err := retargetedConfig(real, home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), cfg, 0o600); err != nil {
			return err
		}
	}
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

// codexApplyPermissions renders req.Permissions onto the exec argv/env. The zero Permissions returns both untouched.
func codexApplyPermissions(req agent.RunOptions, argv, env []string) (outArgv, outEnv []string, remove func(), err error) {
	remove = func() {}
	p := req.Permissions
	if p.IsZero() {
		return argv, env, remove, nil
	}
	pol, err := codexRender(p, req.Tools, codexGateInstalled())
	if err != nil {
		return nil, nil, remove, err
	}
	for _, r := range pol.unmapped {
		fmt.Fprintf(os.Stderr, "ask: codex cannot enforce permission rule %s\n", r)
	}
	if pol.approve {
		argv = codexSwapSandbox(argv)
	}
	if pol.rules == "" {
		return argv, env, remove, nil
	}
	if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, CodexKeyEnv+"=") }) {
		if _, serr := os.Stat(filepath.Join(codexHome(), "auth.json")); serr != nil {
			return nil, nil, remove, errors.New("codex: Bash permission rules need a per-dispatch CODEX_HOME, which loses the keyring-backed ChatGPT sign-in; use a provider route")
		}
	}
	home, remove, err := codexRulesHome(pol.rules)
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("codex: permission rules: %w", err)
	}
	return argv, append(slices.Clone(env), "CODEX_HOME="+home), remove, nil
}
