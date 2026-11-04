package transcript

import (
	"regexp"
	"strings"
)

// gitValueOpts are git global options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--super-prefix": true, "--config-env": true,
}

// gitShortCircuitOpts are git global options that print something and exit before any verb runs —
// `--exec-path` only short-circuits bare (an `=`-valued form supplies a path, not a request to print one).
var gitShortCircuitOpts = map[string]bool{
	"--version": true, "--help": true, "-h": true, "--exec-path": true,
	"--html-path": true, "--man-path": true, "--info-path": true,
}

// maxShellRecursionDepth bounds `bash -c "..."` (and sh/zsh) recursion into the quoted argument.
const maxShellRecursionDepth = 3

// shellCNames are the interpreters whose `-c <script>` argument runs as a fresh shell command line.
var shellCNames = map[string]bool{"bash": true, "sh": true, "zsh": true}

// gitCommitCommand reports whether cmd actually RUNS `git … commit …` in some shell segment: split on
// &&/||/;/|/newline, tokenize each segment (quotes collapse to one token, so quoted text never matches),
// skip a leading env assignment or sudo/env, then require `git` (or a .../git path) followed by global
// options up to the verb `commit`. Heredoc bodies are skipped first (this is a counter, not the safety
// guard — undercounting a piped heredoc body is acceptable); `$(...)`/backtick substitutions are scanned
// too, since a captured `git commit` still runs.
func gitCommitCommand(cmd string) bool {
	return gitCommitCommandDepth(cmd, 0)
}

func gitCommitCommandDepth(cmd string, depth int) bool {
	cmd = stripHeredocBodies(cmd)
	if commitInText(cmd, depth) {
		return true
	}
	for _, sub := range extractSubstitutions(cmd) {
		if commitInText(stripHeredocBodies(sub), depth) {
			return true
		}
	}
	return false
}

// commitInText runs the segment/token git-commit check over one piece of shell text (a command, or a
// `$(...)`/backtick substitution's inner text); depth bounds `bash -c`-style recursion.
func commitInText(text string, depth int) bool {
	for _, seg := range commitShellSegments(text) {
		toks := skipCommitWrappers(commitShellTokens(seg))
		if len(toks) == 0 {
			continue
		}
		if commitViaShellC(toks, depth) || isGitCommitInvocation(toks, depth) {
			return true
		}
	}
	return false
}

// commitViaShellC reports whether toks is a `bash -c "..."`-style wrapper whose quoted script commits.
func commitViaShellC(toks []string, depth int) bool {
	if depth >= maxShellRecursionDepth || !shellCNames[shellBaseName(toks[0])] {
		return false
	}
	inner, ok := findDashCArg(toks[1:])
	return ok && gitCommitCommandDepth(inner, depth+1)
}

// isGitCommitInvocation reports whether toks is a `git [global opts] commit` invocation (alias or literal).
func isGitCommitInvocation(toks []string, depth int) bool {
	if toks[0] != "git" && !strings.HasSuffix(toks[0], "/git") {
		return false
	}
	i, aliases, shortCircuit := scanGitGlobalOpts(toks)
	if shortCircuit {
		return false
	}
	return i < len(toks) && (toks[i] == "commit" || aliasIsCommit(aliases[toks[i]], depth))
}

// scanGitGlobalOpts walks git's global options after toks[0]: it returns the verb index, the -c aliases seen
// and whether an option such as --version short-circuits before any verb.
func scanGitGlobalOpts(toks []string) (i int, aliases map[string]string, shortCircuit bool) {
	i = 1
	aliases = map[string]string{}
	for i < len(toks) && strings.HasPrefix(toks[i], "-") {
		if gitShortCircuitOpts[toks[i]] {
			return i, aliases, true
		}
		if gitValueOpts[toks[i]] && !strings.Contains(toks[i], "=") {
			if toks[i] == "-c" && i+1 < len(toks) {
				if name, val, ok := splitAliasAssign(toks[i+1]); ok && !gitBuiltinVerbs[name] {
					aliases[name] = val // last -c alias.<name>=<val> for a given name wins
				}
			}
			i += 2
			continue
		}
		i++
	}
	return i, aliases, false
}

// shellBaseName strips a leading directory from a `/bin/bash`-style interpreter path.
func shellBaseName(tok string) string {
	if _, after, ok := strings.CutLast(tok, "/"); ok {
		return after
	}
	return tok
}

// shellLeadingFlags are bash/sh/zsh flags with no attached value, skippable before -c.
var shellLeadingFlags = map[string]bool{"-e": true, "-x": true, "-l": true, "--norc": true, "--noprofile": true}

// findDashCArg returns the argument following a `-c` flag among a shell interpreter's leading flags —
// combined short opts ending in `c` (`-xc`, `-ec`, `-exc`) count as `-c`; an `-n`/no-exec flag anywhere
// in a combined form (including `-nc`) means the script never runs, so it reports no match at all.
func findDashCArg(toks []string) (string, bool) {
	for i := 0; i < len(toks); {
		t := toks[i]
		if !strings.HasPrefix(t, "-") {
			return "", false
		}
		if t == "-o" {
			i += 2
			continue
		}
		if !strings.HasPrefix(t, "--") {
			body := t[1:]
			if strings.ContainsRune(body, 'n') {
				return "", false // -n (noexec) blocks execution even combined with a trailing c
			}
			if strings.HasSuffix(body, "c") {
				return argAfter(toks, i)
			}
		}
		if !shellLeadingFlags[t] {
			return "", false
		}
		i++
	}
	return "", false
}

// argAfter returns toks[i+1] when it exists.
func argAfter(toks []string, i int) (string, bool) {
	if i+1 < len(toks) {
		return toks[i+1], true
	}
	return "", false
}

// gitBuiltinVerbs are git's own command names — git ignores a user alias defined under one of these
// names, so `-c alias.<builtin>=…` never registers (the builtin behavior still runs unaltered).
var gitBuiltinVerbs = map[string]bool{
	"status": true, "add": true, "log": true, "diff": true, "push": true, "pull": true, "fetch": true,
	"checkout": true, "switch": true, "branch": true, "merge": true, "rebase": true, "reset": true,
	"show": true, "tag": true, "stash": true, "clone": true, "init": true, "rm": true, "mv": true,
	"restore": true, "commit": true,
}

// splitAliasAssign splits `alias.<name>=<value>` (from `git -c <val>`) into name and raw value.
func splitAliasAssign(val string) (name, value string, ok bool) {
	n, v, hasEq := strings.Cut(val, "=")
	if !hasEq {
		return "", "", false
	}
	const prefix = "alias."
	if !strings.HasPrefix(n, prefix) {
		return "", "", false
	}
	return strings.TrimPrefix(n, prefix), v, true
}

// aliasIsCommit reports whether an alias's raw value runs a commit: either directly (`commit`) or as a
// shell alias (`!git commit …`, recursed depth-limited); val == "" means no alias was registered.
func aliasIsCommit(val string, depth int) bool {
	if val == "" {
		return false
	}
	if val == "commit" {
		return true
	}
	if rest, ok := strings.CutPrefix(val, "!"); ok {
		return depth < maxShellRecursionDepth && gitCommitCommandDepth(rest, depth+1)
	}
	return false
}

// extractSubstitutions returns the inner text of every `$(...)` and backtick command substitution in
// cmd, recursively — quoting around the substitution is irrelevant to whether its inner command runs.
func extractSubstitutions(cmd string) []string {
	var out []string
	for i := 0; i < len(cmd); i++ {
		inner, last, state := substitutionAt(cmd, i)
		switch state {
		case subOpen:
			return out
		case subFound:
			out = append(out, inner)
			out = append(out, extractSubstitutions(inner)...)
			i = last
		}
	}
	return out
}

// Outcomes of substitutionAt: nothing opens here, a closed substitution, or one that never closes.
const (
	subNone = iota
	subFound
	subOpen
)

// substitutionAt reports the substitution opening at cmd[i]: its inner text and the index of its last byte.
func substitutionAt(cmd string, i int) (inner string, last, state int) {
	switch {
	case cmd[i] == '`':
		j := strings.IndexByte(cmd[i+1:], '`')
		if j < 0 {
			return "", 0, subOpen
		}
		return cmd[i+1 : i+1+j], i + 1 + j, subFound
	case cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '(' && (i+2 >= len(cmd) || cmd[i+2] != '('):
		return dollarSubstitution(cmd, i)
	}
	return "", 0, subNone
}

// dollarSubstitution scans the `$(` at cmd[i] to its matching close paren.
func dollarSubstitution(cmd string, i int) (inner string, last, state int) {
	depth, k := 1, i+2
	for k < len(cmd) && depth > 0 {
		switch cmd[k] {
		case '(':
			depth++
		case ')':
			depth--
		}
		k++
	}
	if depth > 0 {
		return "", 0, subOpen // unterminated `$(` — nothing closes it, so stop scanning rather than slice
	}
	return cmd[i+2 : k-1], k - 1, subFound
}

// stripHeredocBodies removes the body lines of every `<<WORD`/`<<'WORD'`/`<<-WORD` heredoc (and its
// terminator line) from cmd, so a commit mentioned only inside a heredoc body never counts as run.
func stripHeredocBodies(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		word, ok := heredocWord(lines[i])
		if !ok {
			continue
		}
		i++
		for i < len(lines) && strings.TrimSpace(lines[i]) != word {
			i++
		}
		// i now sits on the terminator line (or past EOF); either way it is dropped, not re-appended.
	}
	return strings.Join(out, "\n")
}

var bareHeredocWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// heredocWord reports the terminator word of a `<<[-]WORD` (quoted or bare) on line, false for a
// here-string (`<<<`) or a line with no heredoc redirect at all.
func heredocWord(line string) (string, bool) {
	idx := strings.Index(line, "<<")
	if idx < 0 || (idx+2 < len(line) && line[idx+2] == '<') {
		return "", false
	}
	rest := strings.TrimLeft(strings.TrimPrefix(line[idx+2:], "-"), " \t")
	if rest == "" {
		return "", false
	}
	if rest[0] == '\'' || rest[0] == '"' {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", false
		}
		return rest[1 : 1+end], rest[1:1+end] != ""
	}
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	word := rest[:end]
	return word, bareHeredocWord.MatchString(word) // rejects `$((1<<4))` and `"a <<b"`
}

// commitWrapperCmds pass their argument list through to run as the real command, not git themselves.
var commitWrapperCmds = map[string]bool{"sudo": true, "env": true, "time": true, "command": true, "exec": true, "nohup": true}

// commitShellKeywords start a fresh command after a shell control-flow keyword (if/while/for bodies).
var commitShellKeywords = map[string]bool{"then": true, "do": true, "else": true, "elif": true}

// sudoValueFlags are sudo flags whose value is the next token (`sudo -u user …`, `sudo -g group …`).
var sudoValueFlags = map[string]bool{"-u": true, "--user": true, "-g": true, "--group": true}

// envValueFlags are env flags whose value is the next token (`env -u NAME …`); `--` also ends env's
// own flags (env's next token is the command even if it starts with `-`).
var envValueFlags = map[string]bool{"-u": true, "--unset": true}

// skipCommitWrappers drops leading `NAME=value` env assignments, a leading `!`/shell keyword, and
// wrapper commands (`sudo`, `env`, `time`, `command`, `exec`, `nohup` — plus sudo's/env's own flags)
// before `git` (`env -i PATH=/x git commit` must still reach `git`).
func skipCommitWrappers(toks []string) []string {
	for len(toks) > 0 {
		if toks[0] == "!" || commitShellKeywords[toks[0]] {
			toks = toks[1:]
			continue
		}
		if commitWrapperCmds[toks[0]] {
			toks = skipWrapperFlags(toks[0], toks[1:])
			continue
		}
		if name, _, ok := strings.Cut(toks[0], "="); ok && name != "" && !strings.ContainsAny(name, " \t") {
			toks = toks[1:]
			continue
		}
		break
	}
	return toks
}

// skipWrapperFlags drops sudo's or env's own leading flags (and their values) from toks.
func skipWrapperFlags(wrapper string, toks []string) []string {
	for (wrapper == "sudo" || wrapper == "env") && len(toks) > 0 && strings.HasPrefix(toks[0], "-") {
		if wrapper == "env" && toks[0] == "--" {
			return toks[1:]
		}
		valueFlags := sudoValueFlags
		if wrapper == "env" {
			valueFlags = envValueFlags
		}
		if valueFlags[toks[0]] {
			toks = toks[1:]
		}
		toks = toks[1:]
	}
	return toks
}

// commitShellSegments splits cmd on &&, ||, ;, |, newline — quoted delimiters stay inside their segment.
// A backslash escapes the next char literally outside single quotes (so `\"` never closes a double
// quote); a backslash-newline outside single quotes is a line continuation and vanishes entirely.
func commitShellSegments(cmd string) []string {
	var s segSplitter
	for i := 0; i < len(cmd); i++ {
		i = s.step(cmd, i)
	}
	return append(s.out, s.b.String())
}

// segSplitter is commitShellSegments' scanner state: finished segments, the one being built and the open quote.
type segSplitter struct {
	out   []string
	b     strings.Builder
	quote byte
}

// quoted copies c into the segment and closes the quote when c matches it.
func (s *segSplitter) quoted(c byte) {
	s.b.WriteByte(c)
	if c == s.quote {
		s.quote = 0
	}
}

// escaped copies a backslash and its target; a backslash-newline is a line continuation and vanishes.
func (s *segSplitter) escaped(next byte) {
	if next != '\n' {
		s.b.WriteByte('\\')
		s.b.WriteByte(next)
	}
}

// step consumes cmd[i] and returns the index of the last byte it used.
func (s *segSplitter) step(cmd string, i int) int {
	c := cmd[i]
	if s.quote == '\'' {
		s.quoted(c)
		return i
	}
	if c == '\\' && i+1 < len(cmd) {
		s.escaped(cmd[i+1])
		return i + 1
	}
	if s.quote == '"' {
		s.quoted(c)
		return i
	}
	switch c {
	case '\'', '"':
		s.quote = c
		s.b.WriteByte(c)
	case ';', '|', '&', '\n':
		s.out = append(s.out, s.b.String())
		s.b.Reset()
		if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
			i++
		}
	default:
		s.b.WriteByte(c)
	}
	return i
}

// dqEscapable are the chars a backslash actually escapes inside double quotes (bash rule); before any
// other char the backslash stays a literal character of the token (`"comm\it"` keeps its backslash).
const dqEscapable = "$`\"\\\n"

// commitShellTokens splits one segment into words on whitespace; a quoted run collapses to ONE token,
// so `grep 'git commit'` never tokenizes into separate "git"/"commit" words that could match. Outside
// quotes a backslash escapes the next char literally; inside double quotes only dqEscapable collapses.
func commitShellTokens(seg string) []string {
	var s tokScanner
	for i := 0; i < len(seg); i++ {
		i = s.step(seg, i)
	}
	s.flush()
	return s.out
}

// tokScanner is commitShellTokens' scanner state: finished words, the one being built and the open quote.
type tokScanner struct {
	out     []string
	b       strings.Builder
	quote   byte
	started bool
}

// quoted copies c into the word and closes the quote when c matches it.
func (s *tokScanner) quoted(c byte) {
	if c == s.quote {
		s.quote = 0
		return
	}
	s.b.WriteByte(c)
}

// flush ends the word being built, if any.
func (s *tokScanner) flush() {
	if s.started {
		s.out = append(s.out, s.b.String())
		s.b.Reset()
		s.started = false
	}
}

// escapes reports whether the backslash at seg[i] escapes the next char (inside double quotes only dqEscapable ones).
func (s *tokScanner) escapes(seg string, i int) bool {
	return i+1 < len(seg) && (s.quote != '"' || strings.IndexByte(dqEscapable, seg[i+1]) >= 0)
}

// step consumes seg[i] and returns the index of the last byte it used.
func (s *tokScanner) step(seg string, i int) int {
	c := seg[i]
	if s.quote == '\'' {
		s.quoted(c)
		return i
	}
	if c == '\\' && s.escapes(seg, i) {
		s.started = true
		s.b.WriteByte(seg[i+1])
		return i + 1
	}
	if s.quote == '"' {
		s.quoted(c)
		return i
	}
	s.unquoted(c)
	return i
}

// unquoted handles c outside any quote: it opens a quote, ends the word on whitespace, or extends the word.
func (s *tokScanner) unquoted(c byte) {
	switch c {
	case '\'', '"':
		s.quote = c
		s.started = true
	case ' ', '\t':
		s.flush()
	default:
		s.started = true
		s.b.WriteByte(c)
	}
}
