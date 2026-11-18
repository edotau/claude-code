package source

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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

var hunkHeader = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`) })

// parseHunkSpans reads the new-side ranges of `git diff -U0` output; a pure deletion adds none.
func parseHunkSpans(diff string) lineSpans {
	var spans lineSpans
	for l := range strings.SplitSeq(diff, "\n") {
		m := hunkHeader().FindStringSubmatch(l)
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

// gitBatch holds the git answers for a whole run, asked once instead of once per file.
type gitBatch struct {
	renames   map[string]string
	untracked map[string]bool
	spans     map[string]lineSpans // nil when the batched diff failed
	bases     map[string]string
}

// renameMap maps each path git pairs as a rename against base to the path it had there.
func renameMap(base string) map[string]string {
	out := map[string]string{}
	text, err := GitInherit(".", "diff", "-M", "--relative", "--name-status", base)
	if err != nil {
		return out
	}
	for l := range strings.SplitSeq(text, "\n") {
		if f := strings.Split(l, "\t"); len(f) == 3 && f[0][0] == 'R' {
			out[f[2]] = f[1]
		}
	}
	return out
}

// diffHeaderPath is the path of a `diff --git a/P b/P` header (same path both sides, as with --no-renames).
func diffHeaderPath(head string) string {
	if q, err := strconv.QuotedPrefix(head); err == nil {
		p, _ := strconv.Unquote(q)
		return strings.TrimPrefix(p, "a/")
	}
	if len(head) < 5 {
		return ""
	}
	return head[2 : 2+(len(head)-5)/2]
}

// batchSpans parses one repo-wide `git diff -U0` into each .go file's changed spans (nil when git fails).
func batchSpans(base string) map[string]lineSpans {
	text, err := GitInherit(".", "-c", "core.quotepath=false", "diff", "--no-renames", "-U0", "--relative",
		"--src-prefix=a/", "--dst-prefix=b/", base, "--", "*.go")
	if err != nil {
		return nil
	}
	out := map[string]lineSpans{}
	for _, sec := range strings.Split("\n"+text, "\ndiff --git ")[1:] {
		head, body, _ := strings.Cut(sec, "\n")
		out[diffHeaderPath(head)] = parseHunkSpans(body)
	}
	return out
}

// catBlobs reads each `<rev>:<path>` spec through one `git cat-file --batch`; missing blobs are absent.
func catBlobs(specs []string) map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(specs, "\n") + "\n")
	raw, err := cmd.Output()
	if err != nil {
		return out
	}
	r := bufio.NewReader(bytes.NewReader(raw))
	for _, spec := range specs {
		hdr, err := r.ReadString('\n')
		if err != nil {
			break
		}
		f := strings.Fields(hdr)
		if len(f) != 3 {
			continue
		}
		size, convErr := strconv.Atoi(f[2])
		if convErr != nil {
			continue
		}
		body := make([]byte, size+1)
		if _, err := io.ReadFull(r, body); err != nil {
			break
		}
		if f[1] == "blob" {
			out[spec] = strings.TrimRight(string(body), "\n")
		}
	}
	return out
}

func baseSpec(base, old string) string { return base + ":./" + old }

// loadGitBatch asks git once for the renames, untracked files, changed spans and base texts of files.
func loadGitBatch(files []string, base string) *gitBatch {
	g := &gitBatch{renames: renameMap(base), untracked: map[string]bool{}, spans: batchSpans(base)}
	if l, err := gitLines(".", "ls-files", "--others", "--exclude-standard", "--", "*.go"); err == nil {
		for _, f := range l {
			g.untracked[f] = true
		}
	}
	specs := make([]string, len(files))
	for i, f := range files {
		old := g.renames[f]
		if old == "" {
			old = f
		}
		specs[i] = baseSpec(base, old)
	}
	g.bases = catBlobs(specs)
	return g
}

// changed is changedSpans answered from the batch; renamed, non-local and unbatched paths ask git per file.
func (g *gitBatch) changed(path, old, base string) (lineSpans, error) {
	key := filepath.ToSlash(filepath.Clean(path))
	if g.spans == nil || old != path || !filepath.IsLocal(path) {
		return changedSpans(path, old, base)
	}
	if g.untracked[key] {
		return lineSpans{{1, 1 << 30}}, nil
	}
	return g.spans[key], nil
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
		for _, s := range blockSmells(b, "go") {
			if s.Type == "long_comment" && wasStacked(b, baseBlocks) {
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
	old := baseRules(path, base, th)
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

// baseRules is the set of file-level rules already firing on the base text.
func baseRules(path, base string, th ComplexityThresholds) map[string]bool {
	old := map[string]bool{}
	if res, err := complexityContent(path, base, "go", th); err == nil && base != "" {
		for _, f := range res.Findings {
			old[f.Rule] = true
		}
	}
	return old
}

// testLayoutExempt are test files the layout rules allow to break them, each with its reason.
var testLayoutExempt = map[string]string{
	"internal/memory/jobscope_test.go": "package memory_test: internal/agent imports memory, a real cycle",
}

// testLayoutRules maps the id of each go.md → Tests layout rule a test file's text breaks to its message.
func testLayoutRules(path, text string) map[string]string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	if strings.HasSuffix(file.Name.Name, "_test") {
		out["external-package"] = "test file declares package " + file.Name.Name + "; tests are internal (rules/harness/go.md)"
	}
	for _, g := range file.Comments {
		if g.End() > file.Package {
			break
		}
		for _, c := range g.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				out["build-constraint"] = "test file has a build constraint; skip at runtime (rules/harness/go.md)"
			}
		}
	}
	return out
}

var testFuncRe = regexp.MustCompile(`(?m)^func Test\w*\(`)

// hasSource reports whether dir holds stem.go or an OS-specific stem_<suffix>.go that a foo_test.go may cover.
func hasSource(dir, stem string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if n == stem+".go" || strings.HasPrefix(n, stem+"_") {
			return true
		}
	}
	return false
}

// testLayoutViolations are the layout findings a changed test file adds over base; renamed counts it as new.
func testLayoutViolations(path, text, base string, renamed bool) []string {
	if !strings.HasSuffix(path, "_test.go") {
		return nil
	}
	for suffix := range testLayoutExempt {
		if strings.HasSuffix(filepath.ToSlash(path), suffix) {
			return nil
		}
	}
	old := testLayoutRules(path, base)
	rules := testLayoutRules(path, text)
	var out []string
	for _, id := range []string{"external-package", "build-constraint"} {
		if msg, ok := rules[id]; ok && old[id] == "" {
			out = append(out, "1: "+msg)
		}
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".go"), "_test"), "_regression")
	if (base == "" || renamed) && !hasSource(filepath.Dir(path), stem) {
		out = append(out, "1: test file has no matching "+stem+".go in its directory (rules/harness/go.md)")
	}
	if strings.HasSuffix(path, "_regression_test.go") && len(testFuncRe.FindAllString(text, 2)) < 2 &&
		(base == "" || renamed || len(testFuncRe.FindAllString(base, 2)) >= 2) {
		out = append(out, "1: a _regression_test.go file needs 2 or more tests; fold one into the main test file (rules/harness/go.md)")
	}
	return out
}

// newGoViolations lists the rules path breaks on lines changed since base, as "path:line: message".
func newGoViolations(path, base string, g *gitBatch) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	old := g.renames[path]
	if old == "" {
		old = path
	}
	spans, err := g.changed(path, old, base)
	if err != nil {
		return nil, err
	}
	baseText := g.bases[baseSpec(base, old)]
	text := string(data)
	out, err := sizeViolations(path, text, baseText, spans)
	if err != nil {
		return nil, err
	}
	out = append(out, commentViolations(text, baseText, spans)...)
	out = append(out, testLayoutViolations(path, text, baseText, old != path)...)
	sort.Strings(out)
	for i, v := range out {
		out[i] = path + ":" + v
	}
	return out, nil
}

// failOnNewViolations prints new comment and size violations in the changed Go files; exit 1 when any.
func failOnNewViolations(files []string, base string, stdout, stderr io.Writer) int {
	var found, goFiles []string
	for _, f := range files {
		if strings.HasSuffix(f, ".go") {
			goFiles = append(goFiles, f)
		}
	}
	g := &gitBatch{}
	if len(goFiles) > 0 {
		g = loadGitBatch(goFiles, base)
	}
	for _, f := range goFiles {
		v, err := newGoViolations(f, base, g)
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
