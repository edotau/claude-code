// The rtk hook routes eligible Bash commands through the rtk output compressor.
package hooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// rtkBinaryEnv overrides the proxy binary. Exists for tests, which cannot rely on rtk being
// installed; a real override is also useful for a wrapper script or a pinned path.
const rtkBinaryEnv = "HARNESS_RTK_BIN"

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

// rtkRewrite rewrites a Bash command via `rtk rewrite`, the source of truth for what rtk compresses. It says nothing
// when rtk is absent, the tool is not Bash, a harness veto applies, or rtk leaves the command unchanged.
func rtkRewrite(in *Input) Result {
	if in.Vendor() != VendorClaude {
		return Result{}
	}
	if in.ToolName != "" && in.ToolName != "Bash" {
		return Result{}
	}
	cmd := in.ToolCommand()
	if rtkVetoed(cmd, in.CWD, os.Getenv(rtkBinaryEnv)) {
		return Result{}
	}
	bin := rtkBinary()
	if bin == "" {
		return Result{}
	}
	rewritten := rtkRewritten(bin, cmd)
	if rewritten == "" {
		return Result{}
	}

	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (description, timeout, run_in_background) would silently alter the call.
	updated := in.cloneToolInput()
	if updated == nil {
		return Result{}
	}
	updated["command"] = rtkAbsolute(rewritten, bin)

	note := fmt.Sprintf(
		"rtk: routed through the rtk output compressor (%[1]s). Output is summarized, not literal — "+
			"for exact bytes run `%[1]s bypass <cmd>`; elided output names its own `%[1]s recall <hash>`.", bin)
	return Result{Context: note, UpdatedInput: updated}
}

// rtkRewritten asks `rtk rewrite` for cmd's replacement: exit 0 or 3 with different non-empty stdout.
// Anything else, a timeout or an error is "" (leave unchanged).
func rtkRewritten(bin, cmd string) string {
	ctx, cancel := hookContext("rtk-rewrite")
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "rewrite", cmd).Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			return ""
		}
	}
	got := strings.TrimSpace(string(out))
	if got == cmd {
		return ""
	}
	return got
}

// rtkCommandWord matches a bare `rtk` at command position: the start, or after `&&`, `||`, `;`, `|` or a newline.
var rtkCommandWord = regexp.MustCompile(`(^|&&|\|\||[;|\n])(\s*)rtk(\s|$)`)

// rtkAbsolute points every command-position `rtk` at the resolved bin, so the pinned binary always runs.
func rtkAbsolute(cmd, bin string) string {
	q := bin
	if strings.ContainsAny(q, " \t'\"$`\\") {
		q = "'" + strings.ReplaceAll(q, "'", `'\''`) + "'"
	}
	return rtkCommandWord.ReplaceAllString(cmd, "${1}${2}"+strings.ReplaceAll(q, "$", "$$")+"${3}")
}

// rtkVetoed reports whether the harness keeps cmd out of rtk: already wrapped, git in a linked worktree,
// any commit (the plan tick reads git's own output), machine-readable flags, or git value subcommands.
func rtkVetoed(cmd, cwd, bin string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 || fields[0] == bin || fields[0] == "rtk" || gitCommitCmd.MatchString(cmd) {
		return true
	}
	if rtkGitInLinkedWorktree(cmd, cwd) {
		return true // Claude Code's worktree-isolation check cannot verify an rtk-wrapped git
	}
	for _, seg := range cmdSegment.Split(cmd, -1) {
		args := rtkGitArgs(seg)
		if args == nil {
			continue
		}
		// "" is gitRTKSubcommand's fail-closed answer (unknown option, no subcommand): never wrap what it can't classify.
		if sub := gitRTKSubcommand(args); sub == "" || rtkValueSubcommands[sub] {
			return true
		}
	}
	tokens := shellTokens(strings.ToLower(cmd))
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

// rtkGitArgs returns the argv after `git` when the segment runs git (env assignments skipped), else nil.
func rtkGitArgs(seg string) []string {
	tokens := shellTokens(seg)
	for len(tokens) > 0 && strings.Contains(tokens[0], "=") && !strings.HasPrefix(tokens[0], "-") {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 || tokens[0] != "git" {
		return nil
	}
	return append([]string{}, tokens[1:]...)
}

// rtkGitInLinkedWorktree reports whether any chain segment runs git from a linked worktree: the cwd, a
// `cd <dir>` target or a `git -C <dir>` target.
func rtkGitInLinkedWorktree(cmd, cwd string) bool {
	hasGit := false
	for _, seg := range cmdSegment.Split(cmd, -1) {
		hasGit = hasGit || rtkGitArgs(seg) != nil
	}
	if !hasGit {
		return false
	}
	dirs := []string{cwd}
	for _, seg := range cmdSegment.Split(cmd, -1) {
		t := shellTokens(seg)
		if len(t) == 2 && t[0] == "cd" {
			dirs = append(dirs, t[1])
		}
		if m := gitDirArg.FindStringSubmatch(seg); m != nil {
			dirs = append(dirs, strings.Trim(m[1], `"'`))
		}
	}
	home, _ := os.UserHomeDir()
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if d == "~" || strings.HasPrefix(d, "~/") {
			d = filepath.Join(home, d[1:])
		} else if !filepath.IsAbs(d) {
			if cwd == "" {
				continue
			}
			d = filepath.Join(cwd, d)
		}
		if top, main := paths.CheckoutOf(d); top != "" && main != top {
			return true
		}
	}
	return false
}

// RTKBinary reports the resolved rtk binary for the CLI's status line, or "".
func RTKBinary() string { return rtkBinary() }

// rtkBinary returns the absolute proxy binary to invoke, or "" when it is not available. Absence is the
// normal case on a machine without rtk and must degrade to a no-op, never an error.
func rtkBinary() string {
	candidates := []string{filepath.Join(paths.BinDir(), "rtk"), "rtk"} // the pinned install first, then PATH
	if name := os.Getenv(rtkBinaryEnv); name != "" {
		candidates = []string{name}
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				return abs
			}
			return path
		}
	}
	return ""
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
