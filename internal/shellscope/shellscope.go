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
	for _, r := range cmd {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" _./:=@%+,^-~", r) {
			return false
		}
	}
	f := strings.Fields(cmd)
	if len(f) < 2 || f[0] != "git" || !readOnlyGit[f[1]] {
		return false
	}
	args, lead := f[2:], gitLeadingFlags[f[1]]
	if len(args) < len(lead) || !slices.Equal(args[:len(lead)], lead) {
		return false
	}
	allowed, list := gitVerbFlags[f[1]], false
	for _, a := range args {
		if a == "-c" || strings.HasPrefix(a, "~") || bannedGitFlag(a) {
			return false
		}
		if allowed != nil && strings.HasPrefix(a, "-") && !allowed[a] {
			return false
		}
		list = list || a == "--list" || a == "-l"
	}
	return f[1] != "branch" || list
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
	var segs [][]string
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	flushSeg := func() bool {
		flushWord()
		if len(words) == 0 {
			return false
		}
		segs, words = append(segs, words), nil
		return true
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0 && strings.ContainsRune("`$\\\n\r", r):
			return nil, false // expansion inside double quotes is not judged
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case !inWord && quote == 0 && stderrToStdout(rs, i):
			i += len("2>&1") - 1 // 2>&1 only merges output already going to the terminal
		case strings.ContainsRune("`$<>\\\n\r(){}*?[~", r):
			return nil, false // substitution, redirection, globbing and grouping are not judged
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			flushWord()
		case r == ';' || r == '|' || r == '&':
			if r == '&' && (i+1 >= len(rs) || rs[i+1] != '&') {
				return nil, false
			}
			if i+1 < len(rs) && rs[i+1] == r && r != ';' {
				i++
			}
			if !flushSeg() {
				return nil, false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || !flushSeg() {
		return nil, false
	}
	return segs, true
}

// stderrToStdout reports whether rs[i:] starts a standalone "2>&1" token.
func stderrToStdout(rs []rune, i int) bool {
	end := i + len("2>&1")
	return end <= len(rs) && string(rs[i:end]) == "2>&1" && (end == len(rs) || strings.ContainsRune(" \t;|", rs[end]))
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
	}
	return true
}

func gitCommitAllowed(args []string) bool {
	skip := false
	for _, a := range args {
		switch {
		case skip:
			skip = false
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(a, "=")
			if len(name) < 3 || strings.HasPrefix("--amend", name) || strings.HasPrefix("--no-verify", name) || a == "--" {
				return false
			}
			skip = name == "--message" && !strings.Contains(a, "=")
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a, "nc") { // -n skips hooks; -c/-C reuse another commit
				return false
			}
			skip = strings.HasSuffix(a, "m")
		}
	}
	return !skip
}
