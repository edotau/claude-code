// The rtk hook routes eligible Bash commands through the rtk output compressor.
package safety

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// rtkMachineReadable are flags asking for exact, parseable output (gates read `git status --porcelain`): bypass rtk.
var rtkMachineReadable = sync.OnceValue(func() []string { return shell.GuardWords("rtk_machine_readable_flags") })

// rtkValueSubcommands are git subcommands whose stdout is a value parsed verbatim (`git rev-parse HEAD` has no flag).
var rtkValueSubcommands = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rtk_value_subcommands") })

// RTKRewrite rewrites a Bash command via `rtk rewrite`, the source of truth for what rtk compresses. It says nothing
// when rtk is absent, the tool is not Bash, a harness veto applies, or rtk leaves the command unchanged.
func RTKRewrite(in *core.Input) core.Result {
	if in.Agent() != core.AgentClaude || in.ToolName != "" && in.ToolName != "Bash" {
		return core.Result{}
	}
	res := rtkRewrite(in)
	if claudePrintRun(in.ToolCommand()) {
		res.Context = strings.TrimSpace(res.Context + "\n" + claudePrintHint)
	}
	return res
}

// rtkRewrite is RTKRewrite for a Claude Bash call.
func rtkRewrite(in *core.Input) core.Result {
	bin := paths.RTKBinary()
	if bin == "" {
		return core.Result{}
	}
	cmd := in.ToolCommand()
	if !rtkMayRewrite(cmd) || rtkVetoed(cmd, in.CWD, bin) {
		return core.Result{}
	}
	rewritten := rtkRewritten(bin, cmd)
	if rewritten == "" {
		return core.Result{}
	}
	// auto mode's classifier gives no verdict on a hook-changed input, so hint the rtk form instead of rewriting
	if in.Nested("permission_mode") == "auto" {
		return core.Result{Context: "rtk: write the next call as: " + rewritten}
	}

	// updatedInput REPLACES the whole tool_input, so every original key (description, timeout…) must be echoed back.
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}
	found, onPath := paths.RTKOnPath(bin)
	word := textutil.QuoteSH(bin) // the command moves PATH or HOME, so bare rtk or "$HOME/…" could run another rtk
	if !rtkEnvAltered().MatchString(cmd) {
		word = rtkWord(bin, os.Getenv("HOME"), onPath)
	}
	pinned := rtkAbsolute(rewritten, word)
	if word != "rtk" && rtkBareCount(pinned) > rtkBareCount(cmd) { // rtk put a bare rtk where PATH may not find it
		return core.Result{}
	}
	updated["command"] = pinned

	return core.Result{Context: rtkNotes(in.SessionID, word, found, bin, onPath), UpdatedInput: updated}
}

// rtkRewriteWords are the words rtk rewrite acts on; TestRTKRewriteWordsMatchRTK fails when rtk outgrows them.
var rtkRewriteWords = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rtk_rewrite_words") })

// rtkWordRun is one word of a command, and rtkVersioned a versioned spelling of one (gcc-13, python3.12).
var rtkWordRun = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[\w.+-]+`) })
var rtkVersioned = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[-.]?[0-9][0-9.]*$`) })

// rtkMayRewrite is false only when no word of cmd is one rtk rewrites, so skipping the ~10 ms exec loses nothing.
func rtkMayRewrite(cmd string) bool {
	return slices.ContainsFunc(rtkWordRun().FindAllString(cmd, -1), func(w string) bool {
		return rtkRewriteWords()[w] || rtkRewriteWords()[rtkVersioned().ReplaceAllString(w, "")]
	})
}

// rtkNotes is the once-per-session context: the bypass hint, plus the shadow notice when PATH lacks the pinned rtk.
func rtkNotes(sessionID, word, found, bin string, onPath bool) string {
	var notes []string
	if first, err := rtkSeen("hooks/rtk-note", sessionID, "note"); err != nil || first {
		notes = append(notes, fmt.Sprintf("rtk: output summarized; exact bytes: %s bypass <cmd>", word))
	}
	if !onPath {
		if first, err := rtkSeen("hooks/rtk-shadow", sessionID, "shadow"); err == nil && first {
			notes = append(notes, rtkShadowNotice(found, bin))
		}
	}
	return strings.Join(notes, "\n")
}

// rtkSeen is core.SeenAdd, but no session id records nothing and counts as first: there is nothing to dedupe against.
func rtkSeen(kind, sessionID, key string) (bool, error) {
	if sessionID == "" {
		return true, nil
	}
	return core.SeenAdd(kind, sessionID, key)
}

// rtkShadowNotice names PATH's rtk (untrusted: by path, never executed) and the fix; found "" means PATH has none.
func rtkShadowNotice(found, bin string) string {
	if found == "" {
		return fmt.Sprintf("rtk: no rtk on PATH, so the pinned %s runs by path — run make install", bin)
	}
	return fmt.Sprintf("rtk: rtk on PATH is %s, not the pinned binary — run make install", found)
}

// rtkRewritten is `rtk rewrite`'s replacement for cmd (exit 0 or 3, changed stdout); anything else is "" (unchanged).
func rtkRewritten(bin, cmd string) string {
	ctx, cancel := core.HookContext("rtk-rewrite")
	defer cancel()
	c := exec.CommandContext(ctx, bin, "rewrite", cmd)
	c.WaitDelay = 200 * time.Millisecond // a grandchild holding stdout must not outlive the hook budget
	out, err := c.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 3) {
		return ""
	}
	got := strings.TrimSpace(string(out))
	if got == cmd {
		return ""
	}
	return got
}

// rtkEnvAltered matches a command that assigns, exports, unsets or clears PATH or HOME (over-matching is safe).
var rtkEnvAltered = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w$])(PATH|HOME)(\[[^\]]*\])?\+?=` +
		`|\b(export|unset|readonly|declare|typeset|local)\s[^;&|\n]*\b(PATH|HOME)\b` +
		`|\benv\s[^;&|\n]*(-i\b|-u\s*(PATH|HOME)\b|--ignore-environment|--unset[=\s](PATH|HOME)\b)`)
})

// rtkCommandWord matches a bare `rtk` at command position (line start, or after `&&`, `||`, `;`, `|`, `(`, backtick).
var rtkCommandWord = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|&&|\|\||[;|\n(\x60])(\s*)rtk(\s|$)`)
})

// rtkBareWord matches `rtk` as a whole word not inside a path (after NAME=val, a wrapper word, or in a -c string).
var rtkBareWord = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w./-])rtk($|[^\w.-])`)
})

func rtkBareCount(cmd string) int { return len(rtkBareWord().FindAllStringIndex(cmd, -1)) }

// rtkAbsolute points every command-position `rtk` at word (rtkWord's spelling), so the pinned binary always runs.
func rtkAbsolute(cmd, word string) string {
	out := rtkAbsolutePass(cmd, word)
	if strings.Count(cmd, "rtk") < 2 { // a match eats its trailing separator, so only a second rtk needs a second pass
		return out
	}
	return rtkAbsolutePass(out, word)
}

// rtkAbsolutePass rewrites only matches whose prefix the shell runs; quoted text like '(rtk x)' stays as data.
func rtkAbsolutePass(cmd, word string) string {
	quote := shell.QuoteAt(cmd)
	var b strings.Builder
	last := 0
	for _, m := range rtkCommandWord().FindAllStringSubmatchIndex(cmd, -1) {
		if p := m[2]; p < m[3] && !rtkPrefixRuns(cmd, quote, p) {
			continue
		}
		b.WriteString(cmd[last:m[5]])
		b.WriteString(word)
		b.WriteString(cmd[m[6]:m[7]])
		last = m[1]
	}
	return b.String() + cmd[last:]
}

// rtkPrefixRuns reports whether the separator at cmd[p] is live shell syntax rather than quoted text.
func rtkPrefixRuns(cmd string, quote []byte, p int) bool {
	switch quote[p] {
	case 0:
		return true
	case '"':
		return cmd[p] == '`' || cmd[p] == '(' && p > 0 && cmd[p-1] == '$'
	}
	return false
}

// rtkWord spells bin so allow rules stay machine-independent: bare rtk on PATH, else `"$HOME/<rest>"`, else quoted.
func rtkWord(bin, home string, onPath bool) string {
	if onPath {
		return "rtk"
	}
	home = strings.TrimRight(home, "/")
	rest, ok := strings.CutPrefix(bin, home+"/")
	if home == "" || !ok {
		return textutil.QuoteSH(bin)
	}
	return `"$HOME/` + rtkDoubleQuoteEscaper.Replace(rest) + `"`
}

// RTKWords is every spelling RTKRewrite can give the rtk command word for bin: bare, `"$HOME/…"`, quoted absolute.
func RTKWords(bin, home string) []string {
	if bin == "" {
		return []string{"rtk"}
	}
	return []string{rtkWord(bin, home, true), rtkWord(bin, home, false), textutil.QuoteSH(bin)}
}

// rtkDoubleQuoteEscaper backslashes the four characters still special inside POSIX double quotes.
var rtkDoubleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")

// rtkWrapped reports whether word (an unquoted first token) already names bin: bare rtk, bin, or bin via $HOME.
func rtkWrapped(word, bin string) bool {
	if rest, ok := strings.CutPrefix(word, "$HOME/"); ok {
		word = filepath.Join(os.Getenv("HOME"), rest)
	}
	return word == "rtk" || word == bin
}

// rtkVetoed reports whether cmd stays out of rtk: wrapped, linked-worktree git, any commit, machine flags, value git.
func rtkVetoed(cmd, cwd, bin string) bool {
	tokens := shell.Tokens(cmd)
	if len(tokens) == 0 || rtkWrapped(tokens[0], bin) || core.GitCommitCmd().MatchString(cmd) {
		return true
	}
	// a linked-worktree git stays raw: Claude Code's worktree check cannot verify an rtk-wrapped git
	return rtkGitInLinkedWorktree(cmd, cwd) || rtkGitValueVetoed(cmd) || rtkMachineReadableFlag(cmd) || rtkGoRaw(cmd)
}

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

const claudePrintHint = "headless claude print runs skip dispatch records, routing and recall; " +
	"use `claude-code agents ask --agent claude \"<task>\"` instead"

// claudePrintRun reports whether a chain segment launches a headless claude print run (-p/--print).
func claudePrintRun(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		w, _ := shell.Unwrap(shell.Tokens(seg), "")
		for len(w) > 0 && filepath.Base(w[0]) == "rtk" {
			w, _ = shell.Unwrap(w[1:], "")
		}
		if len(w) > 0 && claudePrintArgs(filepath.Base(w[0]), w[1:]) {
			return true
		}
	}
	return false
}

// claudePrintArgs: claude-code takes -p only before its subcommand (after it -p is the vendor's or a profile); claude anywhere.
func claudePrintArgs(prog string, args []string) bool {
	if prog != "claude" && prog != "claude-code" {
		return false
	}
	i := slices.IndexFunc(args, func(a string) bool { return a == "-p" || a == "--print" })
	if i < 0 || slices.ContainsFunc(args, claudeInfoFlag) {
		return false
	}
	if end := slices.Index(args, "--"); end >= 0 && end < i {
		return false
	}
	return prog == "claude" || !slices.ContainsFunc(args[:i], claudeWord)
}

func claudeInfoFlag(a string) bool {
	return a == "-h" || a == "--help" || a == "--version" || a == "-v"
}

func claudeWord(a string) bool { return !strings.HasPrefix(a, "-") }
