package source

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// lineSpans are inclusive 1-based line ranges of the added or changed lines of one file.
type lineSpans [][2]int

func (s lineSpans) touches(lo, hi int) bool {
	for _, r := range s {
		if r[0] <= hi && lo <= r[1] {
			return true
		}
	}
	return false
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseHunkSpans reads the new-side ranges of `git diff -U0` output; a pure deletion adds none.
func parseHunkSpans(diff string) lineSpans {
	var spans lineSpans
	for l := range strings.SplitSeq(diff, "\n") {
		m := hunkHeader.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		n := 1
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		if n > 0 {
			spans = append(spans, [2]int{start, start + n - 1})
		}
	}
	return spans
}

// renamedFrom is the path a file had at base when git pairs it as a rename ("" otherwise).
func renamedFrom(path, base string) string {
	out, err := GitInherit(".", "diff", "-M", "--relative", "--name-status", base)
	if err != nil {
		return ""
	}
	for l := range strings.SplitSeq(out, "\n") {
		if f := strings.Split(l, "\t"); len(f) == 3 && f[0][0] == 'R' && f[2] == path {
			return f[1]
		}
	}
	return ""
}

// changedSpans is the added or changed lines of path against base (old: its path there); untracked is all changed.
func changedSpans(path, old, base string) (lineSpans, error) {
	if l, err := gitLines(".", "ls-files", "--others", "--exclude-standard", "--", path); err == nil && len(l) > 0 {
		return lineSpans{{1, 1 << 30}}, nil
	}
	diff, err := GitInherit(".", "diff", "-M", "-U0", base, "--", path, old)
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", path, err)
	}
	return parseHunkSpans(diff), nil
}

// goFuncEnds maps each declared function's start line to its end line.
func goFuncEnds(path, text string) (map[int]int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGoSyntax, err)
	}
	ends := map[int]int{}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			ends[fset.Position(fn.Pos()).Line] = fset.Position(fn.End()).Line
		}
	}
	return ends, nil
}

// wasStacked reports whether base has a block with b's first or last prose line and at least as many prose lines.
func wasStacked(b commentBlock, base []commentBlock) bool {
	prose := proseLines(b, "go")
	for _, o := range base {
		op := proseLines(o, "go")
		if len(op) >= len(prose) && len(prose) > 0 && (op[0] == prose[0] || op[len(op)-1] == prose[len(prose)-1]) {
			return true
		}
	}
	return false
}

// commentViolations are the stacked-block (made or grown by the change) and over-long comment smells on changed lines.
func commentViolations(text, base string, spans lineSpans) []string {
	var out []string
	baseBlocks := scanCommentBlocks(base, "go")
	for _, b := range scanCommentBlocks(text, "go") {
		if !spans.touches(b.start, b.start+len(b.lines)-1) {
			continue
		}
		stacked := wasStacked(b, baseBlocks)
		for _, s := range blockSmells(b, "go") {
			if s.Type == "long_comment" && stacked {
				continue
			}
			line := b.start
			if s.Type == "long_comment_line" {
				line, _ = strconv.Atoi(strings.TrimPrefix(s.Location, "line "))
				if !spans.touches(line, line) {
					continue
				}
			}
			out = append(out, fmt.Sprintf("%d: %s", line, s.Message))
		}
	}
	return out
}

// sizeViolations: length/nesting of functions with a changed line, plus file-level findings new vs base.
func sizeViolations(path, text, base string, spans lineSpans) ([]string, error) {
	th := complexityThresholds["medium"]
	res, err := complexityContent(path, text, "go", th)
	if err != nil {
		return nil, err
	}
	ends, err := goFuncEnds(path, text)
	if err != nil {
		return nil, err
	}
	old := map[string]bool{}
	if baseRes, berr := complexityContent(path, base, "go", th); berr == nil && base != "" {
		for _, f := range baseRes.Findings {
			old[f.Rule] = true
		}
	}
	var out []string
	for _, f := range res.Findings {
		switch {
		case f.Line > 0 && (f.Rule == "function-length" || f.Rule == "nesting-depth"):
			if spans.touches(f.Line, ends[f.Line]) {
				out = append(out, fmt.Sprintf("%d: %s", f.Line, f.Message))
			}
		case f.Line == 0 && !old[f.Rule]:
			out = append(out, "1: "+f.Message)
		}
	}
	return out, nil
}

// newGoViolations lists the rules path breaks on lines changed since base, as "path:line: message".
func newGoViolations(path, base string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	old := renamedFrom(path, base)
	if old == "" {
		old = path
	}
	spans, err := changedSpans(path, old, base)
	if err != nil {
		return nil, err
	}
	baseText, _ := GitInherit(".", "show", base+":./"+old)
	text := string(data)
	out, err := sizeViolations(path, text, baseText, spans)
	if err != nil {
		return nil, err
	}
	out = append(out, commentViolations(text, baseText, spans)...)
	sort.Strings(out)
	for i, v := range out {
		out[i] = path + ":" + v
	}
	return out, nil
}

// failOnNewViolations prints new comment and size violations in the changed Go files; exit 1 when any.
func failOnNewViolations(files []string, base string, stdout, stderr io.Writer) int {
	var found []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		v, err := newGoViolations(f, base)
		if err != nil {
			fmt.Fprintf(stderr, "review simplify: %s: %v\n", f, err)
			return 1
		}
		found = append(found, v...)
	}
	for _, v := range found {
		fmt.Fprintln(stdout, v)
	}
	if len(found) > 0 {
		fmt.Fprintf(stdout, "%d new violation(s) vs %s (rules/standards/quality.md)\n", len(found), base)
		return 1
	}
	fmt.Fprintf(stdout, "no new comment or size violations vs %s\n", base)
	return 0
}
