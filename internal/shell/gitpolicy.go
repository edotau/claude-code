package shell

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var conventionalSubject = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(feat|fix|docs|style|refactor|perf|test|chore|ci)(\([^)]+\))?!?: .+`)
})

var heredocStart = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`<<(-?)\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)
})

// subMark is a numbered substitution mask: \x00 N \x01.
var subMark = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("\x00(\\d+)\x01") })

// dynamicValue is a message word carrying a shell expansion whose value is unknown here.
var dynamicValue = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\$[A-Za-z_{]`) })

// reuseMessageFlags make git take the message from elsewhere (-C/-c are short, so matched separately).
var reuseMessageFlags = []string{"--no-edit", "--amend", "--reuse-message", "--fixup", "--squash"}

// CommitMessageProblem says why a `git commit` message breaks Conventional Commits, or "" when fine or unreadable.
func CommitMessageProblem(cmd, cwd string) string {
	masked, subs := extractSubstitutions(cmd, subMask)
	parts := strings.Split(masked, subMask)
	masked = parts[0]
	for i, p := range parts[1:] {
		masked += "\x00" + strconv.Itoa(i) + "\x01" + p
	}
	for _, s := range subs {
		if p := CommitMessageProblem(s, cwd); p != "" {
			return p
		}
	}
	problem, stdin := "", 0
	EachGit(masked, cwd, func(target, verb string, args []string) bool {
		if verb != "commit" {
			return false
		}
		if msg, ok := commitMessage(masked, subs, target, args, &stdin); ok {
			problem = messageProblem(msg)
		}
		return problem != ""
	})
	return problem
}

// commitMessage joins the -m/-F paragraphs of one commit; ok is false when there is none or it is not static.
func commitMessage(cmd string, subs []string, target string, args []string, stdin *int) (string, bool) {
	var parts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if isReuseFlag(a) {
			return "", false
		}
		val, kind, next := messageFlag(args, i)
		i = next
		switch kind {
		case 'm':
			if dynamicValue().MatchString(val) {
				return "", false
			}
			parts = append(parts, val)
		case 'F':
			body, ok := fileMessage(cmd, target, val, stdin)
			if !ok {
				return "", false
			}
			parts = append(parts, body)
		}
	}
	if !expandMasks(parts, subs) {
		return "", false // a message built by a command we cannot read
	}
	return strings.Join(parts, "\n\n"), len(parts) > 0
}

// expandMasks replaces each numbered mask in parts with its substitution's heredoc body; false if one has none.
func expandMasks(parts, subs []string) bool {
	ok := true
	for i, p := range parts {
		parts[i] = subMark().ReplaceAllStringFunc(p, func(m string) string {
			n, _ := strconv.Atoi(subMark().FindStringSubmatch(m)[1])
			body, found := heredocBody(subs[n])
			ok = ok && found
			return body
		})
	}
	return ok
}

func isReuseFlag(a string) bool {
	if strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-c") {
		return true
	}
	for _, f := range reuseMessageFlags {
		if strings.HasPrefix(a, f) {
			return true
		}
	}
	return false
}

// messageFlag reads a message flag at args[i]: its value, 'm' or 'F' (0 for any other word) and the last index used.
func messageFlag(args []string, i int) (val string, kind byte, last int) {
	a := args[i]
	next := func() (string, int) {
		if i+1 < len(args) {
			return args[i+1], i + 1
		}
		return "", i
	}
	switch {
	case a == "--message" || a == "--file":
		val, last = next()
		return val, map[string]byte{"--message": 'm', "--file": 'F'}[a], last
	case strings.HasPrefix(a, "--message="):
		return strings.TrimPrefix(a, "--message="), 'm', i
	case strings.HasPrefix(a, "--file="):
		return strings.TrimPrefix(a, "--file="), 'F', i
	case strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-") || len(a) < 2:
		return "", 0, i
	}
	// A short cluster: the first m or F takes the rest of the word, else the next word (-am msg, -mmsg).
	for j := 1; j < len(a); j++ {
		if a[j] != 'm' && a[j] != 'F' {
			continue
		}
		if j+1 < len(a) {
			return a[j+1:], a[j], i
		}
		val, last = next()
		return val, a[j], last
	}
	return "", 0, i
}

// fileMessage reads -F: `-` is the next commit heredoc on stdin, anything else a file under the commit's tree.
func fileMessage(cmd, target, name string, stdin *int) (string, bool) {
	if name == "-" {
		var commits []heredoc
		for _, h := range heredocs(cmd) {
			if strings.Contains(h.line, "commit") {
				commits = append(commits, h)
			}
		}
		*stdin++
		if *stdin > len(commits) {
			return "", false
		}
		return commits[*stdin-1].body, true
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(target, name)
	}
	b, err := os.ReadFile(name)
	return string(b), err == nil
}

// heredoc is one `<<DELIM` body and the line holding its operator.
type heredoc struct{ line, body string }

// heredocs lists text's heredocs in order; a body ends at its exact delimiter line (leading tabs for `<<-`).
func heredocs(text string) []heredoc {
	lines := strings.Split(text, "\n")
	var out []heredoc
	for i := 0; i < len(lines); i++ {
		last := i
		for _, m := range heredocStart().FindAllStringSubmatchIndex(lines[i], -1) {
			if m[0] > 0 && lines[i][m[0]-1] == '<' { // a <<< here-string has no body
				continue
			}
			dash, delim := m[3] > m[2], lines[i][m[6]:m[7]]
			end := -1
			for j := last + 1; j < len(lines) && end < 0; j++ {
				if l := lines[j]; l == delim || dash && strings.TrimLeft(l, "\t") == delim {
					end = j
				}
			}
			if end < 0 {
				return out
			}
			out = append(out, heredoc{lines[i], strings.Join(lines[last+1:end], "\n")})
			last = end
		}
		i = last
	}
	return out
}

// heredocBody is the body of text's first heredoc.
func heredocBody(text string) (string, bool) {
	hs := heredocs(text)
	if len(hs) == 0 {
		return "", false
	}
	return hs[0].body, true
}

// messageProblem applies rule 1 to a whole message: Conventional subject, at most 2 lines plus BREAKING CHANGE lines.
func messageProblem(msg string) string {
	var lines []string
	for l := range strings.SplitSeq(msg, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if !conventionalSubject().MatchString(lines[0]) {
		return "bad commit subject"
	}
	plain := 0
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "BREAKING CHANGE:") {
			plain++
		}
	}
	if plain > 1 {
		return "commit body over 2 lines"
	}
	return ""
}

// BroadStage is the `git add` operand that stages more than named files (-A, --all, ., :/, a directory), or "".
func BroadStage(cmd, cwd string) string {
	hit := ""
	EachGit(cmd, cwd, func(target, verb string, args []string) bool {
		if verb == "add" {
			hit = broadAddOperand(target, args)
		}
		return hit != ""
	})
	return hit
}

func broadAddOperand(target string, args []string) string {
	paths, update, operands := false, "", 0
	for _, a := range args {
		switch {
		case paths:
		case a == "--":
			paths = true
			continue
		case a == "--all" || strings.HasPrefix(a, "--pathspec-from-file"):
			return a
		case a == "--update":
			update = a
			continue
		case strings.HasPrefix(a, "--"):
			continue
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.Contains(a, "A") {
				return a
			}
			if strings.Contains(a, "u") {
				update = a
			}
			continue
		}
		operands++
		if broadOperand(target, a) {
			return a
		}
	}
	if operands == 0 {
		return update // -u with no named path stages every tracked change
	}
	return ""
}

// broadOperand is a pathspec that can match many files: ".", magic (":/", ":(top)"), a glob or a directory.
func broadOperand(target, a string) bool {
	if a == "." || strings.HasPrefix(a, ":") || strings.ContainsAny(a, "*?[") {
		return true
	}
	p := a
	if !filepath.IsAbs(p) {
		p = filepath.Join(target, p)
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// PushAfterGate reports a `; git push` (or newline) after a lint/test/build/audit command or pipeline: use `&&`.
func PushAfterGate(cmd string) bool {
	segs, seps := SegmentsSep(cmd)
	for i := 0; i+1 < len(segs); i++ {
		if seps[i] == ";" || seps[i] == "\n" {
			if found, _ := gateRun(segs, seps, i, i+1); found {
				return true
			}
		}
	}
	return false
}

// gateRun finds a gate in the pipeline ending at segs[i] when segs[push] is a git push; direct means segs[i] is it.
func gateRun(segs, seps []string, i, push int) (found, direct bool) {
	if !gitPush(commandWords(segs[push])) {
		return false, false
	}
	for j := i; j >= 0; j-- {
		if t := Tokens(segs[j]); len(t) > 0 && slices.Contains([]string{"if", "elif", "while", "until"}, t[0]) {
			return false, false // the condition guards its own body
		}
		if gateCommand(commandWords(segs[j])) {
			return true, j == i
		}
		if j == 0 || seps[j-1] != "|" {
			break
		}
	}
	return false, false
}

func gitPush(toks []string) bool {
	toks, _ = Unwrap(builtinPrefixDropped(toks), "")
	if len(toks) == 0 || filepath.Base(toks[0]) != "git" {
		return false
	}
	_, verb, _ := GitInvocation(toks[1:], "")
	return verb == "push"
}

var gateWords = map[string]bool{"lint": true, "test": true, "build": true, "audit": true}

// gateCommand is a build-tool run named for lint, test, build or audit.
func gateCommand(toks []string) bool {
	toks, _ = Unwrap(builtinPrefixDropped(toks), "")
	if len(toks) == 0 {
		return false
	}
	switch filepath.Base(toks[0]) {
	case "make", "just", "npm", "pnpm", "yarn", "bun":
		for _, w := range toks[1:] {
			if gateWords[w] {
				return true
			}
		}
	case "go":
		return len(toks) > 1 && (gateWords[toks[1]] || toks[1] == "vet")
	case "cargo":
		return len(toks) > 1 && (gateWords[toks[1]] || toks[1] == "clippy")
	case "pytest", "golangci-lint", "ruff", "eslint", "tsc", "mypy":
		return true
	}
	return false
}
