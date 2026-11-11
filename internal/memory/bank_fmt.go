package memory

// Wraps long lines in the bank markdown the harness writes (never index.md, never MEMORY.md): the six
// typed files, archive/*.md, handoffs/*.md and reports/*.md, across every repo store and home/.

import (
	"errors"
	"io/fs"
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

// bankFmtTargets lists every markdown file formatBank formats, across every repo store and home/.
func bankFmtTargets() ([]string, error) {
	dirs := []string{HomeBankDir()}
	entries, err := RepoDirs()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		dirs = append(dirs, e.Dir)
	}
	var files []string
	for _, dir := range dirs {
		dirFiles, err := dirFmtTargets(dir)
		if err != nil {
			return nil, err
		}
		files = append(files, dirFiles...)
	}
	return files, nil
}

// dirFmtTargets lists one bank dir's typed files plus its archive/, handoffs/ and reports/ markdown.
func dirFmtTargets(dir string) ([]string, error) {
	var files []string
	for _, name := range bankFiles {
		p := filepath.Join(dir, name)
		_, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files = append(files, p)
	}
	for _, sub := range bankFmtTargetDirs {
		matches, err := mdFilesIn(filepath.Join(dir, sub))
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	return files, nil
}

// mdFilesIn lists dir's regular *.md files; a missing dir is empty, any other read error is returned.
func mdFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && e.Type().IsRegular() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// formatBank wraps every target file; check=true writes nothing and only reports. changed lists what
// would (or did) change; commitStore settles the result the same way `memory index` does.
func formatBank(check bool) (changed []string, err error) {
	if check {
		return formatBankLocked(true)
	}
	err = WithBankLock(BankLockWait, func() error {
		changed, err = formatBankLocked(false)
		return err
	})
	return changed, err
}

// formatBankLocked is formatBank's body; a writing caller holds the bank lock.
func formatBankLocked(check bool) (changed []string, err error) {
	files, err := bankFmtTargets()
	if err != nil {
		return nil, err
	}
	changed, err = formatFiles(files, check)
	if err != nil {
		return changed, err
	}
	if !check && len(changed) > 0 {
		scope := StoreRepo().dropCredentialShaped(storeRels(changed), nil) // fmt spans every repo's bank: skip, never wedge
		if err := commitStore("fmt: wrap bank lines", scope...); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// formatFiles wraps each file in place (check=true only reports) and returns the ones that changed.
func formatFiles(files []string, check bool) (changed []string, err error) {
	for _, f := range files {
		data, rerr := os.ReadFile(f)
		if rerr != nil {
			return changed, rerr
		}
		next := formatBankMarkdown(string(data))
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
	return changed, nil
}

// formatBankMarkdown wraps text's over-width list/paragraph lines; fenced code, tables, headings, front
// matter and comment-only lines pass through untouched. Words rejoin with single spaces, so runs of spaces
// collapse; idempotent: a line already ≤ BankFmtMaxLine never changes, so a second pass is a no-op.
func formatBankMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var scan bankScan
	for i, line := range lines {
		if scan.special(i, line) {
			out = append(out, line)
			continue
		}
		out = append(out, wrapBankLine(line)...)
	}
	return strings.Join(out, "\n")
}

// bankScan tracks which multi-line region (front matter, fence run, rules block) the next line sits in.
type bankScan struct {
	fence         string
	inFrontMatter bool
	inRules       bool
}

// special handles fence/front-matter/rules/heading/comment/blank/table lines; false means a content line.
func (s *bankScan) special(i int, line string) bool {
	trimmed := strings.TrimSpace(line)
	switch {
	case i == 0 && trimmed == "---":
		s.inFrontMatter = true
	case s.inFrontMatter:
		s.inFrontMatter = trimmed != "---"
	case s.fence != "":
		if fenceCloses(trimmed, s.fence) {
			s.fence = ""
		}
	case bankFmtFence.MatchString(line):
		s.fence = fenceRun(trimmed)
	case strings.Contains(line, "RULES:BEGIN"):
		s.inRules = !strings.Contains(line, "RULES:END")
	case s.inRules:
		s.inRules = !strings.Contains(line, "RULES:END")
	default:
		return passThroughLine(trimmed)
	}
	return true
}

// passThroughLine reports whether a trimmed line is blank, a table row, a heading or a comment-only line.
func passThroughLine(trimmed string) bool {
	return trimmed == "" || strings.HasPrefix(trimmed, "|") || bankFmtHeading.MatchString(trimmed) ||
		bankFmtComment.MatchString(trimmed)
}

// groupLogicalLines splits text's physical lines into logical-line groups: a continuation line that
// wrapBankLine would have produced (leading spaces matching its predecessor's content column) joins
// that predecessor's group; every fence/front-matter/heading/comment/blank/table line, and every new
// bullet/ordered/paragraph line, starts its own group. Mirrors formatBankMarkdown's state machine line
// for line so the two can never drift on what counts as "one line".
func groupLogicalLines(text string) [][]string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	g := &lineGrouper{contCol: -1}
	for i, line := range strings.Split(text, "\n") {
		if !g.special(i, line) {
			g.content(line)
		}
	}
	return g.groups
}

// lineGrouper is groupLogicalLines' state machine; contCol is the content column an indent-only
// continuation must match (-1 = none open).
type lineGrouper struct {
	groups  [][]string
	scan    bankScan
	contCol int
}

func (g *lineGrouper) open(line string) { g.groups = append(g.groups, []string{line}) }

// special opens a group for a line formatBankMarkdown passes through untouched; false means a content line.
func (g *lineGrouper) special(i int, line string) bool {
	if !g.scan.special(i, line) {
		return false
	}
	g.contCol = -1
	g.open(line)
	return true
}

// content groups a bullet, ordered or paragraph line, joining it to the open logical line when it continues one.
func (g *lineGrouper) content(line string) {
	lead := len(line) - len(strings.TrimLeft(line, " "))
	m := bankFmtBullet.FindStringSubmatch(line)
	if m == nil {
		m = bankFmtOrdered.FindStringSubmatch(line)
	}
	switch {
	case m != nil:
		g.contCol = len(m[1]) + len(m[2])
		g.open(line)
	case g.contCol >= 0 && lead == g.contCol:
		g.groups[len(g.groups)-1] = append(g.groups[len(g.groups)-1], line) // continuation of the open logical line
	default:
		g.contCol = lead
		g.open(line)
	}
}

// logicalLineCount is len(groupLogicalLines(text)) — the cap unit every bank line cap counts in: a
// wrapped bullet or paragraph counts once, matching the pre-wrap line count exactly.
func logicalLineCount(text string) int {
	return len(groupLogicalLines(text))
}

// wrapBankLine wraps one list-item or paragraph line at word boundaries, never inside an inline code
// span, a link, or an HTML comment; a line already within the cap passes through unchanged.
func wrapBankLine(line string) []string {
	if utf8.RuneCountInString(line) <= BankFmtMaxLine {
		return []string{line}
	}
	var indent, marker, rest string
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
	out := bankFmtPack(tokens, firstPrefix, contIndent)
	if strings.HasSuffix(line, "  ") { // a trailing double space is a Markdown hard break: keep it on the last line
		out[len(out)-1] += "  "
	}
	return out
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
