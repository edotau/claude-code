package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
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

// notPrefixError is a Bash rule that is no command prefix; with the permission-gate hook installed that Deny is its
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
		if group.RunsGate("codex", command, "Bash") {
			return true
		}
	}
	return false
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
		if lerr != nil && decision != agent.Allow {
			err = lerr // an ask codex cannot enforce would run unprompted: refused like a deny
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
	if !wild && decision == "allow" {
		return "", fmt.Errorf("codex: %s is an exact command; a prefix rule would match more", r) // a deny or ask may over-match
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
		keepSessionsLink(home)
	}
	if err = linkHome(real, home, rules); err != nil {
		remove()
		return "", nil, err
	}
	return home, remove, nil
}

// keepSessionsLink removes home but its sessions link: the thread db records rollout paths under home, so a resume needs them.
func keepSessionsLink(home string) {
	sessions := filepath.Join(home, "sessions")
	if fi, err := os.Lstat(sessions); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		return // codex made a real sessions dir here (the real home had none): its rollouts live only in home
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.Name() != "sessions" {
			_ = os.RemoveAll(filepath.Join(home, e.Name()))
		}
	}
	if !isLink(sessions) {
		_ = os.RemoveAll(home) // no sessions link to anchor
	}
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
	user, err := userRuleFiles(filepath.Join(real, "rules"))
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

// userRuleFiles lists dir's *.rules paths; a missing dir is empty, an unreadable one an error (no Glob: it eats [ ]).
func userRuleFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".rules") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

// writeRulesDir creates home/rules/ holding links to the user's rule files plus dispatch.rules.
// The user's dispatch.rules is never linked: writing through that link would truncate the real file,
// so its content is prepended to the dispatch rules instead.
func writeRulesDir(home string, user []string, rules string) error {
	if err := os.Mkdir(filepath.Join(home, "rules"), 0o700); err != nil {
		return err
	}
	var own strings.Builder
	for _, f := range user {
		if filepath.Base(f) == "dispatch.rules" {
			body, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			own.Write(body)
			own.WriteString("\n")
			continue
		}
		if err := os.Symlink(f, filepath.Join(home, "rules", filepath.Base(f))); err != nil {
			return err
		}
	}
	own.WriteString(rules)
	return os.WriteFile(filepath.Join(home, "rules", "dispatch.rules"), []byte(own.String()), 0o600)
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
	from, to := codexHookSourcePath(filepath.Join(real, "hooks.json"))+":", codexHookSourcePath(filepath.Join(home, "hooks.json"))+":"
	return []byte(strings.ReplaceAll(string(raw), from, to)), nil
}

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// requireFileAuth errors when neither a provider key in env nor auth.json would survive a per-dispatch CODEX_HOME.
func requireFileAuth(env []string) error {
	if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, codexKeyEnv+"=") }) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(codexHome(), "auth.json")); err != nil {
		return errors.New("codex: Bash permission rules need a per-dispatch CODEX_HOME, which loses the keyring-backed ChatGPT sign-in; use a provider route")
	}
	return nil
}

// codexCheckSafeMode rejects Deny and Ask rules under --safe-mode and logs the rules codex cannot enforce.
func codexCheckSafeMode(req agent.RunOptions, pol codexPolicy) error {
	if _, ok := agent.SafeModeDeny(req, func(agent.Rule) bool { return pol.rules != "" }); ok {
		return errors.New("codex: --safe-mode disables permission rules and hooks; remove --safe-mode to enforce --deny")
	}
	if req.SafeMode && req.Tools && len(req.Permissions.Ask) > 0 {
		return fmt.Errorf("codex: --safe-mode disables permission rules and hooks, so %s would run unprompted; remove --safe-mode to enforce --ask",
			req.Permissions.Ask[0])
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

// codexDenial names the layer that refused a failed shell call and its real control. An OS block never offers an
// --allow (no rule overrides the sandbox), and a rule or hook block offers one only when Explain proves it repairs it.
func codexDenial(p agent.Permissions, command, output string, tools bool, sandbox string) (agent.Denial, bool) {
	call := agent.Rule{Tool: "Bash", Arg: codexShellCommand(command)}
	text := strings.ToLower(output)
	denial := func(layer, source, remedy string) (agent.Denial, bool) {
		return agent.Denial{Call: call, Layer: layer, Source: source, Remedy: remedy}, true
	}
	switch {
	case strings.Contains(text, "blocked by pretooluse hook"):
		d := p.Explain(call.Tool, call.Arg)
		if d.Source == "" {
			return denial("hook", "safety hook", "a safety block has no override; report it instead of retrying another way")
		}
		d.Call, d.Layer = call, "hook"
		return d, true
	case strings.Contains(text, "network access was denied by the codex sandbox"), strings.Contains(text, "network access was blocked by policy"):
		if !tools {
			return denial("os-sandbox", "read-only sandbox", "relaunch with --tools; the read-only sandbox has no network")
		}
		return denial("os-sandbox", "sandbox network", "set "+codexNetworkEnv+"=1 to open sandbox network (all egress)")
	case strings.Contains(text, "blocked by policy"):
		if d := p.Explain(call.Tool, call.Arg); d.Source == "permissions.deny" {
			d.Layer = "codex-rules"
			return d, true
		}
		return denial("codex-rules", "$CODEX_HOME/rules", "a rule file outside this dispatch forbids it; edit that rule")
	case containsAny(text, "operation not permitted", "read-only file system", "denied by sandbox", "sandbox denied"):
		switch {
		case !tools:
			return denial("os-sandbox", "read-only sandbox", "relaunch with --tools; the read-only sandbox cannot write")
		case sandbox == "workspace-write":
			return denial("os-sandbox", "workspace-write sandbox", "set "+codexSandboxEnv+"=danger-full-access (writes anywhere) only if the write is intended")
		}
		return denial("os-sandbox", sandbox+" sandbox", "the OS refused beyond the sandbox; no harness control repairs it")
	}
	return agent.Denial{}, false
}

func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}

// codexShellCommand is the command inside codex's `bash -lc '<cmd>'` wrapper, else command as given.
func codexShellCommand(command string) string {
	toks := shell.Tokens(command)
	for i := 0; i+1 < len(toks); i++ {
		if toks[i] == "-lc" || toks[i] == "-c" {
			return toks[i+1]
		}
	}
	return command
}

// denialSink forwards every event and, after a failed shell result or an answer that reports an OS refusal (codex
// emits no item for a sandbox-denied command), adds a warning naming the layer and its real control.
type denialSink struct {
	next     wire.Sink
	p        agent.Permissions
	tools    bool
	sandbox  string
	commands map[string]string // tool id -> command
	warned   bool              // one OS-sandbox warning per run
}

func (s *denialSink) Emit(ev wire.Event) error {
	if err := s.next.Emit(ev); err != nil {
		return err
	}
	d, ok := s.denial(ev)
	if !ok {
		return nil
	}
	s.warned = s.warned || d.Layer == "os-sandbox"
	what := d.Call.String()
	if d.Call.Arg == "" {
		what = "a command"
	}
	return s.next.Emit(wire.Event{Type: wire.EventProgress, Status: "warning",
		Text: fmt.Sprintf("codex: %s refused %s (%s): %s", d.Layer, what, d.Source, d.Remedy)})
}

// denial is the refusal ev reports, if any; a tool use is remembered so its result can name the command.
func (s *denialSink) denial(ev wire.Event) (d agent.Denial, ok bool) {
	switch {
	case ev.Type == wire.EventToolUse && ev.Tool != nil:
		s.commands[ev.Tool.ID] = ev.Text
	case ev.Type == wire.EventToolResult && ev.Tool != nil && !strings.HasPrefix(ev.Status, "completed"):
		d, ok = codexDenial(s.p, s.commands[ev.Tool.ID], ev.Text, s.tools, s.sandbox)
	case ev.Type == wire.EventText && !s.warned:
		if d, ok = codexDenial(s.p, "", ev.Text, s.tools, s.sandbox); ok && d.Layer != "os-sandbox" {
			ok = false // prose names a refusal only by its OS signature
		}
	}
	return d, ok
}
