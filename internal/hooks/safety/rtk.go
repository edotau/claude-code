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

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// rtkMachineReadable are flags whose whole point is exact, parseable output. Compressing those
// breaks every downstream parser — this harness's own gates read `git status --porcelain`, and
// a diff stat drives transcript tooling. A command carrying one of these bypasses rtk.
var rtkMachineReadable = []string{
	"--porcelain", "--json", "-o json", "--format", "--pretty", "--name-only", "--name-status",
	"--numstat", "--shortstat", "--raw", "-z", "--null", "--format-version", "--parseable", "--parse",
	"--show-current",
}

// rtkValueSubcommands name a git subcommand whose stdout is a VALUE to consume, not a report to read.
// The flag list cannot catch these: `git rev-parse HEAD` carries no flag at all, yet its single line
// is parsed verbatim — compressing it corrupts the caller exactly as compressing porcelain would.
var rtkValueSubcommands = map[string]bool{
	"rev-parse": true, "rev-list": true, "config": true, "describe": true, "symbolic-ref": true,
	"hash-object": true, "cat-file": true, "ls-files": true, "ls-tree": true, "ls-remote": true,
	"show-ref": true, "for-each-ref": true, "merge-base": true, "name-rev": true, "var": true,
	"check-ignore": true, "check-attr": true, "count-objects": true,
}

// RTKRewrite rewrites a Bash command via `rtk rewrite`, the source of truth for what rtk compresses. It says nothing
// when rtk is absent, the tool is not Bash, a harness veto applies, or rtk leaves the command unchanged.
func RTKRewrite(in *core.Input) core.Result {
	if !rtkApplies(in) {
		return core.Result{}
	}
	bin := shell.RTKBinary()
	if bin == "" {
		return core.Result{}
	}
	cmd := in.ToolCommand()
	if rtkVetoed(cmd, in.CWD, bin) {
		return core.Result{}
	}
	rewritten := rtkRewritten(bin, cmd)
	if rewritten == "" {
		return core.Result{}
	}

	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (description, timeout, run_in_background) would silently alter the call.
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}
	}
	found, onPath := shell.RTKOnPath(bin)
	word := textutil.QuoteSH(bin) // the command moves PATH or HOME, so bare rtk or "$HOME/…" could run another rtk
	if !rtkEnvAltered.MatchString(cmd) {
		word = rtkWord(bin, os.Getenv("HOME"), onPath)
	}
	updated["command"] = rtkAbsolute(rewritten, word)

	note := fmt.Sprintf("rtk: output summarized; exact bytes: %s bypass <cmd>", word)
	if !onPath && rtkFirstShadowNotice(in.SessionID) {
		note += "\n" + rtkShadowNotice(found, bin)
	}
	return core.Result{Context: note, UpdatedInput: updated}
}

// rtkShadowNotice names PATH's rtk (untrusted: by path, never executed) and the fix; found "" means PATH has none.
func rtkShadowNotice(found, bin string) string {
	if found == "" {
		return fmt.Sprintf("rtk: no rtk on PATH, so the pinned %s runs by path — run make install", bin)
	}
	return fmt.Sprintf("rtk: rtk on PATH is %s, not the pinned binary — run make install", found)
}

// rtkFirstShadowNotice claims the session's one shadow notice: true only for the first caller (an O_EXCL marker).
func rtkFirstShadowNotice(session string) bool {
	dir := filepath.Join(paths.StateDir(), "hooks", "rtk-shadow")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, paths.Slug(session, "", 80, "session")), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

// rtkApplies reports whether the hook handles in at all: a Claude session's Bash call.
func rtkApplies(in *core.Input) bool {
	return in.Agent() == core.AgentClaude && (in.ToolName == "" || in.ToolName == "Bash")
}

// rtkRewritten asks `rtk rewrite` for cmd's replacement: exit 0 or 3 with different non-empty stdout.
// Anything else, a timeout or an error is "" (leave unchanged).
func rtkRewritten(bin, cmd string) string {
	ctx, cancel := core.HookContext("rtk-rewrite")
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "rewrite", cmd).Output()
	if !rtkExitOK(err) {
		return ""
	}
	got := strings.TrimSpace(string(out))
	if got == cmd {
		return ""
	}
	return got
}

// rtkExitOK reports whether `rtk rewrite` exited 0 or 3, the two codes that carry a rewrite.
func rtkExitOK(err error) bool {
	var ee *exec.ExitError
	return err == nil || errors.As(err, &ee) && ee.ExitCode() == 3
}

// rtkEnvAltered matches a command that assigns, exports, unsets or clears PATH or HOME (over-matching is safe).
var rtkEnvAltered = regexp.MustCompile(`(^|[^\w$])(PATH|HOME)\+?=` +
	`|\b(export|unset|readonly|declare|typeset|local)\s[^;&|\n]*\b(PATH|HOME)\b` +
	`|\benv\s[^;&|\n]*(-i\b|-u\s*(PATH|HOME)\b|--ignore-environment|--unset[=\s](PATH|HOME)\b)`)

// rtkCommandWord matches a bare `rtk` at command position: the start, or after `&&`, `||`, `;`, `|` or a newline.
var rtkCommandWord = regexp.MustCompile(`(^|&&|\|\||[;|\n])(\s*)rtk(\s|$)`)

// rtkAbsolute points every command-position `rtk` at word (rtkWord's spelling), so the pinned binary always runs.
func rtkAbsolute(cmd, word string) string {
	return rtkCommandWord.ReplaceAllString(cmd, "${1}${2}"+strings.ReplaceAll(word, "$", "$$")+"${3}")
}

// rtkWord spells bin for the shell so allow rules stay machine-independent: bare rtk when PATH finds bin (onPath),
// else `"$HOME/<rest>"` under home, else the quoted absolute path.
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

// rtkDoubleQuoteEscaper backslashes the four characters still special inside POSIX double quotes.
var rtkDoubleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")

// rtkWrapped reports whether word (an unquoted first token) already names bin: bare rtk, bin, or bin via $HOME.
func rtkWrapped(word, bin string) bool {
	if rest, ok := strings.CutPrefix(word, "$HOME/"); ok {
		word = filepath.Join(os.Getenv("HOME"), rest)
	}
	return word == "rtk" || word == bin
}

// rtkVetoed reports whether the harness keeps cmd out of rtk: already wrapped, git in a linked worktree,
// any commit (the plan tick reads git's own output), machine-readable flags, or git value subcommands.
func rtkVetoed(cmd, cwd, bin string) bool {
	tokens := shell.Tokens(cmd)
	if len(tokens) == 0 || rtkWrapped(tokens[0], bin) || core.GitCommitCmd.MatchString(cmd) {
		return true
	}
	if rtkGitInLinkedWorktree(cmd, cwd) {
		return true // Claude Code's worktree-isolation check cannot verify an rtk-wrapped git
	}
	return rtkGitValueVetoed(cmd) || rtkMachineReadableFlag(cmd) || rtkGoVet(cmd)
}

// rtkGoVet: rtk 1.1.0 summarizes a failed `go vet` (e.g. a missing package) as "No issues found", so vet stays raw.
func rtkGoVet(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		if t, _ := shell.Unwrap(shell.Tokens(seg), ""); len(t) > 1 && t[0] == "go" && t[1] == "vet" {
			return true
		}
	}
	return false
}

// rtkGitValueVetoed reports whether any git segment runs a value subcommand or one gitRTKSubcommand can't classify.
func rtkGitValueVetoed(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		args := rtkGitArgs(seg)
		if args == nil {
			continue
		}
		// "" is gitRTKSubcommand's fail-closed answer (unknown option, no subcommand): never wrap what it can't classify.
		if sub := gitRTKSubcommand(args); sub == "" || rtkValueSubcommands[sub] {
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
		for _, flag := range rtkMachineReadable {
			if flag == "-o json" {
				if name == "-o" && i+1 < len(tokens) && tokens[i+1] == "json" {
					return true
				}
			} else if name == flag {
				return true
			}
		}
	}
	return false
}

// rtkGitArgs returns the argv after `git` when the segment runs git, else nil. Wrappers (env, sudo, time…) are
// seen through so the vetoes still apply to them; whether rtk rewrites a wrapped command is rtk's own call.
func rtkGitArgs(seg string) []string {
	rest, _ := shell.Unwrap(shell.Tokens(seg), "")
	if len(rest) == 0 || rest[0] != "git" {
		return nil
	}
	return append([]string{}, rest[1:]...)
}

// rtkGitInLinkedWorktree reports whether any chain segment runs git from a linked worktree: the cwd, a
// `cd <dir>` target or a `git -C <dir>` target.
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

// gitRTKGlobalValueFlags are git's own options that CONSUME the next argument. Their values must not
// be mistaken for the subcommand: `git -C /repo rev-parse` would otherwise classify as "/repo".
var gitRTKGlobalValueFlags = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--super-prefix": true, "--config-env": true,
}

// gitRTKSubcommand returns the subcommand from git's argv tail, skipping global options and the
// values they consume. "" when there is none, or on an UNRECOGNIZED option — fail closed, because
// guessing wrong here compresses a value the caller parses verbatim.
func gitRTKSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a // first non-option word is the subcommand
		}
		if name, _, hasEq := strings.Cut(a, "="); hasEq {
			if !gitRTKGlobalValueFlags[name] {
				return "" // unknown --opt=value: do not guess what follows
			}
			continue // value rode along on the same token
		}
		if gitRTKGlobalValueFlags[a] {
			i++ // consume its value
			continue
		}
		if !gitRTKGlobalBoolFlags[a] {
			return "" // unrecognized option — fail closed rather than misread the next word
		}
	}
	return ""
}

// gitRTKGlobalBoolFlags are git's valueless global options; anything outside both maps is unknown.
var gitRTKGlobalBoolFlags = map[string]bool{
	"--no-pager": true, "--paginate": true, "-p": true, "--bare": true, "--literal-pathspecs": true,
	"--no-replace-objects": true, "--glob-pathspecs": true, "--noglob-pathspecs": true,
	"--icase-pathspecs": true, "--no-optional-locks": true, "--no-lazy-fetch": true,
}
