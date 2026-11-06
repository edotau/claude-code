// Scoped allowlist: judges shell commands for copilot's permission handler and the copilot-bash hook (merged from shellscope).

package shell

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
)

// readOnlyGit are the git subcommands a read-only run may shell out to.
var readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "blame": true, "rev-parse": true, "branch": true, "grep": true}

// gitBannedFlags also match any abbreviation git would accept (--out=x is --output=x).
var gitBannedFlags = []string{
	"--output", "--exec", "--ext-diff", "--config", "--open-files-in-pager",
	"--textconv", "--remerge-diff", "--diff-merges", "--submodule", // each still reaches a textconv or merge driver
	"--no-index", // diffs and greps paths outside the repository
}

// gitLeadingFlags must open the verb's args: later, a value option or -- swallows them (git diff -- --no-textconv).
var gitLeadingFlags = map[string][]string{
	"diff": {"--no-ext-diff", "--no-textconv"}, "log": {"--no-ext-diff", "--no-textconv"},
	"show": {"--no-ext-diff", "--no-textconv"}, "blame": {"--no-textconv"},
}

// goEnvPrefix is the only env assignment tolerated before `go` (cross-compile vet and build checks).
var goEnvPrefix = regexp.MustCompile(`^(GOOS|GOARCH|CGO_ENABLED)=[A-Za-z0-9_]+$`)

// stripGoEnv drops leading goEnvPrefix words from w when the command that follows is go; otherwise w is unchanged.
func stripGoEnv(w []string) []string {
	i := 0
	for i < len(w) && goEnvPrefix.MatchString(w[i]) {
		i++
	}
	if i == 0 || i >= len(w) || w[i] != "go" {
		return w
	}
	return w[i:]
}

// gitVerbFlags are those verbs' only dash args: branch must carry --list or -l, and `git status -v` runs textconv.
var gitVerbFlags = map[string]map[string]bool{
	"branch": {"--list": true, "-l": true, "-a": true, "--all": true, "-r": true, "--remotes": true, "-v": true, "-vv": true},
	"status": {"--short": true, "-s": true, "--branch": true, "-b": true, "--long": true, "--porcelain": true, "--porcelain=v2": true,
		"-z": true, "--ignored": true, "-uno": true, "-uall": true, "--": true},
}

// ReadOnlyGit reports whether cmd is a single allowlisted read-only git invocation (fail closed).
func ReadOnlyGit(cmd string) bool {
	if strings.ContainsFunc(cmd, unsafeGitRune) {
		return false
	}
	f := strings.Fields(cmd)
	if len(f) > 2 && f[0] == "git" && f[1] == "--no-pager" { // exactly one, which copilot adds itself
		f = append([]string{"git"}, f[2:]...)
	}
	if len(f) < 2 || f[0] != "git" || !readOnlyGit[f[1]] {
		return false
	}
	args, lead := f[2:], gitLeadingFlags[f[1]]
	if len(args) < len(lead) || !slices.Equal(args[:len(lead)], lead) {
		return false
	}
	return readOnlyGitArgs(f[1], args)
}

// unsafeGitRune is any rune outside letters, digits and the plain punctuation a read-only git command needs.
func unsafeGitRune(r rune) bool {
	return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" _./:=@%+,^-~", r)
}

// readOnlyGitArgs checks verb's args against the banned and per-verb allowed flags; branch must list.
func readOnlyGitArgs(verb string, args []string) bool {
	allowed, list := gitVerbFlags[verb], false
	for _, a := range args {
		if badGitArg(a, allowed) {
			return false
		}
		list = list || a == "--list" || a == "-l"
	}
	return (verb != "branch" || list) && (verb != "grep" || grepArgsSafe(args))
}

// badGitArg is -c, a ~ path, a banned flag, or a dash arg outside the verb's allowed set (when it has one).
func badGitArg(a string, allowed map[string]bool) bool {
	return a == "-c" || strings.HasPrefix(a, "~") || bannedGitFlag(a) || allowed != nil && strings.HasPrefix(a, "-") && !allowed[a]
}

// grepArgsSafe refuses git grep's pager flag (-O, --open-files-in-pager) and a pattern file (-f) outside the checkout.
func grepArgsSafe(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return true
		case strings.HasPrefix(a, "--"):
			name, val, hasEq := strings.Cut(a, "=")
			if len(name) >= 4 && strings.HasPrefix("--file", name) { // --fi.. may abbreviate --file
				if !hasEq && i+1 < len(args) {
					i, val = i+1, args[i+1]
				}
				if escapesPath(val) {
					return false
				}
			}
		case strings.HasPrefix(a, "-"):
			var ok bool
			if i, ok = grepShortCluster(args, i); !ok {
				return false
			}
		}
	}
	return true
}

// grepShortCluster walks the short flags of args[i]; a value letter ends the cluster (its value may be the next arg).
func grepShortCluster(args []string, i int) (int, bool) {
	a := args[i]
	for j := 1; j < len(a); j++ {
		switch a[j] {
		case 'O':
			return i, false
		case 'f', 'e', 'A', 'B', 'C', 'm':
			val := a[j+1:]
			if val == "" && i+1 < len(args) {
				i, val = i+1, args[i+1]
			}
			return i, a[j] != 'f' || !escapesPath(val)
		}
	}
	return i, true
}

// escapesPath is a value that is absolute, home-relative or climbs with .. (it may leave every root).
func escapesPath(v string) bool {
	return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~") || slices.Contains(strings.Split(v, "/"), "..")
}

func bannedGitFlag(arg string) bool {
	name, _, _ := strings.Cut(arg, "=")
	if !strings.HasPrefix(name, "--") || len(name) < 3 {
		return false
	}
	for _, b := range gitBannedFlags {
		if strings.HasPrefix(b, name) {
			return true
		}
	}
	return false
}

// resolveReal makes p absolute under base and resolves symlinks through its deepest existing ancestor.
func resolveReal(p, base string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return "~escape" // never inside a workdir
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	var rest []string
	for cur := p; ; cur = filepath.Dir(cur) {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		if cur == filepath.Dir(cur) {
			return p
		}
		rest = slices.Insert(rest, 0, filepath.Base(cur))
	}
}

// relInside returns p's path relative to the resolved workDir, false when it escapes.
func relInside(p, workDir string) (string, bool) {
	root := resolveReal(workDir, ".")
	rel, err := filepath.Rel(root, resolveReal(p, root))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// WriteAllowed reports whether a write to file stays inside workDir and clear of secrets, .git and state.
func WriteAllowed(file, workDir string) bool {
	rel, ok := relInside(file, workDir)
	if !ok || file == "" {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if slices.Contains(parts, ".git") || parts[0] == "state" {
		return false
	}
	base := parts[len(parts)-1]
	ext := filepath.Ext(base)
	return !strings.HasPrefix(base, ".env") && ext != ".pem" && ext != ".key" &&
		base != ".credentials.json" && !strings.HasPrefix(base, "vault.db")
}

// splitShell splits cmd on && || ; | outside quotes into word lists; false on anything it cannot judge.
func splitShell(cmd string) ([][]string, bool) {
	var s shellWords
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		var ok bool
		if i, ok = s.step(rs, i); !ok {
			return nil, false
		}
	}
	if s.quote != 0 || !s.flushSeg() {
		return nil, false
	}
	return s.segs, true
}

// shellWords is splitShell's walk state: finished segments, the current segment's words, the word being built.
type shellWords struct {
	segs   [][]string
	words  []string
	cur    strings.Builder
	quote  rune
	inWord bool
}

// step consumes rs[i] (and a rune it pairs with), returning the last index used; false on anything not judged.
func (s *shellWords) step(rs []rune, i int) (int, bool) {
	r := rs[i]
	switch {
	case s.quote != 0:
		return i, s.quoted(r)
	case !s.inWord && stderrToStdout(rs, i):
		return i + len("2>&1") - 1, true // 2>&1 only merges output already going to the terminal
	case strings.ContainsRune("`$<>\\\n\r(){}*?[~", r):
		return i, false // substitution, redirection, globbing and grouping are not judged
	case strings.ContainsRune(`'"`, r):
		s.quote, s.inWord = r, true
	case strings.ContainsRune(" \t", r):
		s.flushWord()
	case strings.ContainsRune(";|&", r):
		end, ok := separatorEnd(rs, i)
		return end, ok && s.flushSeg()
	default:
		s.cur.WriteRune(r)
		s.inWord = true
	}
	return i, true
}

// quoted handles one rune inside quotes: the closing quote ends them, any other rune joins the word.
func (s *shellWords) quoted(r rune) bool {
	if strings.ContainsRune("`$\\\n\r", r) {
		return false // expansion inside double quotes is not judged
	}
	if r == s.quote {
		s.quote = 0
	} else {
		s.cur.WriteRune(r)
	}
	return true
}

func (s *shellWords) flushWord() {
	if s.inWord {
		s.words = append(s.words, s.cur.String())
		s.cur.Reset()
		s.inWord = false
	}
}

func (s *shellWords) flushSeg() bool {
	s.flushWord()
	if len(s.words) == 0 {
		return false
	}
	s.segs, s.words = append(s.segs, s.words), nil
	return true
}

// separatorEnd is the last index of the separator at rs[i] (`&&` and `||` take two); false on a lone `&`.
func separatorEnd(rs []rune, i int) (int, bool) {
	r := rs[i]
	if r == '&' && (i+1 >= len(rs) || rs[i+1] != '&') {
		return i, false
	}
	if i+1 < len(rs) && rs[i+1] == r && r != ';' {
		return i + 1, true
	}
	return i, true
}

// stderrToStdout reports whether rs[i:] starts a standalone "2>&1" token.
func stderrToStdout(rs []rune, i int) bool {
	end := i + len("2>&1")
	return end <= len(rs) && string(rs[i:end]) == "2>&1" && (end == len(rs) || strings.ContainsRune(" \t;|", rs[end]))
}

// CanonicalSelf rewrites the harness binary's full paths ($HOME, ~ or absolute) to plain claude-code.
func CanonicalSelf(cmd string) string {
	home, _ := os.UserHomeDir()
	for _, self := range []string{"$HOME/.claude/bin/claude-code", "~/.claude/bin/claude-code", home + "/.claude/bin/claude-code"} {
		cmd = strings.ReplaceAll(cmd, self+" ", "claude-code ")
	}
	return cmd
}

// Command is CommandIn with roots[0] as the execution cwd (the hook runs in its first root).
func Command(cmd string, roots []string, extra agentconfig.ShellRules) bool {
	if len(roots) == 0 {
		return false
	}
	return CommandIn(cmd, roots[0], roots, extra)
}

// CommandIn reports whether every segment of cmd, run in cwd, passes the policy's commands or extra (which may be nil)
// in a root; cwd must be absolute and inside a root, and relative operands resolve against it, never another root.
func CommandIn(cmd, cwd string, roots []string, extra agentconfig.ShellRules) bool {
	if !filepath.IsAbs(cwd) || !slices.ContainsFunc(roots, func(root string) bool { _, ok := relInside(cwd, root); return ok }) {
		return false
	}
	segs, ok := splitShell(cmd)
	if !ok {
		return false
	}
	rules := agentconfig.ShellPolicy().Commands
	for _, w := range segs {
		w = absolutizeOperands(stripGoEnv(w), cwd)
		if !slices.ContainsFunc(roots, func(root string) bool { return allows(rules, w, root) || allows(extra, w, root) }) {
			return false
		}
	}
	return true
}

// checks are the Go rules a policy entry names by "check"; each gets the args after the matched subcommand.
var checks = map[string]func(args []string, workDir string) bool{
	"scopedGit":    func(args []string, workDir string) bool { return scopedGit(append([]string{"git"}, args...), workDir) },
	"noOutputFlag": func(args []string, _ string) bool { return !outputFlag(args) },
	"regularFiles": rmAllowed,
}

// allows reports whether any rule for w's command admits w inside workDir (an unknown check admits nothing).
func allows(rules agentconfig.ShellRules, w []string, workDir string) bool {
	return slices.ContainsFunc(rules, func(r agentconfig.ShellRule) bool {
		return slices.Contains(r.Commands, w[0]) && ruleAllows(r, w[1:], workDir)
	})
}

func ruleAllows(r agentconfig.ShellRule, args []string, workDir string) bool {
	rest, ok := matchArgs(r, args)
	if !ok || !noFlag(args, r.BannedFlags...) || r.ArgsInside && !argsInside(args, workDir) {
		return false
	}
	if r.Check == "" {
		return true
	}
	check, ok := checks[r.Check]
	return ok && check(rest, workDir)
}

// matchArgs matches args against the rule's subcommands, exact forms and targets; rest follows the subcommand.
func matchArgs(r agentconfig.ShellRule, args []string) ([]string, bool) {
	if len(r.Targets) > 0 {
		return nil, len(args) > 0 && !slices.ContainsFunc(args, func(a string) bool { return !slices.Contains(r.Targets, a) })
	}
	if len(r.Subcommands) == 0 && len(r.Exact) == 0 {
		return args, true
	}
	for _, s := range r.Subcommands {
		if sub := strings.Fields(s); len(args) >= len(sub) && slices.Equal(args[:len(sub)], sub) {
			return args[len(sub):], true
		}
	}
	return nil, slices.ContainsFunc(r.Exact, func(e string) bool { return slices.Equal(args, strings.Fields(e)) })
}

// outputFlag reports a -o/--output flag in any dash count or =value form.
func outputFlag(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		return strings.HasPrefix(a, "-") && (name == "o" || name == "output")
	})
}

// noFlag reports whether no arg is a banned flag in any dash count or =value form (go takes -x and --x alike).
func noFlag(args []string, banned ...string) bool {
	return !slices.ContainsFunc(args, func(a string) bool {
		if !strings.HasPrefix(a, "-") {
			return false
		}
		name, _, _ := strings.Cut(a, "=")
		return slices.ContainsFunc(banned, func(b string) bool { return strings.TrimLeft(b, "-") == strings.TrimLeft(name, "-") })
	})
}

// pathLooking is an absolute path or one climbing with ..; relative names stay inside by construction.
func pathLooking(a string) bool {
	return strings.HasPrefix(a, "/") || a == ".." || strings.HasPrefix(a, "../") || strings.Contains(a, "/../") || strings.HasSuffix(a, "/..")
}

// optionValue is the value of a "-name=value" flag whatever its name, or of an attached -o/-C ("-o/x"); "" otherwise.
func optionValue(a string) string {
	if !strings.HasPrefix(a, "-") {
		return ""
	}
	flag := strings.TrimLeft(a, "-")
	if _, val, ok := strings.Cut(flag, "="); ok {
		return val
	}
	if len(flag) > 1 && (flag[0] == 'o' || flag[0] == 'C') {
		return flag[1:]
	}
	return ""
}

// argsInside requires every path-looking arg, and every -name=value or attached -o/-C value, to resolve inside workDir.
func argsInside(args []string, workDir string) bool {
	for _, a := range args {
		if strings.HasPrefix(optionValue(a), "~") {
			return false
		}
		for _, v := range []string{a, optionValue(a)} {
			if !pathLooking(v) {
				continue
			}
			if _, ok := relInside(v, workDir); !ok {
				return false
			}
		}
	}
	return true
}

// absolutizeOperands joins rm's, git add/commit's plain relative operands and git -C's dir onto cwd, so no root can rebase them.
func absolutizeOperands(w []string, cwd string) []string {
	first, last := 1, len(w)
	switch {
	case w[0] == "git" && len(w) >= 3 && w[1] == "-C":
		first, last = 2, 3
	case w[0] == "git" && len(w) >= 3 && (w[1] == "add" || w[1] == "commit"):
		first = 2
	case w[0] != "rm":
		return w
	}
	out := slices.Clone(w)
	for i := first; i < last; i++ {
		if rmOperandPlain(out[i]) && !filepath.IsAbs(out[i]) && !(w[0] == "git" && strings.HasSuffix(out[i], "/")) {
			out[i] = filepath.Join(cwd, out[i])
		}
	}
	return out
}

// rmOperandPlain is an operand with no flag, home, glob or .. form.
func rmOperandPlain(a string) bool {
	return !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "~") && !strings.ContainsAny(a, "*?[{") &&
		!slices.Contains(strings.Split(a, "/"), "..")
}

// rmAllowed admits plain `rm` of absolute (see absolutizeOperands) existing regular files, never symlinks, inside workDir.
func rmAllowed(args []string, workDir string) bool {
	if len(args) == 0 {
		return false
	}
	home, _ := os.UserHomeDir()
	state := filepath.Join(home, ".claude", "state")
	for _, a := range args {
		if !rmOperandPlain(a) || !filepath.IsAbs(a) {
			return false
		}
		abs := filepath.Clean(a)
		if st, err := os.Lstat(abs); err != nil || !st.Mode().IsRegular() {
			return false
		}
		if _, ok := relInside(abs, workDir); !ok || rmProtected(abs, state) || rmProtected(resolveReal(abs, workDir), resolveReal(state, "/")) {
			return false
		}
	}
	return true
}

// rmProtected is a path (lexical or resolved) inside .git or the state dir, or a settings, credential or vault file, any case.
func rmProtected(path, state string) bool {
	low := strings.ToLower(path)
	st := strings.ToLower(state)
	if slices.Contains(strings.Split(low, string(filepath.Separator)), ".git") ||
		low == st || strings.HasPrefix(low, st+string(filepath.Separator)) {
		return true
	}
	base := filepath.Base(low)
	return base == "settings.json" || base == "settings.local.json" || strings.HasSuffix(base, ".credentials.json") ||
		strings.HasPrefix(base, "vault.db")
}

func scopedGit(w []string, workDir string) bool {
	if len(w) >= 3 && w[1] == "-C" { // git -C <dir> is the same command run from dir, which must stay inside workDir
		dir := resolveReal(w[2], workDir)
		if _, ok := relInside(dir, workDir); !ok {
			return false
		}
		return scopedGit(append([]string{"git"}, w[3:]...), dir)
	}
	if ReadOnlyGit(strings.Join(w, " ")) {
		return true
	}
	if len(w) < 2 {
		return false
	}
	switch w[1] {
	case "add":
		return gitAddAllowed(w[2:], workDir)
	case "commit":
		return gitCommitAllowed(w[2:], workDir)
	}
	return false
}

func gitAddAllowed(paths []string, workDir string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !gitAddPathAllowed(p, workDir) {
			return false
		}
	}
	return true
}

// gitAddPathAllowed admits one named file inside workDir: no flag, directory, `.`/`..` or trailing slash.
func gitAddPathAllowed(p, workDir string) bool {
	if strings.HasPrefix(p, "-") || p == "." || p == ".." || strings.HasSuffix(p, "/") {
		return false
	}
	rel, ok := relInside(p, workDir)
	if !ok {
		return false
	}
	if st, err := os.Stat(resolveReal(p, resolveReal(workDir, "."))); err == nil && st.IsDir() || rel == "." {
		return false
	}
	return true
}

// gitCommitAllowed admits flag-safe commits whose pathspecs pass gitAddPathAllowed (files inside workDir).
func gitCommitAllowed(args []string, workDir string) bool {
	skip := false
	for _, a := range args {
		ok := true
		switch {
		case skip:
			skip = false
		case strings.HasPrefix(a, "--"):
			ok, skip = commitLongFlag(a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			ok, skip = commitShortFlags(a)
		default:
			ok = gitAddPathAllowed(a, workDir)
		}
		if !ok {
			return false
		}
	}
	return !skip
}

// commitBannedLong are the long commit flags that rewrite history, skip hooks or reuse another commit's message.
var commitBannedLong = []string{"--amend", "--no-verify", "--reuse-message", "--reedit-message"}

// commitLongFlag bans commitBannedLong (and abbreviations) and a bare --; skip means --message takes the next arg.
func commitLongFlag(a string) (ok, skip bool) {
	name, _, _ := strings.Cut(a, "=")
	banned := slices.ContainsFunc(commitBannedLong, func(b string) bool { return strings.HasPrefix(b, name) })
	if len(name) < 3 || a == "--" || banned {
		return false, false
	}
	return true, name == "--message" && !strings.Contains(a, "=")
}

// commitShortFlags judges a short-flag cluster; skip means a trailing m takes the next arg.
func commitShortFlags(a string) (ok, skip bool) {
	if strings.ContainsAny(a, "ncC") { // -n skips hooks; -c/-C reuse another commit
		return false, false
	}
	m := strings.IndexByte(a, 'm')
	return true, m == len(a)-1 // text after the first m is the attached message; only a trailing m takes the next arg
}
