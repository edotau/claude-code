package rtkwrap

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

// rtkMachineReadable are flags asking for exact, parseable output (gates read `git status --porcelain`): bypass rtk.
var rtkMachineReadable = sync.OnceValue(func() []string { return shell.GuardWords("rtk_machine_readable_flags") })

// rtkMachineLetters are the single-letter machine flags (-z), matched inside a short cluster too (-sz).
var rtkMachineLetters = sync.OnceValue(func() string {
	var letters strings.Builder
	for _, f := range rtkMachineReadable() {
		if len(f) == 2 && f[0] == '-' && f[1] != '-' {
			letters.WriteByte(f[1])
		}
	}
	return letters.String()
})

// rtkValueSubcommands are git subcommands whose stdout is a value parsed verbatim (`git rev-parse HEAD` has no flag).
var rtkValueSubcommands = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rtk_value_subcommands") })

// Surface is one place a command can gain an rtk prefix; each gate row names the surfaces it holds on.
type Surface uint8

const (
	ClaudeHook  Surface = 1 << iota // Claude Code's Bash hook
	AgentHook                       // another agent's command hook
	ModelOutput                     // ForModel: commands whose output a model reads
	Shim                            // the PATH shims
	hooks       = ClaudeHook | AgentHook
	everywhere  = hooks | ModelOutput | Shim
)

// Call is one command up for an rtk prefix, with what a gate row may read to judge it.
type Call struct{ Cmd, CWD, Bin, Session string }

// exception is one gate row: a command it matches runs as typed on the surfaces in on.
type exception struct {
	name string
	on   Surface
	when func(Call) bool
}

// byCmd lifts a command-only condition into a gate row's condition.
func byCmd(f func(string) bool) func(Call) bool { return func(c Call) bool { return f(c.Cmd) } }

// exceptions is the gate, first match wins: add a row to keep a command as typed, widen on to cover a new surface.
var exceptions = []exception{
	{"codemode", hooks | ModelOutput, func(c Call) bool { return CodeModeFor(c.Session) }},
	{"no-rtk", hooks | ModelOutput, func(c Call) bool { return c.Bin == "" }},
	{"empty", hooks, byCmd(func(cmd string) bool { return len(shell.Tokens(cmd)) == 0 })},
	{"wrapped", hooks, wrapped},
	// a linked-worktree git stays raw: Claude Code's worktree check cannot verify an rtk-wrapped git
	{"linked-worktree", everywhere, func(c Call) bool { return rtkGitInLinkedWorktree(c.Cmd, c.CWD) }},
	{"commit", everywhere, byCmd(rtkGitCommits)},
	{"value-git", everywhere, byCmd(rtkGitValueVetoed)},
	{"branch-mutate", everywhere, byCmd(rtkGitBranchMutates)},
	{"machine-flag", everywhere, byCmd(rtkMachineReadableFlag)},
	{"go-raw", everywhere, byCmd(rtkGoRaw)},
	{"file-output", everywhere, byCmd(rtkFileOutput)},
	{"ls-all", everywhere, byCmd(rtkLsAll)},
	{"env-altered", AgentHook | ModelOutput, byCmd(EnvAltered)},
}

// Exception names the first gate row that keeps c as typed on surface s ("" when none).
func Exception(s Surface, c Call) string {
	for _, row := range exceptions {
		if row.on&s != 0 && row.when(c) {
			return row.name
		}
	}
	return ""
}

// wrapped reports c's first word already names rtk: bare rtk, the pinned binary, or it under $HOME.
func wrapped(c Call) bool {
	t := shell.Tokens(c.Cmd)
	if len(t) == 0 {
		return false
	}
	word := t[0]
	if rest, ok := strings.CutPrefix(word, "$HOME/"); ok {
		word = filepath.Join(os.Getenv("HOME"), rest)
	}
	return word == rtkName || word == c.Bin
}

// Vetoed reports whether cmd stays out of rtk on every surface.
func Vetoed(cmd, cwd string) bool { return VetoRule(cmd, cwd) != "" }

// VetoRule names the first gate row that holds on every surface for cmd ("" when none).
func VetoRule(cmd, cwd string) string { return Exception(Shim, Call{Cmd: cmd, CWD: cwd}) }

// segWords is seg's command words past env/sudo wrappers and an existing rtk prefix, so a veto sees the real command.
func segWords(seg string) []string {
	t, _ := shell.Unwrap(shell.Tokens(seg), "")
	if len(t) > 0 && t[0] == rtkName {
		return t[1:]
	}
	return t
}

// rtkLsAll: rtk's compacted `ls` drops the `.` and `..` rows that `ls -a` prints, so a count over it is wrong.
func rtkLsAll(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		t := segWords(seg)
		if len(t) > 0 && t[0] == "ls" && slices.ContainsFunc(t[1:], func(a string) bool {
			return shortFlag(a, "aA") || a == "--all" || a == "--almost-all"
		}) {
			return true
		}
	}
	return false
}

// rtkFileOutput: rtk drops file-output flags (`git diff --output=f`, `curl -o f`), so the file is never written.
func rtkFileOutput(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		t := segWords(seg)
		if len(t) > 0 && slices.ContainsFunc(t[1:], func(a string) bool { return fileOutputFlag(t[0], a) }) {
			return true
		}
	}
	return false
}

func fileOutputFlag(name, a string) bool {
	switch name {
	case "git":
		return a == "--output" || strings.HasPrefix(a, "--output=")
	case "curl":
		return shortFlag(a, "oO") || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--remote-name")
	case "wget":
		return shortFlag(a, "O")
	}
	return false
}

// shortFlag reports a single-dash option cluster (`-sSo`, `-ofile`) holding any letter of set.
func shortFlag(a, set string) bool {
	return len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], set)
}

// rtkEnvAltered matches a command that assigns, exports, unsets or clears PATH or HOME (over-matching is safe).
var rtkEnvAltered = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w$])(PATH|HOME)(\[[^\]]*\])?\+?=` +
		`|\b(export|unset|readonly|declare|typeset|local)\s[^;&|\n]*\b(PATH|HOME)\b` +
		`|\benv\s[^;&|\n]*(-i\b|-u\s*(PATH|HOME)\b|--ignore-environment|--unset[=\s](PATH|HOME)\b)`)
})

// EnvAltered reports whether cmd assigns, exports, unsets or clears PATH or HOME.
func EnvAltered(cmd string) bool { return rtkEnvAltered().MatchString(cmd) }

// rtkGoRaw: rtk 1.1.0 reports a failed `go vet` as "No issues found" and drops `go test -v`/`-list` output.
func rtkGoRaw(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		t := segWords(seg)
		if len(t) < 2 || t[0] != "go" {
			continue
		}
		if t[1] == "vet" || t[1] == "test" && slices.ContainsFunc(t[2:], rtkGoTestRawFlag) {
			return true
		}
	}
	return false
}

var rtkGoTestRawFlags = sync.OnceValue(func() []string { return shell.GuardWords("rtk_go_test_raw_flags") })

// rtkGoTestRawFlag reports a go test flag whose output rtk's summary drops: -v, -list (also -test.* and --).
func rtkGoTestRawFlag(arg string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
	return strings.HasPrefix(arg, "-") && slices.Contains(rtkGoTestRawFlags(), name)
}

// rtkGitValueVetoed reports whether any git segment runs a value subcommand or one gitRTKSubcommand can't classify.
func rtkGitValueVetoed(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		args := rtkGitArgs(seg)
		if args == nil {
			continue
		}
		// "" is gitRTKSubcommand's fail-closed answer (unknown option, no subcommand): never wrap what it can't classify.
		if sub := gitRTKSubcommand(args); sub == "" || rtkValueSubcommands()[sub] {
			return true
		}
	}
	return false
}

// rtkGitCommits reports whether any git segment commits, past global options (`git -c k=v commit`).
func rtkGitCommits(cmd string) bool {
	return slices.ContainsFunc(shell.Segments(cmd), func(seg string) bool {
		args := rtkGitArgs(seg)
		return args != nil && gitRTKSubcommand(args) == "commit"
	})
}

// rtkGitBranchMutates: rtk prints only `ok` for `git branch -D`, so a delete, move or copy stays raw.
func rtkGitBranchMutates(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		args := rtkGitArgs(seg)
		if args == nil || gitRTKSubcommand(args) != "branch" {
			continue
		}
		if slices.ContainsFunc(args[slices.Index(args, "branch")+1:], func(a string) bool {
			return shortFlag(a, "dDmMcC") || a == "--delete" || a == "--move" || a == "--copy"
		}) {
			return true
		}
	}
	return false
}

// rtkMachineReadableFlag reports whether cmd carries a flag that asks for machine-readable output.
func rtkMachineReadableFlag(cmd string) bool {
	tokens := shell.Tokens(strings.ToLower(cmd))
	for i, token := range tokens {
		name, _, _ := strings.Cut(token, "=")
		if slices.Contains(rtkMachineReadable(), name) || shortFlag(name, rtkMachineLetters()) ||
			name == "-o" && i+1 < len(tokens) && tokens[i+1] == "json" {
			return true
		}
	}
	return false
}

// rtkGitArgs is the argv after `git` when the segment runs git (seen through env, sudo, time…), else nil.
func rtkGitArgs(seg string) []string {
	rest := segWords(seg)
	if len(rest) == 0 || rest[0] != "git" {
		return nil
	}
	return rest[1:]
}

// rtkGitInLinkedWorktree reports whether a chain segment runs git from a linked worktree (cwd, `cd`, or `git -C`).
func rtkGitInLinkedWorktree(cmd, cwd string) bool {
	if !slices.ContainsFunc(shell.Segments(cmd), func(seg string) bool { return rtkGitArgs(seg) != nil }) {
		return false
	}
	var seen []string
	for _, d := range rtkChainDirs(cmd, cwd) {
		d, ok := rtkAbsDir(d, cwd)
		if !ok || slices.Contains(seen, d) { // CheckoutOf stats up the tree: each dir once
			continue
		}
		seen = append(seen, d)
		if top, main := paths.CheckoutOf(d); top != "" && main != top {
			return true
		}
	}
	return false
}

// rtkChainDirs lists cwd, every `cd <dir>` result and every git target (`-C`, `env -C`) in the chain.
func rtkChainDirs(cmd, cwd string) []string {
	dirs := []string{cwd}
	cur := cwd
	for _, seg := range shell.Segments(cmd) {
		rest, dir := shell.Unwrap(shell.Tokens(seg), cur)
		if next, ok := shell.CdTarget(rest, cur); ok {
			cur = next
			dirs = append(dirs, cur)
		} else if len(rest) > 0 && rest[0] == "git" {
			target, _, _ := shell.GitInvocation(rest[1:], dir)
			dirs = append(dirs, target)
		}
	}
	return dirs
}

// rtkAbsDir resolves a candidate dir against cwd (~ expanded); false when it is empty or relative with no cwd.
func rtkAbsDir(d, cwd string) (string, bool) {
	if d == "" {
		return "", false
	}
	if d == "~" || strings.HasPrefix(d, "~/") {
		return paths.ExpandHome(d), true
	}
	if filepath.IsAbs(d) {
		return d, true
	}
	if cwd == "" {
		return "", false
	}
	return filepath.Join(cwd, d), true
}

// gitRTKSubcommand is the subcommand in git's argv tail; "" when none or on an UNRECOGNIZED option (fail closed).
func gitRTKSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a // first non-option word is the subcommand
		}
		if name, _, hasEq := strings.Cut(a, "="); hasEq {
			if !shell.GitValueOpt(name) {
				return "" // unknown --opt=value: do not guess what follows
			}
			continue // value rode along on the same token
		}
		if shell.GitValueOpt(a) {
			i++ // consume its value
			continue
		}
		if !gitRTKGlobalBoolFlags()[a] {
			return "" // unrecognized option — fail closed rather than misread the next word
		}
	}
	return ""
}

// gitRTKGlobalBoolFlags are git's valueless global options; anything outside it and shell.GitValueOpt is unknown.
var gitRTKGlobalBoolFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("git_valueless_global_flags") })
