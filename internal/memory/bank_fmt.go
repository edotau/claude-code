package memory

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/mdlint"
)

var (
	bankFmtHeading = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^#{1,6}\s`)
	})
	bankFmtComment = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^<!--.*-->$`)
	})
	bankFmtBullet = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^(\s*)([-*+] )(.*)$`)
	})
	bankFmtOrdered = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^(\s*)(\d+\. )(.*)$`)
	})
	bankFmtSpan = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\[[^\]\n]*\]\([^)\n]*\)|<!--.*?-->`)
	})
)

// bankFmtTargetDirs are the flat subs whose *.md files get formatted; top-level files come from bankFiles.
var bankFmtTargetDirs = []string{"archive", "handoffs", "reports"}

// bankFmtTargets lists every markdown file formatBank formats: the typed files plus archive, handoffs and reports.
func bankFmtTargets() ([]string, error) {
	var files []string
	for _, name := range bankFiles {
		p := Path(name)
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
		files = append(files, ownedFiles(sub)...)
	}
	return files, nil
}

// formatBank wraps every target file; check=true writes nothing and only reports what would change.
func formatBank(check bool) (changed []string, err error) {
	if check {
		return formatBankLocked(true)
	}
	err = withBankLock(BankLockWait, func() error {
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
	if !check && len(changed) > 0 { // commit what was written even when a later file failed, so a retry is not blind to it
		// Credential-shaped files must not block the commit.
		scope := StoreRepo().dropCredentialShaped(storeRels(changed), nil)
		if cerr := commitStore("fmt: wrap bank lines", scope...); cerr != nil {
			return changed, errors.Join(err, cerr)
		}
	}
	return changed, err
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
		if !check {
			ok, err := writeIfStill(f, data, true, []byte(next), 0o600)
			if err != nil {
				return changed, err
			}
			if !ok {
				continue // an outside edit stays; it is not reported as wrapped
			}
		}
		changed = append(changed, f)
	}
	return changed, nil
}

// formatBankMarkdown wraps over-width list/paragraph lines idempotently (space runs collapse); fences, tables pass.
func formatBankMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var scan bankScan
	for i, line := range lines {
		if scan.special(i, line) {
			out = append(out, line)
			continue
		}
		ending := ""
		if content, ok := strings.CutSuffix(line, "\r"); ok {
			line, ending = content, "\r"
		}
		for _, wrapped := range wrapBankLine(line) {
			out = append(out, wrapped+ending)
		}
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
	case s.fence != "" || nextFence("", trimmed) != "":
		s.fence = nextFence(s.fence, trimmed)
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
	return trimmed == "" || strings.HasPrefix(trimmed, "|") || bankFmtHeading().MatchString(trimmed) ||
		bankFmtComment().MatchString(trimmed)
}

// groupLogicalLines groups physical lines into logical lines, mirroring formatBankMarkdown's state machine exactly.
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

// lineGrouper is groupLogicalLines' state machine; contCol is the column a continuation must match (-1 = none).
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
	m := listItem(line)
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

// logicalLineCount is len(groupLogicalLines(text)), the unit every bank line cap counts in.
func logicalLineCount(text string) int {
	return len(groupLogicalLines(text))
}

// listItem is line's (indent, marker, text) submatches when it is a bullet or ordered list item, else nil.
func listItem(line string) []string {
	if m := bankFmtBullet().FindStringSubmatch(line); m != nil {
		return m
	}
	return bankFmtOrdered().FindStringSubmatch(line)
}

// wrapBankLine wraps one line at word boundaries, never inside a code span, link or HTML comment.
func wrapBankLine(line string) []string {
	if utf8.RuneCountInString(line) <= mdlint.Width {
		return []string{line}
	}
	hardBreak := strings.HasSuffix(line, "  ")
	text, id := trailingMarker(strings.TrimRight(line, " \t"))
	var indent, marker, rest string
	contentCol := 0
	if m := listItem(text); m != nil {
		indent, marker, rest = m[1], m[2], m[3]
		contentCol = len(indent) + len(marker)
	} else { // a plain paragraph, or an already-wrapped continuation line: keep its own leading indent
		lead := len(text) - len(strings.TrimLeft(text, " "))
		indent, rest = text[:lead], text[lead:]
		contentCol = lead
	}
	tokens := bankFmtTokenize(rest)
	if len(tokens) == 0 {
		return []string{line}
	}
	if hardBreak {
		tokens[len(tokens)-1] += "  "
	}
	contIndent := strings.Repeat(" ", contentCol)
	reserve := 0
	if id != "" {
		reserve = 1 + utf8.RuneCountInString(id)
	}
	out := bankFmtPack(tokens, indent+marker, contIndent, reserve)
	if id != "" {
		if len(out) == 1 && hardBreak {
			out[0] = strings.TrimSuffix(out[0], "  ") + " " + id + "  "
		} else {
			out[0] += " " + id
		}
	}
	return out
}

// trailingMarker extracts the id for line 0's suffix, even when the input has text after its marker.
func trailingMarker(line string) (text, id string) {
	r, lo, hi, ok := marker(line)
	if !ok || r.Derived || strings.TrimSpace(line[:lo]+line[hi:]) == "" {
		return line, ""
	}
	return strings.TrimRight(line[:lo]+line[hi:], " "), line[lo:hi]
}

// bankFmtTokenize splits s on bare spaces; spaces inside a code span, link or HTML comment stay glued.
func bankFmtTokenize(s string) []string {
	protected := make([]bool, len(s))
	for _, sp := range append(codeSpans(s), bankFmtSpan().FindAllStringIndex(s, -1)...) {
		for i := sp[0]; i < sp[1]; i++ {
			protected[i] = true
		}
	}
	var tokens []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' && !protected[i] {
			if start < i {
				tokens = append(tokens, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		tokens = append(tokens, s[start:])
	}
	return tokens
}

// bankFmtPack greedily fills lines ≤ mdlint.Width runes, the first reserve runes less; an over-width token stays whole.
func bankFmtPack(tokens []string, firstPrefix, contIndent string, reserve int) []string {
	var lines []string
	prefix, row := firstPrefix, []string(nil)
	width, limit := utf8.RuneCountInString(prefix), mdlint.Width-reserve
	for _, t := range tokens {
		nextWidth := width + 1 + utf8.RuneCountInString(t)
		if len(row) == 0 {
			nextWidth = utf8.RuneCountInString(prefix + t)
		}
		if len(row) == 0 || nextWidth <= limit {
			width = nextWidth
			row = append(row, t)
			continue
		}
		keep := len(row)
		if bankFmtMarker(contIndent, t) { // a marker must not open a line: carry the trailing marker run and a word
			keep = len(row) - 1
			for keep > 0 && bankFmtMarker(contIndent, row[keep]) {
				keep--
			}
		}
		if keep == 0 { // nothing precedes the run: keep it on this line, over width
			width = nextWidth
			row = append(row, t)
			continue
		}
		lines = append(lines, prefix+strings.Join(row[:keep], " "))
		prefix, row, limit = contIndent, append(slices.Clone(row[keep:]), t), mdlint.Width
		width = utf8.RuneCountInString(prefix + strings.Join(row, " "))
	}
	return append(lines, prefix+strings.Join(row, " "))
}

// bankFmtMarker reports whether tok, opening a continuation line, would read as a list marker, heading or fence.
func bankFmtMarker(contIndent, tok string) bool {
	return listItem(contIndent+tok+" x") != nil || bankFmtHeading().MatchString(tok+" ") || nextFence("", tok) != ""
}
