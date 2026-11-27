package source

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Size caps from rules/standards/quality.md, shared with the diff scope gate.
const (
	CapFileLines     = 1200
	CapTestFileLines = 1500
	CapFunctionLines = 50
	CapImports       = 25
	CapNesting       = 4
)

// thresholds mirrors complexity_checker.THRESHOLDS.
type thresholds struct {
	MaxCyclomatic         int
	MaxNesting            int
	MaxFunctionLines      int
	MaxImports            int
	MaxClassesPer100Lines float64
	MaxFileLines          int
}

var complexityThresholds = map[string]thresholds{
	"strict":  {8, 3, 40, 12, 2.0, 400},
	"medium":  {10, CapNesting, CapFunctionLines, CapImports, 3.0, CapFileLines},
	"relaxed": {15, 5, 80, 30, 5.0, CapTestFileLines},
}

// Finding is one complexity_checker finding; Line is 0 (omitted) for file-level findings.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// fileResult is complexity_checker.analyze_file's dict shape.
type fileResult struct {
	File     string    `json:"file"`
	Language string    `json:"language"`
	Lines    int       `json:"lines"`
	Score    int       `json:"score"`
	Findings []Finding `json:"findings"`
}

// complexitySummary is complexity_checker.build_summary's dict shape.
type complexitySummary struct {
	Status        string       `json:"status"`
	Threshold     string       `json:"threshold"`
	FilesAnalyzed int          `json:"files_analyzed"`
	TotalFindings int          `json:"total_findings"`
	AverageScore  PyFloat      `json:"average_score"`
	Verdict       string       `json:"verdict"`
	Results       []fileResult `json:"results"`
}

// DetectComplexityLang mirrors complexity_checker.detect_lang.
func DetectComplexityLang(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python"
	case ".ts", ".tsx", ".js", ".jsx":
		return "typescript"
	case ".go":
		return "go"
	default:
		return ""
	}
}

var tsClassPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:abstract\s+)?class\s+\w+`)
})
var tsImportPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*import\s+`)
})

// analyzeTypescript ports complexity_checker.analyze_typescript exactly (only file-wide checks; no per-function ones).
func analyzeTypescript(text string, lineCount int, th thresholds) []Finding {
	var findings []Finding
	findings = append(findings, importCountFinding(len(tsImportPattern().FindAllString(text, -1)), th)...)
	findings = append(findings, classDensityFinding(len(tsClassPattern().FindAllString(text, -1)), lineCount, th)...)
	deepest := deepestIndentUnits(text)
	if deepest > th.MaxNesting {
		findings = append(findings, Finding{Rule: "nesting-depth", Severity: "warn",
			Message: fmt.Sprintf("Indentation reaches %d levels (max %d). Flatten.", deepest, th.MaxNesting)})
	}
	return findings
}

// deepestIndentUnits ports `(len(m)-len(m.lstrip()))//2` over non-blank lines (2-space indent unit, TS norm).
func deepestIndentUnits(text string) int {
	deepest := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		deepest = max(deepest, (len(line)-len(strings.TrimLeft(line, " \t\v\f\r")))/2)
	}
	return deepest
}

// leadingWidth counts indentation columns (a tab advances to the next multiple of 8, Python's tab rule).
func leadingWidth(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8 - (n % 8)
		default:
			return n
		}
	}
	return n
}

// analyzeFileComplexity ports complexity_checker.analyze_file.
func analyzeFileComplexity(path string, th thresholds) (*fileResult, error) {
	lang := DetectComplexityLang(path)
	if lang == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return complexityContent(path, string(data), lang, th)
}

// testFileLines is the file-length cap of a Go test file (rules/standards/quality.md); 0 for any other path.
func testFileLines(path string) int {
	if strings.HasSuffix(path, "_test.go") {
		return CapTestFileLines
	}
	return 0
}

// complexityContent is analyzeFileComplexity over already-read text.
func complexityContent(path, text, lang string, th thresholds) (*fileResult, error) {
	lineCount := 0
	if text != "" { // splitlines() gives 0 lines for "" and drops a trailing newline's empty tail
		lineCount = len(strings.Split(strings.TrimSuffix(text, "\n"), "\n"))
	}

	var findings []Finding
	if maxLines := max(th.MaxFileLines, testFileLines(path)); lineCount > maxLines {
		findings = append(findings, Finding{Rule: "file-length", Severity: "warn",
			Message: fmt.Sprintf("%d lines (max %d). Consider splitting.", lineCount, maxLines)})
	}
	switch lang {
	case "python":
		findings = append(findings, analyzePython(text, lineCount, th)...)
	case "go":
		goFindings, err := analyzeGo(path, text, th)
		if err != nil {
			return nil, err
		}
		findings = append(findings, goFindings...)
	default:
		findings = append(findings, analyzeTypescript(text, lineCount, th)...)
	}

	score := 100
	for _, f := range findings {
		if f.Severity == "warn" {
			score -= 15
		} else {
			score -= 5
		}
	}
	return &fileResult{File: path, Language: lang, Lines: lineCount, Score: max(score, 0), Findings: NonNil(findings)}, nil
}

// importCountFinding is the shared import-coupling check.
func importCountFinding(imports int, th thresholds) []Finding {
	if imports <= th.MaxImports {
		return nil
	}
	return []Finding{{Rule: "import-count", Severity: "warn",
		Message: fmt.Sprintf("%d imports (max %d). High coupling?", imports, th.MaxImports)}}
}

// classDensityFinding is the shared classes-per-100-lines check.
func classDensityFinding(classes, lineCount int, th thresholds) []Finding {
	if lineCount <= 0 {
		return nil
	}
	density := float64(classes) / (float64(lineCount) / 100)
	if density <= th.MaxClassesPer100Lines {
		return nil
	}
	return []Finding{{Rule: "class-density", Severity: "warn",
		Message: fmt.Sprintf("%d classes in %d lines (%.1f/100). Premature abstraction?", classes, lineCount, density)}}
}

// functionFindings is the shared length, cyclomatic and nesting check for one function.
func functionFindings(name string, line, length, cyclo, nest int, th thresholds) []Finding {
	var out []Finding
	if length > th.MaxFunctionLines {
		out = append(out, Finding{Rule: "function-length", Severity: "warn", Line: line,
			Message: fmt.Sprintf("'%s' is %d lines (max %d). Split it.", name, length, th.MaxFunctionLines)})
	}
	if cyclo > th.MaxCyclomatic {
		out = append(out, Finding{Rule: "cyclomatic-complexity", Severity: "warn", Line: line,
			Message: fmt.Sprintf("'%s' has cyclomatic complexity %d (max %d). Flatten branching.", name, cyclo, th.MaxCyclomatic)})
	}
	if nest > th.MaxNesting {
		out = append(out, Finding{Rule: "nesting-depth", Severity: "warn", Line: line,
			Message: fmt.Sprintf("'%s' nests %d levels deep (max %d). Use early returns.", name, nest, th.MaxNesting)})
	}
	return out
}

// walkComplexityFiles returns the matching files under target and every path the walk could not read.
func walkComplexityFiles(target string, extensions []string, warn io.Writer) (files, unreadable []string) {
	info, err := os.Stat(target)
	if err == nil && !info.IsDir() {
		return []string{target}, nil
	}
	unreadable = walkSourceFiles(target, warn, func(p string) {
		// Keep suffix matching: callers may request multi-part or overlapping extensions.
		if hasComplexityExtension(p, extensions) {
			files = append(files, p)
		}
	})
	return files, unreadable
}

// buildComplexitySummary ports complexity_checker.build_summary.
func buildComplexitySummary(results []fileResult, threshold string) complexitySummary {
	total, sum := 0, 0
	for _, r := range results {
		total, sum = total+len(r.Findings), sum+r.Score
	}
	avg := 100.0
	if len(results) > 0 {
		avg = Round1(float64(sum) / float64(len(results)))
	}
	verdict := "PASS"
	if total > 0 {
		if avg >= 50 {
			verdict = "WARN"
		} else {
			verdict = "FAIL"
		}
	}
	return complexitySummary{Status: "ok", Threshold: threshold, FilesAnalyzed: len(results),
		TotalFindings: total, AverageScore: PyFloat(avg), Verdict: verdict, Results: NonNil(results)}
}

func printComplexityReport(out io.Writer, s complexitySummary) error {
	var w strings.Builder
	fmt.Fprintf(&w, "Complexity Check — %d files, threshold %s\n", s.FilesAnalyzed, s.Threshold)
	fmt.Fprintf(&w, "Average score: %.0f/100   Findings: %d\n\n", float64(s.AverageScore), s.TotalFindings)
	for _, r := range s.Results {
		if len(r.Findings) == 0 {
			continue
		}
		fmt.Fprintf(&w, "  %s  (score %d/100)\n", r.File, r.Score)
		for _, f := range r.Findings {
			loc := ""
			if f.Line > 0 {
				loc = fmt.Sprintf("  line %d", f.Line)
			}
			fmt.Fprintf(&w, "    [%s] %s%s: %s\n", strings.ToUpper(f.Severity), f.Rule, loc, f.Message)
		}
		fmt.Fprintln(&w)
	}
	if s.TotalFindings == 0 {
		fmt.Fprintln(&w, "  No findings. Code looks appropriately simple.")
	}
	fmt.Fprintf(&w, "Verdict: %s\n", s.Verdict)
	_, err := io.WriteString(out, w.String())
	return err
}

// checkTargets refuses a named target that is missing or a file no analyzer handles; directories pass.
func checkTargets(targets []string, language func(string) string) error {
	for _, t := range targets {
		info, err := os.Stat(t)
		if err != nil {
			return fmt.Errorf("%w; pass an existing file or directory", err)
		}
		if !info.IsDir() && language(t) == "" {
			return fmt.Errorf("%s: unsupported file type %q; pass a source file or a directory", t, filepath.Ext(t))
		}
	}
	return nil
}

// analyzeComplexityFiles scores each file; failed counts files that could not be read or parsed.
func analyzeComplexityFiles(files []string, th thresholds, stderr io.Writer) (results []fileResult, failed int) {
	for _, f := range files {
		r, err := analyzeFileComplexity(f, th)
		if err != nil {
			verb := "read"
			if errors.Is(err, errGoSyntax) {
				verb = "parse"
			}
			fmt.Fprintf(stderr, "[warning] could not %s %s: %v\n", verb, f, err)
			failed++
			continue
		}
		if r != nil {
			results = append(results, *r)
		}
	}
	return results, failed
}

func hasComplexityExtension(path string, extensions []string) bool {
	for _, ext := range extensions {
		if strings.HasSuffix(path, "."+ext) {
			return true
		}
	}
	return false
}

// errGoSyntax marks Go source that does not parse; --complexity exits 1 for it.
var errGoSyntax = errors.New("go syntax error")

// analyzeGo scores Go source with go/ast; source that does not parse is an error, never a clean score.
func analyzeGo(path, text string, th thresholds) ([]Finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGoSyntax, err)
	}
	var findings []Finding
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			findings = append(findings, goFunctionFindings(fset, fn, th)...)
		}
	}
	findings = append(findings, importCountFinding(len(file.Imports), th)...)
	return findings, nil
}

func goFunctionFindings(fset *token.FileSet, fn *ast.FuncDecl, th thresholds) []Finding {
	start, end := fset.Position(fn.Pos()), fset.Position(fn.End())
	name, line := fn.Name.Name, start.Line
	return functionFindings(name, line, end.Line-start.Line+1, goCyclomatic(fn.Body), goNesting(fn.Body, 0), th)
}

// goCyclomatic is 1 + if/for/range/case/comm clauses + short-circuit operators.
func goCyclomatic(body *ast.BlockStmt) int {
	score := 1
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			score++
		case *ast.CaseClause:
			if v.List != nil { // default adds no branch
				score++
			}
		case *ast.CommClause:
			if v.Comm != nil { // select default adds no branch either
				score++
			}
		case *ast.BinaryExpr:
			if v.Op == token.LAND || v.Op == token.LOR {
				score++
			}
		}
		return true
	})
	return score
}

// goNesting returns the deepest count of enclosing if/for/range/switch/select blocks.
func goNesting(root ast.Node, depth int) int {
	deepest := depth
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil || n == root {
			return n == root
		}
		switch v := n.(type) {
		case *ast.IfStmt:
			if d := goIfChainNesting(v, depth+1); d > deepest {
				deepest = d
			}
			return false
		case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			if d := goNesting(n, depth+1); d > deepest {
				deepest = d
			}
			return false
		}
		return true
	})
	return deepest
}

// goIfChainNesting scores an if/else-if chain flat: each else-if shares the first if's depth.
func goIfChainNesting(v *ast.IfStmt, depth int) int {
	deepest := depth
	for cur := v; cur != nil; {
		if d := goNesting(cur.Body, depth); d > deepest {
			deepest = d
		}
		switch e := cur.Else.(type) {
		case *ast.IfStmt:
			cur = e
			continue
		case *ast.BlockStmt:
			if d := goNesting(e, depth); d > deepest {
				deepest = d
			}
		}
		cur = nil
	}
	return deepest
}
