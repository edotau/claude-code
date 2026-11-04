// Package shellscope judges shell commands against the scoped allowlist shared by copilot's permission handler and hooks.
package shellscope

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
)

// readOnlyGit are the git subcommands a read-only run may shell out to.
var readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "blame": true, "rev-parse": true, "branch": true}

// gitBannedFlags also match any abbreviation git would accept (--out=x is --output=x).
var gitBannedFlags = []string{
	"--output", "--exec", "--ext-diff", "--config", "--open-files-in-pager",
	"--textconv", "--remerge-diff", "--diff-merges", "--submodule", // each still reaches a textconv or merge driver
}

// gitLeadingFlags must open the verb's args: later, a value option or -- swallows them (git diff -- --no-textconv).
var gitLeadingFlags = map[string][]string{
	"diff": {"--no-ext-diff", "--no-textconv"}, "log": {"--no-ext-diff", "--no-textconv"},
	"show": {"--no-ext-diff", "--no-textconv"}, "blame": {"--no-textconv"},
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
	return verb != "branch" || list
}

// badGitArg is -c, a ~ path, a banned flag, or a dash arg outside the verb's allowed set (when it has one).
func badGitArg(a string, allowed map[string]bool) bool {
	return a == "-c" || strings.HasPrefix(a, "~") || bannedGitFlag(a) || allowed != nil && strings.HasPrefix(a, "-") && !allowed[a]
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

// ResolvePath makes p absolute under base and resolves symlinks through its deepest existing ancestor.
func ResolvePath(p, base string) string {
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

// RelInside returns p's path relative to the resolved workDir, false when it escapes.
func RelInside(p, workDir string) (string, bool) {
	root := ResolvePath(workDir, ".")
	rel, err := filepath.Rel(root, ResolvePath(p, root))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// WriteAllowed reports whether a write to file stays inside workDir and clear of secrets, .git and state.
func WriteAllowed(file, workDir string) bool {
	rel, ok := RelInside(file, workDir)
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

// Command reports whether every segment of cmd passes the policy's commands, or extra (which may be nil), in a root.
func Command(cmd string, roots []string, extra agentconfig.ShellRules) bool {
	segs, ok := splitShell(cmd)
	if !ok {
		return false
	}
	rules := agentconfig.ShellPolicy().Commands
	for _, w := range segs {
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

// noFlag reports whether no arg is (or starts with, for a =value form) one of the banned flags.
func noFlag(args []string, banned ...string) bool {
	return !slices.ContainsFunc(args, func(a string) bool {
		name, _, _ := strings.Cut(a, "=")
		return slices.Contains(banned, name)
	})
}

// argsInside requires every path-looking arg (absolute or with ..) to resolve inside workDir.
func argsInside(args []string, workDir string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "/") || a == ".." || strings.HasPrefix(a, "../") || strings.Contains(a, "/../") || strings.HasSuffix(a, "/..") {
			if _, ok := RelInside(a, workDir); !ok {
				return false
			}
		}
	}
	return true
}

func scopedGit(w []string, workDir string) bool {
	if len(w) >= 3 && w[1] == "-C" { // git -C <dir> is the same command run from dir, which must stay inside workDir
		dir := ResolvePath(w[2], workDir)
		if _, ok := RelInside(dir, workDir); !ok {
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
		return gitCommitAllowed(w[2:])
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
	rel, ok := RelInside(p, workDir)
	if !ok {
		return false
	}
	if st, err := os.Stat(ResolvePath(p, ResolvePath(workDir, "."))); err == nil && st.IsDir() || rel == "." {
		return false
	}
	return true
}

func gitCommitAllowed(args []string) bool {
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
		}
		if !ok {
			return false
		}
	}
	return !skip
}

// commitLongFlag bans --amend/--no-verify (and abbreviations) and a bare --; skip means --message takes the next arg.
func commitLongFlag(a string) (ok, skip bool) {
	name, _, _ := strings.Cut(a, "=")
	if len(name) < 3 || strings.HasPrefix("--amend", name) || strings.HasPrefix("--no-verify", name) || a == "--" {
		return false, false
	}
	return true, name == "--message" && !strings.Contains(a, "=")
}

// commitShortFlags judges a short-flag cluster; skip means a trailing m takes the next arg.
func commitShortFlags(a string) (ok, skip bool) {
	if strings.ContainsAny(a, "ncC") { // -n skips hooks; -c/-C reuse another commit
		return false, false
	}
	return true, strings.HasSuffix(a, "m")
}
