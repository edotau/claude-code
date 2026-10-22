package memory

// Wraps long lines in the bank markdown the harness writes (never index.md, never MEMORY.md): the six
// typed files, archive/*.md, handoffs/*.md and reports/*.md, across every repo store and home/.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
)

// BankFmtMaxLine is the wrap width — mirrors .mdsmith.yml's line-length.max for the repo's own markdown.
const BankFmtMaxLine = 120

var (
	bankFmtFence   = regexp.MustCompile(`^\s*(` + "```" + `|~~~)`)
	bankFmtHeading = regexp.MustCompile(`^#{1,6}\s`)
	bankFmtComment = regexp.MustCompile(`^<!--.*-->$`)
	bankFmtBullet  = regexp.MustCompile(`^(\s*)([-*] )(.*)$`)
	bankFmtOrdered = regexp.MustCompile(`^(\s*)(\d+\. )(.*)$`)
	bankFmtSpan    = regexp.MustCompile("`[^`\n]*`" + `|\[[^\]\n]*\]\([^)\n]*\)` + `|<!--.*?-->`)
)

// bankFmtTargetDirs is the glob paths inside one bank dir worth formatting; bare "*.md" is handled separately.
var bankFmtTargetDirs = []string{"archive", "handoffs", "reports"}

// BankFmtTargets lists every markdown file FormatBank formats, across every repo store and home/.
func BankFmtTargets() ([]string, error) {
	var dirs []string
	dirs = append(dirs, HomeBankDir())
	entries, err := RepoDirs()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		dirs = append(dirs, e.Dir)
	}
	var files []string
	for _, dir := range dirs {
		for _, name := range bankFiles {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
			}
		}
		for _, sub := range bankFmtTargetDirs {
			matches, _ := filepath.Glob(filepath.Join(dir, sub, "*.md"))
			files = append(files, matches...)
		}
	}
	return files, nil
}

// FormatBank wraps every target file; check=true writes nothing and only reports. changed lists what
// would (or did) change; commitStore settles the result the same way `memory index` does.
func FormatBank(check bool) (changed []string, err error) {
	files, err := BankFmtTargets()
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		data, rerr := os.ReadFile(f)
		if rerr != nil {
			return changed, rerr
		}
		next := FormatBankMarkdown(string(data))
		if next == string(data) {
			continue
		}
		changed = append(changed, f)
		if check {
			continue
		}
		if err := paths.AtomicWrite(f, []byte(next), 0o600); err != nil {
			return changed, err
		}
	}
	if !check && len(changed) > 0 {
		if err := commitStore("fmt: wrap bank lines"); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// FormatBankMarkdown wraps text's over-width list/paragraph lines; fenced code, tables, headings, front
// matter and comment-only lines pass through untouched. Lossless: joining a wrapped line's continuations
// (after trimming their indent) with single spaces reproduces the original; idempotent: a line already
// ≤ BankFmtMaxLine never changes, so a second pass is a no-op.
func FormatBankMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence, inFrontMatter, inRules := false, false, false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case i == 0 && trimmed == "---":
			inFrontMatter = true
			out = append(out, line)
			continue
		case inFrontMatter:
			out = append(out, line)
			if trimmed == "---" {
				inFrontMatter = false
			}
			continue
		case bankFmtFence.MatchString(line):
			inFence = !inFence
			out = append(out, line)
			continue
		case inFence:
			out = append(out, line)
			continue
		case strings.Contains(line, "RULES:BEGIN"):
			inRules = true
			out = append(out, line)
			continue
		case inRules:
			out = append(out, line)
			if strings.Contains(line, "RULES:END") {
				inRules = false
			}
			continue
		case trimmed == "" || strings.HasPrefix(trimmed, "|") || bankFmtHeading.MatchString(trimmed) ||
			bankFmtComment.MatchString(trimmed):
			out = append(out, line)
			continue
		}
		out = append(out, wrapBankLine(line)...)
	}
	return strings.Join(out, "\n")
}

// wrapBankLine wraps one list-item or paragraph line at word boundaries, never inside an inline code
// span, a link, or an HTML comment; a line already within the cap passes through unchanged.
func wrapBankLine(line string) []string {
	if utf8.RuneCountInString(line) <= BankFmtMaxLine {
		return []string{line}
	}
	indent, marker, rest := "", "", line
	contentCol := 0
	switch {
	case bankFmtBullet.MatchString(line):
		m := bankFmtBullet.FindStringSubmatch(line)
		indent, marker, rest = m[1], m[2], m[3]
		contentCol = len(indent) + len(marker)
	case bankFmtOrdered.MatchString(line):
		m := bankFmtOrdered.FindStringSubmatch(line)
		indent, marker, rest = m[1], m[2], m[3]
		contentCol = len(indent) + len(marker)
	default: // a plain paragraph, or an already-wrapped continuation line: keep its own leading indent
		lead := len(line) - len(strings.TrimLeft(line, " "))
		indent, rest = line[:lead], line[lead:]
		contentCol = lead
	}
	tokens := bankFmtTokenize(rest)
	if len(tokens) == 0 {
		return []string{line}
	}
	contIndent := strings.Repeat(" ", contentCol)
	firstPrefix := indent + marker
	return bankFmtPack(tokens, firstPrefix, contIndent)
}

// bankFmtTokenize splits s into words on a bare space, except a space inside a code span, link or
// HTML comment: those stay glued to whatever touches them, so a token like "(`cmd`;" never gains a
// space the original line didn't have.
func bankFmtTokenize(s string) []string {
	protected := make([]bool, len(s))
	for _, sp := range bankFmtSpan.FindAllStringIndex(s, -1) {
		for i := sp[0]; i < sp[1]; i++ {
			protected[i] = true
		}
	}
	var tokens []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' && !protected[i] {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(s[i])
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// bankFmtPack greedily fills lines ≤ BankFmtMaxLine runes; a single over-width token is kept whole.
func bankFmtPack(tokens []string, firstPrefix, contIndent string) []string {
	var lines []string
	cur := firstPrefix
	onLine := 0
	for _, t := range tokens {
		sep := ""
		if onLine > 0 {
			sep = " "
		}
		candidate := cur + sep + t
		if onLine > 0 && utf8.RuneCountInString(candidate) > BankFmtMaxLine {
			lines = append(lines, cur)
			cur = contIndent + t
			onLine = 1
			continue
		}
		cur = candidate
		onLine++
	}
	return append(lines, cur)
}
