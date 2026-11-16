package shell

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var conventionalSubject = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(feat|fix|docs|style|refactor|perf|test|chore|ci)(\([^)]+\))?!?: .+`)
})

var heredocStart = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)
})

// reuseMessageFlags make git take the message from elsewhere (-C/-c are short, so matched separately).
var reuseMessageFlags = []string{"--no-edit", "--amend", "--reuse-message", "--fixup", "--squash"}

// CommitMessageProblem says why a `git commit` message breaks Conventional Commits, or "" when fine or unreadable.
func CommitMessageProblem(cmd, cwd string) string {
	_, subs := ExtractSubstitutions(cmd)
	problem := ""
	EachGit(cmd, cwd, func(target, verb string, args []string) bool {
		if verb != "commit" {
			return false
		}
		if msg, ok := commitMessage(cmd, subs, target, args); ok {
			problem = messageProblem(msg)
		}
		return problem != ""
	})
	return problem
}

// commitMessage joins the -m/-F paragraphs of one commit; ok is false when there is none or it is not static.
func commitMessage(cmd string, subs []string, target string, args []string) (string, bool) {
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
			parts = append(parts, val)
		case 'F':
			body, ok := fileMessage(cmd, target, val)
			if !ok {
				return "", false
			}
			parts = append(parts, body)
		}
	}
	for i, p := range parts {
		if !strings.Contains(p, subMask) {
			continue
		}
		body, ok := substitutedHeredoc(subs)
		if !ok {
			return "", false // a message built by a command we cannot read
		}
		parts[i] = strings.ReplaceAll(p, subMask, body)
	}
	return strings.Join(parts, "\n\n"), len(parts) > 0
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

// fileMessage reads -F: `-` is the heredoc on stdin, anything else a file under the commit's tree.
func fileMessage(cmd, target, name string) (string, bool) {
	if name == "-" {
		return heredocBody(cmd)
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(target, name)
	}
	b, err := os.ReadFile(name)
	return string(b), err == nil
}

// substitutedHeredoc is the body of the first `$(cat <<EOF ...)` substitution, the usual way a commit message is built.
func substitutedHeredoc(subs []string) (string, bool) {
	for _, s := range subs {
		if body, ok := heredocBody(s); ok {
			return body, true
		}
	}
	return "", false
}

// heredocBody is the text between the first heredoc operator's line and its closing delimiter line.
func heredocBody(text string) (string, bool) {
	m := heredocStart().FindStringSubmatchIndex(text)
	if m == nil {
		return "", false
	}
	delim := text[m[4]:m[5]]
	rest := text[m[1]:]
	_, rest, _ = strings.Cut(rest, "\n")
	var body []string
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.TrimSpace(line) == delim {
			return strings.Join(body, "\n"), true
		}
		body = append(body, line)
	}
	return "", false
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
	paths := false
	for _, a := range args {
		switch {
		case paths:
		case a == "--":
			paths = true
			continue
		case a == "--all":
			return a
		case strings.HasPrefix(a, "--"):
			continue
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.Contains(a, "A") {
				return a
			}
			continue
		}
		if a == ":/" || a == "." {
			return a
		}
		p := a
		if !filepath.IsAbs(p) {
			p = filepath.Join(target, p)
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return a
		}
	}
	return ""
}

// PushAfterGate reports a `; git push` (or newline) straight after a lint/test/build/audit command: use `&&`.
func PushAfterGate(cmd string) bool {
	segs, seps := SegmentsSep(cmd)
	for i := 0; i+1 < len(segs); i++ {
		if (seps[i] == ";" || seps[i] == "\n") && gateCommand(commandWords(segs[i])) && gitPush(commandWords(segs[i+1])) {
			return true
		}
	}
	return false
}

func gitPush(toks []string) bool {
	toks = builtinPrefixDropped(toks)
	if len(toks) == 0 || toks[0] != "git" {
		return false
	}
	_, verb, _ := GitInvocation(toks[1:], "")
	return verb == "push"
}

var gateWords = map[string]bool{"lint": true, "test": true, "build": true, "audit": true}

// gateCommand is a build-tool run named for lint, test, build or audit.
func gateCommand(toks []string) bool {
	toks = builtinPrefixDropped(toks)
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

// ChainPushAfterGate turns `;`/newline between a gate and `git push` into `&&`; ok is false near heredocs or grouping.
func ChainPushAfterGate(cmd string) (fixed, gate string, ok bool) {
	if strings.Contains(cmd, "<<") {
		return "", "", false
	}
	segs, seps := SegmentsSep(cmd)
	var b strings.Builder
	b.WriteString(segs[0])
	for i := 0; i+1 < len(segs); i++ {
		if (seps[i] == ";" || seps[i] == "\n") && gateCommand(commandWords(segs[i])) && gitPush(commandWords(segs[i+1])) {
			if strings.ContainsAny(segs[i]+segs[i+1], "(){}`\n") {
				return "", "", false
			}
			if gate == "" {
				gate = strings.TrimSpace(segs[i])
			}
			chained := strings.TrimRight(b.String(), " \t") + " && " + strings.TrimLeft(segs[i+1], " \t")
			b.Reset()
			b.WriteString(chained)
			continue
		}
		b.WriteString(seps[i] + segs[i+1])
	}
	return b.String(), gate, gate != ""
}

var footerLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\W*(co-authored-by:|generated (with|by))`)
})

var footerArg = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\s+(?:-m|--message)\s+("[^"]*"|'[^']*')`)
})

// StripFooters drops footer-only lines and lone -m "<footer>" args; ok is false if text is mixed or quotes unbalance.
func StripFooters(cmd string) (string, bool) {
	fixed := footerArg().ReplaceAllStringFunc(cmd, func(arg string) string {
		for l := range strings.SplitSeq(footerArg().FindStringSubmatch(arg)[1], "\n") {
			if l = strings.Trim(strings.TrimSpace(l), `"'`); l != "" && !footerLine().MatchString(l) {
				return arg
			}
		}
		if hasFooter(arg) {
			return ""
		}
		return arg
	})
	lines := strings.Split(fixed, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !hasFooter(l) {
			kept = append(kept, l)
			continue
		}
		t := strings.TrimSpace(l)
		if !footerLine().MatchString(t) {
			return cmd, false // the footer shares its line with other command text
		}
		if q := t[len(t)-1]; (q == '"' || q == '\'') && strings.Count(t, string(q)) == 1 {
			kept = append(kept, string(q)) // the footer ended the message: keep its closing quote
		}
	}
	fixed = strings.Join(kept, "\n")
	balanced := strings.Count(fixed, `"`)%2 == strings.Count(cmd, `"`)%2 && strings.Count(fixed, "'")%2 == strings.Count(cmd, "'")%2
	return fixed, fixed != cmd && balanced && !hasFooter(fixed)
}
