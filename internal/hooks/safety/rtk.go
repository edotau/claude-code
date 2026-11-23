// The rtk hook routes eligible Bash commands through the rtk output compressor.
package safety

import (
	"bytes"
	"encoding/json"
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
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// RTKRewrite rewrites a Bash command via `rtk rewrite`, the source of truth for what rtk compresses. It says nothing
// when rtk is absent, the tool is not Bash, a harness veto applies, or rtk leaves the command unchanged.
func RTKRewrite(in *core.Input) core.Result {
	if in.ToolName != "" && in.ToolName != "Bash" {
		return core.Result{}
	}
	if in.Agent() != core.AgentClaude {
		return rtkAgentRewrite(in)
	}
	res := rtkRewrite(in)
	if claudePrintRun(in.ToolCommand()) {
		res.Context = strings.TrimSpace(res.Context + "\n" + claudePrintHint)
	}
	return res
}

// rtkAgentRewrite offers an external agent rtk's rewrite under RTK_WRAP=on, only when both forms pass the gates.
func rtkAgentRewrite(in *core.Input) core.Result {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(rtkwrap.ModeEnv)))
	agent, bin, cmd := in.Agent(), paths.RTKBinary(), in.ToolCommand()
	if !rtkAgentEligible(in, mode, bin, cmd) {
		return core.Result{}
	}
	rewritten := rtkHookCommand(bin, agent, in, cmd)
	if rewritten == "" || rewritten == cmd {
		return core.Result{}
	}
	_, onPath := paths.RTKOnPath(bin)
	word := rtkWord(bin, os.Getenv("HOME"), onPath)
	pinned := rtkAbsolute(rewritten, word)
	if word != "rtk" && rtkBareCount(pinned) > rtkBareCount(cmd) {
		return core.Result{}
	}
	if judged := rtkWithCommand(in, pinned); judged == nil || !rtkGatesAllow(judged) {
		return core.Result{}
	}
	updated := rtkAgentInput(in, agent, pinned)
	if updated == nil {
		return core.Result{}
	}
	return core.Result{UpdatedInput: updated, Verdict: core.VerdictRewrite}
}

// rtkAgentEligible gates before the rtk exec; debug is off here, as rtkwrap's decision log is not exported.
func rtkAgentEligible(in *core.Input, mode, bin, cmd string) bool {
	return mode == "on" && os.Getenv(rtkwrap.CodemodeEnv) == "" && bin != "" &&
		in.Agent() != core.AgentCopilot && rtkMayRewrite(cmd) && !rtkVetoed(cmd, in.CWD, bin) &&
		!rtkwrap.EnvAltered(cmd) && rtkGatesAllow(in) && !rtkCodexRuleHit(in, cmd)
}

// rtkHookCommand is `rtk hook <agent>`'s rewritten command; "" on any error, timeout or unknown agent (fail open).
func rtkHookCommand(bin, agent string, in *core.Input, cmd string) string {
	var args, path []string
	switch agent {
	case core.AgentOpencode:
		args, path = []string{"hook", "opencode", cmd}, []string{"command"}
	case core.AgentCodex:
		args, path = []string{"hook", "codex"}, []string{"hookSpecificOutput", "updatedInput", "command"}
	case core.AgentGemini:
		args, path = []string{"hook", "antigravity"}, []string{"overwrite", "CommandLine"}
	default:
		return ""
	}
	ctx, cancel := core.HookContext("rtk-rewrite")
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	c.WaitDelay = 200 * time.Millisecond
	c.Env = append(os.Environ(), "RTK_SUPPRESS_HOOK_WARNING=1")
	if agent != core.AgentOpencode {
		c.Stdin = bytes.NewReader(in.Raw)
	}
	out, err := c.Output()
	if err != nil {
		return ""
	}
	var reply map[string]any
	if json.Unmarshal(out, &reply) != nil {
		return ""
	}
	var value any = reply
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = object[key]
	}
	rewritten, _ := value.(string)
	return rewritten
}

// rtkGatesAllow: every Bash PreToolUse module the agent runs allows in; agentBashAllow's lift is not applied here.
func rtkGatesAllow(in *core.Input) bool {
	surface := "native"
	if in.Agent() == core.AgentOpencode {
		surface = "plugin"
	}
	for _, s := range settings.SpecsFor(in.Agent(), surface) {
		if s.Event != settings.EventPreToolUse || s.Name == "rtk-rewrite" || !rtkMatchesBash(s.Matcher) {
			continue
		}
		if judge, ok := rtkJudges[s.Name]; !ok || !judge(in) {
			return false
		}
	}
	return true
}

func rtkMatchesBash(matcher string) bool {
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	return err == nil && re.MatchString("Bash")
}

// rtkJudges are side-effect-free stand-ins for the Bash PreToolUse modules; a module missing here stops the rewrite.
var rtkJudges = map[string]func(*core.Input) bool{
	"safety-scan":     func(in *core.Input) bool { return Verdict(in) == core.Allow },
	"permission-gate": func(in *core.Input) bool { return PermissionGate(in).Decision == core.Allow },
	"docs-placement": func(in *core.Input) bool {
		r := docs.Placement(in) // its Bash path only reads git state
		return r.Decision == core.Allow && r.UpdatedInput == nil
	},
	// ClaimCheck writes seen-store marks, so it is screened: it can refuse only a git add/commit/apply/am/mv
	"worktree-claim-check": func(in *core.Input) bool { return !wthook.MayRefuse(in) },
}

// rtkWithCommand is in's real payload with only the command replaced, parsed as the gate entries do; nil on error.
func rtkWithCommand(in *core.Input, cmd string) *core.Input {
	var doc map[string]any
	if json.Unmarshal(in.Raw, &doc) != nil || doc == nil {
		return nil
	}
	holder, key := rtkCommandHolder(doc, in.Agent())
	if holder == nil {
		return nil
	}
	holder[key] = cmd
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	judged := core.ParseInputFor(bytes.NewReader(payload), in.Agent())
	if judged.Invalid != "" || judged.ToolCommand() != cmd {
		return nil
	}
	return judged
}

// rtkCommandHolder is the object and key holding the command: agy's toolCall.args.CommandLine, else tool_input.command.
func rtkCommandHolder(doc map[string]any, agent string) (map[string]any, string) {
	if call, ok := doc["toolCall"].(map[string]any); ok && agent == core.AgentGemini {
		args, _ := call["args"].(map[string]any)
		return args, "CommandLine"
	}
	input, _ := doc["tool_input"].(map[string]any)
	return input, "command"
}

// rtkCodexRuleHit: codex enforces dispatch Ask and Allow rules as .rules prefixes, which `rtk <cmd>` no longer matches.
func rtkCodexRuleHit(in *core.Input, cmd string) bool {
	if in.Agent() != core.AgentCodex {
		return false
	}
	p, set, err := dispatchPolicy()
	if err != nil || !set {
		return err != nil
	}
	if (permission.Permissions{Mode: permission.PermAllow, Ask: p.Ask}).DecideAt(in.CWD, "Bash", cmd) != permission.Allow {
		return true
	}
	allow := permission.Permissions{Mode: permission.PermDefault, Allow: p.Allow}
	return slices.ContainsFunc(shell.Segments(cmd), func(seg string) bool {
		return allow.DecideAt(in.CWD, "Bash", seg) == permission.Allow
	})
}

func rtkAgentInput(in *core.Input, agent, pinned string) map[string]any {
	if agent == core.AgentGemini { // every run_command arg is sent back: agy's overwrite may replace, not merge
		var doc map[string]any
		if json.Unmarshal(in.Raw, &doc) != nil {
			return nil
		}
		args, key := rtkCommandHolder(doc, agent)
		if args != nil && key == "CommandLine" {
			args[key] = pinned
			return args
		}
		return nil
	}
	updated := in.CloneToolInput()
	if updated != nil {
		updated["command"] = pinned
	}
	return updated
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
	if !rtkwrap.EnvAltered(cmd) {
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

// rtkVetoed reports whether cmd stays out of rtk: wrapped, any commit, or a shared rtkwrap veto.
func rtkVetoed(cmd, cwd, bin string) bool {
	tokens := shell.Tokens(cmd)
	return len(tokens) == 0 || rtkWrapped(tokens[0], bin) || core.GitCommitCmd().MatchString(cmd) || rtkwrap.Vetoed(cmd, cwd)
}

const claudePrintHint = "headless claude print runs skip dispatch records, routing and recall; " +
	"use `claude-code agents ask --agent claude \"<task>\"` instead"

// claudePrintRun reports whether a chain segment launches a headless claude print run (-p/--print).
func claudePrintRun(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		if w := unwrapAll(shell.Tokens(seg)); len(w) > 0 && claudePrintArgs(filepath.Base(w[0]), w[1:]) {
			return true
		}
	}
	return false
}

// claudePrintArgs: claude-code takes -p only before its subcommand (after it -p is the vendor's or a profile); claude anywhere.
func claudePrintArgs(prog string, args []string) bool {
	if prog != "claude" && !shell.IsHarnessName(prog) { // .hookd hands -p to claude-code
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
