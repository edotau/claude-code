package rtkwrap

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// rtkMachineReadable are flags asking for exact, parseable output (gates read `git status --porcelain`): bypass rtk.
var rtkMachineReadable = sync.OnceValue(func() []string { return shell.GuardWords("rtk_machine_readable_flags") })

// rtkValueSubcommands are git subcommands whose stdout is a value parsed verbatim (`git rev-parse HEAD` has no flag).
var rtkValueSubcommands = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rtk_value_subcommands") })

// Vetoed reports whether cmd stays out of rtk: linked-worktree git, machine flags, value git, raw-only go output.
func Vetoed(cmd, cwd string) bool {
	// a linked-worktree git stays raw: Claude Code's worktree check cannot verify an rtk-wrapped git
	return rtkGitInLinkedWorktree(cmd, cwd) || rtkGitValueVetoed(cmd) || rtkMachineReadableFlag(cmd) || rtkGoRaw(cmd)
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
		t, _ := shell.Unwrap(shell.Tokens(seg), "")
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

// rtkMachineReadableFlag reports whether cmd carries a flag that asks for machine-readable output.
func rtkMachineReadableFlag(cmd string) bool {
	tokens := shell.Tokens(strings.ToLower(cmd))
	for i, token := range tokens {
		name, _, _ := strings.Cut(token, "=")
		if slices.Contains(rtkMachineReadable(), name) || name == "-o" && i+1 < len(tokens) && tokens[i+1] == "json" {
			return true
		}
	}
	return false
}

// rtkGitArgs is the argv after `git` when the segment runs git (seen through env, sudo, time…), else nil.
func rtkGitArgs(seg string) []string {
	rest, _ := shell.Unwrap(shell.Tokens(seg), "")
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
	for _, d := range rtkChainDirs(cmd, cwd) {
		d, ok := rtkAbsDir(d, cwd)
		if !ok {
			continue
		}
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
