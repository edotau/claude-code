// The rtk hook routes eligible Bash commands through the rtk output compressor.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// rtkBinaryEnv overrides the proxy binary. Exists for tests, which cannot rely on rtk being
// installed; a real override is also useful for a wrapper script or a pinned path.
const rtkBinaryEnv = "HARNESS_RTK_BIN"

// rtkCommands are the noisy commands rtk knows how to compress: exactly the set rtk documents.
var rtkCommands = map[string]bool{
	"git": true, "npm": true, "cargo": true, "kubectl": true, "pytest": true, "docker": true,
}

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

// rtkChainOperators start a NEW command. Eligibility is judged on the first word only, so a chain's
// later segments were never checked, and prefixing the whole string leaves them outside the proxy
// while the "output is summarized" notice claims otherwise. A pipe is deliberately absent: it feeds
// rtk's output onward, which is what the caller asked for.
var rtkChainOperators = []string{"&&", "||", ";", "\n"}

// RTKRewrite rewrites eligible Bash commands through the Claude rtk output compressor.
func RTKRewrite(r io.Reader, stdout, _ io.Writer) int {
	return rtkWrap(ParseInput(r), stdout)
}

// rtkWrap rewrites an eligible Bash command to run through the rtk proxy. Emits nothing (and always
// proceeds) when rtk is absent, the tool is not Bash, or the command is ineligible.
func rtkWrap(in *Input, stdout io.Writer) int {
	if in.Vendor() != VendorClaude {
		return ExitProceed
	}
	if in.ToolName != "" && in.ToolName != "Bash" {
		return ExitProceed
	}
	cmd := in.ToolCommand()
	if !rtkEligible(cmd, os.Getenv(rtkBinaryEnv)) {
		return ExitProceed
	}
	if rtkGitInLinkedWorktree(cmd, in.CWD) {
		return ExitProceed // Claude Code's worktree-isolation check cannot verify an rtk-wrapped git
	}
	bin := rtkBinary()
	if bin == "" {
		return ExitProceed
	}

	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (description, timeout, run_in_background) would silently alter the call.
	updated := in.cloneToolInput()
	if updated == nil {
		return ExitProceed
	}
	updated["command"] = bin + " " + cmd

	note := fmt.Sprintf(
		"rtk: routed through the rtk output compressor (%s). Output is summarized, not literal — "+
			"re-run without rtk if you need exact bytes.", bin)
	inner := map[string]any{"hookEventName": "PreToolUse", "additionalContext": note, "updatedInput": updated}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": inner})
	if err != nil {
		return ExitProceed
	}
	fmt.Fprintln(stdout, string(out))
	return ExitProceed
}

// rtkGitInLinkedWorktree reports whether cmd is a git command run from inside a linked worktree.
func rtkGitInLinkedWorktree(cmd, cwd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 || fields[0] != "git" || cwd == "" {
		return false
	}
	top, main := paths.CheckoutOf(cwd)
	return top != "" && main != top
}

// RTKBinary reports the resolved rtk binary for the CLI's status line, or "".
func RTKBinary() string { return rtkBinary() }

// RTKCommandList renders the compressible command set, for the doctor/status report.
func RTKCommandList() string {
	names := make([]string, 0, len(rtkCommands))
	for c := range rtkCommands {
		names = append(names, c)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// rtkBinary returns the proxy binary to invoke, or "" when it is not available. Absence is the
// normal case on a machine without rtk and must degrade to a no-op, never an error.
func rtkBinary() string {
	candidates := []string{filepath.Join(paths.BinDir(), "rtk"), "rtk"} // the pinned install first, then PATH
	if name := os.Getenv(rtkBinaryEnv); name != "" {
		candidates = []string{name}
	}
	for _, name := range candidates {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// rtkEligible reports whether cmd should be routed through the proxy: it is a single command whose
// FIRST word is one rtk compresses, it is not already wrapped, and its output is a report to read
// rather than a value to parse.
func rtkEligible(cmd, bin string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	head := fields[0]
	if head == bin || head == "rtk" {
		return false // already proxied; wrapping again would nest the compressor
	}
	if !rtkCommands[head] {
		return false
	}
	for _, op := range rtkChainOperators {
		if strings.Contains(cmd, op) {
			return false
		}
	}
	if head == "git" {
		// "" is gitRTKSubcommand's fail-closed answer (unknown option, no subcommand): never wrap what it can't classify.
		if sub := gitRTKSubcommand(fields[1:]); sub == "" || rtkValueSubcommands[sub] {
			return false
		}
	}
	tokens := shellTokens(strings.ToLower(cmd))
	for i, token := range tokens {
		name, _, _ := strings.Cut(token, "=")
		for _, flag := range rtkMachineReadable {
			if flag == "-o json" {
				if name == "-o" && i+1 < len(tokens) && tokens[i+1] == "json" {
					return false
				}
			} else if name == flag {
				return false
			}
		}
	}
	return true
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
