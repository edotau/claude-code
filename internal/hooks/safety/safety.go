// Package safety is the command-safety hook area: safety-scan and the rtk rewrite.
package safety

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/shell"
)

// isAskLessEdit marks a non-Bash tool on an agent with no ask path (every non-Claude agent's edit tools).
func isAskLessEdit(in *core.Input) bool {
	return in.ToolName != "" && !strings.EqualFold(in.CanonicalTool(), "Bash") && !core.SupportsAsk(in.Agent())
}

// Scan denies a settings.json deny match and an AI-attribution footer, and gates a subagent's instruction-file write.
func Scan(in *core.Input) core.Result {
	if isAskLessEdit(in) { // a protected path's block names its own rule, ahead of the instruction-file one
		if r := protectedEdit(in); r.Decision != core.Allow {
			return r
		}
	}
	if r := suppressVerdict(in); r.Decision != core.Allow {
		return r
	}
	if r := wholeTreeVerdict(in); r.Decision != core.Allow {
		return r
	}
	cmd := dropMessageHeredocs(dropPatchBodies(in.ToolCommand()))
	d, msg, detail := commandVerdict(in, cmd)
	if d == core.Allow {
		return core.Result{}
	}
	if d == core.Ask {
		detail = "ask: " + detail
	}
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", detail)
	if d == core.Ask && core.SupportsAsk(in.Agent()) {
		return core.Result{Decision: core.Ask, Reason: strings.TrimSpace(msg), Rule: detail, Floor: true}
	}
	return core.Result{Decision: core.Block, Reason: strings.TrimSpace(msg), Rule: detail, Floor: true}
}

// Verdict is safety-scan's decision for in with no audit entry.
func Verdict(in *core.Input) core.Decision {
	d, _, _ := commandVerdict(in, dropMessageHeredocs(dropPatchBodies(in.ToolCommand())))
	return d
}

// commandVerdict is the stricter of the shell verdict and the instruction-file verdict.
func commandVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	d, msg, detail := shellVerdict(in, cmd)
	if d == core.Block {
		return d, msg, detail
	}
	if id, imsg, idetail := instructionVerdict(in, cmd); id > d {
		return id, imsg, idetail
	}
	return d, msg, detail
}

// shellVerdict judges cmd: an attribution footer first, then the settings deny rules.
func shellVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	if in.ToolName != "" && !strings.EqualFold(in.CanonicalTool(), "Bash") || cmd == "" {
		return core.Allow, "", ""
	}
	if d, msg, detail := attributionVerdict(in); d != core.Allow {
		return d, msg, detail
	}
	// A plain Python stdin script's body is Python and data text is no invocation; interpreterWrite judges its paths.
	py := withPyLaunches(dropPyScripts(cmd))
	asked := intentView(py, false)
	plain := asked // intentView(cmd, false), the same view when no Python script was dropped
	if py != cmd {
		plain = intentView(cmd, false)
	}
	return settingsVerdict(in, cmd, asked, plain)
}

// attributionVerdict blocks an AI-attribution footer in a commit, merge or tag message or a PR title or body.
func attributionVerdict(in *core.Input) (core.Decision, string, string) {
	view := intentView(withPyLaunches(dropPyScripts(dropPatchBodies(in.ToolCommand()))), true)
	if shell.AttributionFooterCommit(view, in.CWD) || shell.FooterFileWritten(in.ToolCommand(), in.CWD) {
		what := assets.SafetyAttributionWhat()
		hint := assets.SafetyAttributionHint() + ownerRoute
		return core.Block, core.SafetyMessage(in, core.Block, what, "", hint), "attribution footer"
	}
	return core.Allow, "", ""
}

// settingsVerdict denies a command a settings.json deny rule covers; a fuzzy interpreter write asks.
func settingsVerdict(in *core.Input, cmd, asked, plain string) (core.Decision, string, string) {
	p, cwd := livePolicy(), in.CWD
	switch rule, f := fedRuleHit(cmd, cwd, p.bashDeny), protectedWrite(cmd, cwd, p.editDeny, p.twin); {
	case rule != "":
		return core.Block, core.SafetyMessage(in, core.Block, assets.SafetyDenyWhat(), rule,
			assets.SafetyDenyHint()+ownerRoute), "settings deny"
	case sudoDenied() && sudoInvoked(asked, cwd):
		return core.Block, core.SafetyMessage(in, core.Block, assets.SafetySudoWhat(), "", sudoHint), "sudo"
	case f != "":
		return core.Block, core.SafetyMessage(in, core.Block, assets.SafetyProtectedWhat(), f,
			assets.SafetyProtectedHint()), "protected write"
	}
	if needle, _ := interpreterWrite(plain, p.editDeny); needle != "" { // fuzzy: asks; Scan blocks without ask
		return core.Ask, core.SafetyMessage(in, core.Ask, assets.SafetyInterpreterWhat(), needle, assets.SafetyInterpreterHint()), "interpreter protected write"
	}
	return core.Allow, "", ""
}

// fedRuleHit is the deny rule cmd hits, else the one a printf/echo piped into a shell prints.
func fedRuleHit(cmd, cwd string, rules []permission.Rule) string {
	if rule := settingsRuleHit(dropInertHeredocs(cmd), cwd, rules); rule != "" {
		return rule
	}
	_, fed := shellFeeds(cmd)
	for _, text := range fed {
		if rule := settingsRuleHit(text, cwd, rules); rule != "" {
			return rule
		}
	}
	return ""
}

// unwrapAll drops rtk and wrappers until neither leads (`env rtk timeout 5 X`), bounded like shell nesting.
func unwrapAll(toks []string) []string {
	for range 8 {
		next, _ := shell.Unwrap(shell.DropRtk(toks), "")
		if slices.Equal(next, toks) {
			break
		}
		toks = next
	}
	return toks
}

// fdToSubst matches a redirect to fd 3+, which `exec 3> >(sh)` can point at an interpreter.
var fdToSubst = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[<>]&[3-9]`) })

// pipeFilters only read stdin and print it: none runs, writes or forwards its input.
var pipeFilters = sync.OnceValue(func() map[string]bool { return shell.GuardSet("pipe_filters") })

// msgHeredocOpen matches a line ending in `$(cat <<'EOF'`: the body becomes the substitution's text.
var msgHeredocOpen = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\$\(\s*cat\s+<<-?\s*(?:'[^']*'|"[^"]*"|[A-Za-z_][A-Za-z0-9_]*)\s*$`)
})

// msgSlot stands in for a dropped message body when judging the command it is an argument of.
const msgSlot = "HEREDOCMSG"

// dropMessageHeredocs drops each non-expanding `"$(cat <<'EOF' … EOF\n)"` body that is an unpiped inert message.
func dropMessageHeredocs(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	return core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		loc := msgHeredocOpen().FindStringIndex(lines[i])
		ms := core.HeredocOp().FindAllStringSubmatch(lines[i], -1)
		if loc == nil || len(ms) != 1 {
			return -1
		}
		m := ms[0]
		end := core.HeredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-")
		if end < 0 || end+1 >= len(lines) || m[4] != "" && slices.ContainsFunc(lines[i+1:end], substitutes) {
			return -1
		}
		closer, ok := strings.CutPrefix(strings.TrimLeft(lines[end+1], " \t"), ")")
		if !ok || !messageArg(lines[i][:loc[0]]+msgSlot+closer) {
			return -1
		}
		return end
	})
}

// messageArg reports that msgSlot sits in an inert segment whose output is not piped on.
func messageArg(flat string) bool {
	segs, seps := shell.SegmentsSep(flat)
	for k, seg := range segs {
		if strings.Contains(seg, msgSlot) {
			return inertSegment(seg) && seps[k] != "|" && seps[k] != "|&"
		}
	}
	return false
}

// inertSegment reports a segment whose words are data: echo/printf, grep/rg patterns, commit/tag/gh text.
func inertSegment(seg string) bool {
	toks := shell.Tokens(seg)
	if len(toks) == 0 {
		return false
	}
	name := toks[0]
	if shell.IsHarnessName(filepath.Base(name)) {
		name = "claude-code"
	}
	inert, ok := inertCommands[name]
	return ok && inert(toks)
}

// inertCommands judge, per command name, whether a segment's words are only data.
var inertCommands = map[string]func(toks []string) bool{
	"echo": inertEcho, "printf": inertEcho, "grep": inertGrep, "rg": inertGrep, "git": inertGit, "gh": inertGh,
	"claude-code": func(toks []string) bool { return inertScm(toks) || inertWorktreeCommit(toks) },
}

// inertWorktreeCommit: the -m text of `claude-code worktree commit` is data; the footer scan keeps it (keepCommit).
func inertWorktreeCommit(toks []string) bool {
	args := shell.HarnessArgs(toks)
	return len(args) > 1 && args[0] == "worktree" && args[1] == "commit" && inertEcho(toks)
}

// inertEcho: a redirect can plant the text in a file that later runs (~/.bashrc).
func inertEcho(toks []string) bool {
	return !slices.ContainsFunc(toks, func(t string) bool { return strings.Contains(t, ">") })
}

func inertGrep(toks []string) bool {
	return !slices.ContainsFunc(toks, func(t string) bool {
		return strings.HasPrefix(t, "--pre") || strings.HasPrefix(t, "--hostname-bin")
	})
}

func inertGit(toks []string) bool {
	return len(toks) > 1 && (toks[1] == "commit" || toks[1] == "tag")
}

func inertGh(toks []string) bool {
	return len(toks) > 3 && (toks[1] == "pr" || toks[1] == "issue") && slices.Contains([]string{"create", "edit", "comment"}, toks[2])
}

// inertScm: the --title of `claude-code worktree pr create` is data; a --body-file path or a redirect is not.
func inertScm(toks []string) bool {
	if len(toks) < 4 || toks[1] != "worktree" || !inertEcho(toks) ||
		slices.ContainsFunc(toks, func(t string) bool { return strings.HasPrefix(t, "--body-file") }) {
		return false
	}
	return toks[2] == "pr" && toks[3] == "create"
}

// dropPatchBodies drops each apply_patch call's patch text unless cmd runs a file or names apply_patch elsewhere.
func dropPatchBodies(cmd string) string {
	if !strings.Contains(cmd, "apply_patch") {
		return cmd
	}
	n := 0
	out := core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		end := patchHeredocEnd(lines, i)
		if end > i {
			n++
		}
		return end
	})
	out, args := dropPatchArgs(out)
	if n += args; n == 0 || strings.Count(out, "apply_patch") != n || core.RunsWrittenFile().MatchString(out) {
		return cmd
	}
	return out
}

// patchHeredocEnd is the closing line of a `*** Begin Patch` heredoc fed to an unpiped `apply_patch`, else -1.
func patchHeredocEnd(lines []string, i int) int {
	line := lines[i]
	ms := core.HeredocOp().FindAllStringSubmatch(line, -1)
	if len(ms) != 1 || i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "*** Begin Patch" ||
		strings.Contains(line, "<<<") || strings.Contains(line, ">(") || strings.Contains(line, "<(") {
		return -1
	}
	segs, seps := shell.SegmentsSep(line)
	for k, seg := range segs {
		if !core.HeredocOp().MatchString(seg) {
			continue
		}
		if !patchCall(seg, core.HeredocOp().ReplaceAllString(seg, " "), seps[k]) {
			return -1
		}
		m := ms[0]
		end := core.HeredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-")
		if end < 0 || m[4] != "" && slices.ContainsFunc(lines[i+1:end], substitutes) { // a bare delimiter expands
			return -1
		}
		return end
	}
	return -1
}

// dropPatchArgs replaces each `apply_patch '<patch>'` segment's literal patch argument with nothing.
func dropPatchArgs(cmd string) (string, int) {
	segs, seps := shell.SegmentsSep(cmd)
	var b strings.Builder
	n := 0
	for i, seg := range segs {
		if toks := shell.Tokens(seg); len(toks) == 2 && strings.HasPrefix(toks[1], "*** Begin Patch") &&
			literalWord(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(seg), "apply_patch"))) &&
			patchCall(seg, seg, seps[i]) {
			seg = seg[:len(seg)-len(strings.TrimLeft(seg, " \t"))] + "apply_patch"
			n++
		}
		b.WriteString(seg)
		b.WriteString(seps[i])
	}
	return b.String(), n
}

// patchCall reports a segment whose command word is a bare `apply_patch` and whose output is not piped on.
func patchCall(seg, words, sep string) bool {
	toks := shell.Tokens(words)
	return len(toks) > 0 && toks[0] == "apply_patch" && strings.HasPrefix(strings.TrimLeft(seg, " \t"), "apply_patch ") &&
		sep != "|" && sep != "|&"
}

// literalWord reports one quoted word the shell passes as written: '…' alone, or "…" with no substitution.
func literalWord(w string) bool {
	if len(w) < 2 || w[len(w)-1] != w[0] {
		return false
	}
	return w[0] == '\'' && strings.Count(w, "'") == 2 || w[0] == '"' && !substitutes(w)
}

// substitutes reports command substitution text, which an unquoted heredoc or a "…" word runs.
func substitutes(s string) bool { return strings.Contains(s, "$(") || strings.Contains(s, "`") }

// dropInertHeredocs removes file-only heredoc bodies unless the command also runs a file.
func dropInertHeredocs(cmd string) string {
	return core.DropHeredocBodies(cmd, true, func(lines []string, i int) int {
		delim, quoted, dash, ok := core.InertHeredocStart(lines[i])
		if !ok {
			return -1
		}
		end := core.HeredocEnd(lines, i+1, delim, dash)
		// An unquoted delimiter expands substitutions in the body, so a body holding one is not inert.
		if end < 0 || !quoted && slices.ContainsFunc(lines[i+1:end], substitutes) {
			return -1
		}
		return end
	})
}

// protectedWrite is the first path a command writes or removes under the raw globs ("" if none); temp folders pass.
func protectedWrite(cmd, cwd string, raw []string, twin map[string]string) string {
	var globs []string
	folded := false
	fold := func() []string {
		if !folded {
			globs, folded = foldedGlobs(raw, cwd, twin), true
		}
		return globs
	}
	for _, p := range core.WrittenPaths(cmd, cwd) {
		if protectedPath(p, fold()) && !tempWriteAllowed(cmd, p) {
			return p
		}
	}
	removed := core.RemovedPaths(cmd, cwd)
	for _, p := range removed {
		if protectedPath(p, fold()) && !removalAllowed(cmd, removed) {
			return p
		}
	}
	return ""
}

// protectedPath reports whether p, or its symlink-resolved form, falls under a folded Edit deny glob.
func protectedPath(p string, globs []string) bool {
	return matchesProtected(p, globs) || matchesProtected(paths.Real(p), globs)
}

// interpreterWrites are the per-language write indicators; reads (open(p), readFile) match none.
var interpreterWrites = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python": regexp.MustCompile(`open\s*\([^\n]*?(,\s*|mode\s*=\s*)['"][rbt]*[wax+][rwxabt+]*['"]|` +
			`\b(write_text|write_bytes|unlink|copy\w*|move|replace|rename|remove|rmtree)\s*\(`),
		"js": regexp.MustCompile(`\b(writeFile|appendFile|createWriteStream|rename|unlink|rm|copyFile|truncate|writeTextFile|remove)` +
			`(Sync)?\s*\(|Bun\.write\b`),
		"script": regexp.MustCompile(`open\b[^\n]*(['"]\+?>>?|['"]\+?[wa]\+?['"])|` +
			`\b(File|IO)\.(binwrite|write|delete|unlink|rename)\b|\b(unlink|rename)\s*[(\s]|FileUtils\.`),
	}
})

// interpreterLang maps an interpreter's basename to its interpreterWrites key ("" when not an interpreter).
func interpreterLang(base string) string {
	switch {
	case strings.HasPrefix(base, "python"):
		return "python"
	case base == "node" || base == "deno" || base == "bun":
		return "js"
	case base == "perl" || base == "ruby":
		return "script"
	}
	return ""
}

// homePrefix is the home directory with a trailing slash, "" when unknown.
func homePrefix() string {
	if home, _ := os.UserHomeDir(); home != "" {
		return home + "/"
	}
	return ""
}

// interpreterWrite is the raw glob's needle (and glob) an interpreter command's text mentions beside a write indicator.
func interpreterWrite(cmd string, raw []string) (needle, glob string) {
	cmd = core.DropDataHeredocs(cmd)
	heredoc := strings.Contains(cmd, "<<")   // a script body sits on lines after its interpreter's segment
	whole := paths.FoldCase(pyUsedText(cmd)) // a Python comment or prose string only mentions a path
	segs := shell.Segments(cmd)
	assigns := assignSegments(segs)
	for _, seg := range segs {
		rest := runnerUnwrap(shell.Tokens(seg))
		if len(rest) == 0 {
			continue
		}
		text, used := cmd, whole
		if !heredoc { // judge this segment: a path another segment merely names is no write
			text = strings.Join(append([]string{seg}, assigns...), "\n")
			used = paths.FoldCase(text)
		}
		lang := interpreterLang(filepath.Base(rest[0]))
		if re := interpreterWrites()[lang]; re == nil || !scriptWrites(lang, re, text) {
			continue
		}
		if n, g := mentionedGlob(raw, used); n != "" {
			return n, g
		}
	}
	return "", ""
}

// mentionedGlob is the raw glob's needle (and glob) the folded text names.
func mentionedGlob(raw []string, used string) (needle, glob string) {
	home := homePrefix()
	for _, g := range raw {
		if n := globNeedle(g, home); n != "" && strings.Contains(used, paths.FoldCase(n)) {
			return n, g
		}
	}
	return "", ""
}

// assignSegments are the segments made only of NAME=value words: `f=~/.ssh/k; python3 -c ...` carries the path.
func assignSegments(segs []string) []string {
	var out []string
	for _, seg := range segs {
		if toks := shell.Tokens(seg); len(toks) > 0 && !slices.ContainsFunc(toks, func(t string) bool { return !isAssignment(t) }) {
			out = append(out, seg)
		}
	}
	return out
}

// pyIndirectModule spots os/shutil reached by another name (`as`, `fs = os`, `(os)`, `modules['os']`) or dynamically.
var pyIndirectModule = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bimport\s[^;\n]*\b(os|shutil|posix|nt)\s+as\b|` +
		`\bfrom\s+(os|shutil|posix|nt)\s+import\b|__import__|import_module|getattr|` +
		`[=(\[]\s*(os|shutil|posix|nt)\s*(?:[;\n,)\]]|$)|['"](os|shutil|posix|nt)['"]`)
})

// scriptWrites reports a write indicator for lang in cmd; a Python str/list .replace(a, b)/.remove(x) is none.
func scriptWrites(lang string, re *regexp.Regexp, cmd string) bool {
	if lang != "python" || pyIndirectModule().MatchString(cmd) {
		return re.MatchString(cmd)
	}
	for _, m := range re.FindAllStringSubmatchIndex(cmd, -1) {
		if m[4] < 0 || !pyDataCall(cmd, m[4], m[5], m[1]) { // m[4:6]: the write-call name group
			return true
		}
	}
	return false
}

// pyDataCall reports str.replace(a, b) or list.remove(x), not os.replace or one-argument Path.replace(target).
func pyDataCall(cmd string, start, end, args int) bool {
	name := cmd[start:end]
	if name != "replace" && name != "remove" || start == 0 || cmd[start-1] != '.' {
		return false
	}
	qual := cmd[:start-1]
	notIdent := func(r rune) bool { return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	qual = qual[strings.LastIndexFunc(qual, notIdent)+1:]
	if slices.Contains([]string{"os", "shutil", "posix", "nt"}, qual) {
		return false
	}
	n, ok := pyArgCount(cmd, args)
	if name == "replace" {
		return ok && n >= 2
	}
	return ok && n >= 1
}

// pyArgCount counts the top-level arguments of the call whose `(` ends at cmd[from-1]; ok=false when unclosed.
func pyArgCount(cmd string, from int) (n int, ok bool) {
	depth, quote, empty := 0, byte(0), true
	for i := from; i < len(cmd); i++ {
		c := cmd[i]
		closer := strings.IndexByte(")]}", c) >= 0
		switch {
		case quote != 0 && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote, empty = c, false
		case strings.IndexByte("([{", c) >= 0:
			depth, empty = depth+1, false
		case closer && depth > 0:
			depth--
		case closer:
			if empty {
				return 0, true
			}
			return n + 1, true
		case c == ',' && depth == 0:
			n++
		case c != ' ' && c != '\t':
			empty = false
		}
	}
	return 0, false
}

// matchesProtected reports whether abs falls under a deny glob: `**/x` by suffix, `dir/**` by prefix, else exact.
func matchesProtected(abs string, globs []string) bool {
	abs = paths.FoldCase(paths.CleanGlob(abs))
	return slices.ContainsFunc(globs, func(g string) bool {
		return paths.GlobMatch(g, abs) || strings.HasSuffix(g, "/**") && paths.GlobMatch(strings.TrimSuffix(g, "/**"), abs)
	})
}
