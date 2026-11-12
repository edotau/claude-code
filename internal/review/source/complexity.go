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
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// ComplexityThresholds mirrors complexity_checker.THRESHOLDS.
type ComplexityThresholds struct {
	MaxCyclomatic         int
	MaxNesting            int
	MaxFunctionLines      int
	MaxImports            int
	MaxClassesPer100Lines float64
	MaxFileLines          int
}

var complexityThresholds = map[string]ComplexityThresholds{
	"strict":  {8, 3, 40, 12, 2.0, 400},
	"medium":  {10, 4, 50, 18, 3.0, 800},
	"relaxed": {15, 5, 80, 30, 5.0, 1000},
}

// Finding is one complexity_checker finding; Line is 0 (omitted) for file-level findings.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// FileResult is complexity_checker.analyze_file's dict shape.
type FileResult struct {
	File     string    `json:"file"`
	Language string    `json:"language"`
	Lines    int       `json:"lines"`
	Score    int       `json:"score"`
	Findings []Finding `json:"findings"`
}

// ComplexitySummary is complexity_checker.build_summary's dict shape.
type ComplexitySummary struct {
	Status        string       `json:"status"`
	Threshold     string       `json:"threshold"`
	FilesAnalyzed int          `json:"files_analyzed"`
	TotalFindings int          `json:"total_findings"`
	AverageScore  PyFloat      `json:"average_score"`
	Verdict       string       `json:"verdict"`
	Results       []FileResult `json:"results"`
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
func analyzeTypescript(text string, lineCount int, th ComplexityThresholds) []Finding {
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
		leading := 0
		for _, r := range line {
			if r == ' ' || r == '\t' || r == '\v' || r == '\f' || r == '\r' {
				leading++
				continue
			}
			break
		}
		if depth := leading / 2; depth > deepest {
			deepest = depth
		}
	}
	return deepest
}

// analyzeFileComplexity ports complexity_checker.analyze_file.
func analyzeFileComplexity(path string, th ComplexityThresholds) (*FileResult, error) {
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
		return 1000
	}
	return 0
}

// complexityContent is analyzeFileComplexity over already-read text.
func complexityContent(path, text, lang string, th ComplexityThresholds) (*FileResult, error) {
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
	if score < 0 {
		score = 0
	}
	return &FileResult{File: path, Language: lang, Lines: lineCount, Score: score, Findings: NonNil(findings)}, nil
}

// importCountFinding is the shared import-coupling check.
func importCountFinding(imports int, th ComplexityThresholds) []Finding {
	if imports <= th.MaxImports {
		return nil
	}
	return []Finding{{Rule: "import-count", Severity: "warn",
		Message: fmt.Sprintf("%d imports (max %d). High coupling?", imports, th.MaxImports)}}
}

// classDensityFinding is the shared classes-per-100-lines check.
func classDensityFinding(classes, lineCount int, th ComplexityThresholds) []Finding {
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
func functionFindings(name string, line, length, cyclo, nest int, th ComplexityThresholds) []Finding {
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

// collectComplexityFiles ports complexity_checker.collect_files (a target may be a file or a directory).
func collectComplexityFiles(target string, extensions []string, warn io.Writer) []string {
	files, unreadable := walkComplexityFiles(target, extensions, warn)
	// Unreadable paths below target stay in the list so the analysis reports them; a missing target stays out.
	for _, p := range unreadable {
		if p != target && hasComplexityExtension(p, extensions) {
			files = append(files, p)
		}
	}
	return files
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
func buildComplexitySummary(results []FileResult, threshold string) ComplexitySummary {
	total := 0
	for _, r := range results {
		total += len(r.Findings)
	}
	avg := 100.0
	if len(results) > 0 {
		sum := 0
		for _, r := range results {
			sum += r.Score
		}
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
	return ComplexitySummary{Status: "ok", Threshold: threshold, FilesAnalyzed: len(results),
		TotalFindings: total, AverageScore: PyFloat(avg), Verdict: verdict, Results: NonNil(results)}
}

func printComplexityReport(out io.Writer, s ComplexitySummary) error {
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

type complexityArgs struct {
	targets   []string
	threshold string
	ext       string
	jsonOut   bool
}

// parseComplexityArgs: -h anywhere prints usage to stdout (exit 0); a bad flag goes to stderr (exit 2).
func parseComplexityArgs(args []string, stdout, stderr io.Writer) (complexityArgs, int, bool) {
	fs := helptext.NewFlagSet("review complexity", helptext.Page{
		Usage: "claude-code review complexity <targets...> [flags]",
		About: "Function length, nesting depth and branch counts over files and directories. " +
			"Findings are advisory.",
	})
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	threshold := fs.String("threshold", "medium", "strict|medium|relaxed")
	ext := fs.String("ext", "py,ts,tsx,js,jsx,go", "file extensions to scan, comma-separated")
	if helptext.WantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return complexityArgs{}, 0, true
	}
	fs.SetOutput(stderr)
	targets, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return complexityArgs{}, helptext.ExitCode(err), true
	}
	if len(targets) == 0 {
		fmt.Fprintln(stderr, "review complexity: the following arguments are required: targets")
		fs.Usage()
		return complexityArgs{}, 2, true
	}
	if _, ok := complexityThresholds[*threshold]; !ok {
		fmt.Fprintf(stderr, "review complexity: argument --threshold: invalid choice: %q\n", *threshold)
		return complexityArgs{}, 2, true
	}
	return complexityArgs{targets: targets, threshold: *threshold, ext: *ext, jsonOut: *jsonOut}, 0, false
}

// RunComplexity is the complexity_checker.py CLI: findings are advisory; output and Go syntax errors exit 1.
func RunComplexity(args []string, stdout, stderr io.Writer) int {
	parsed, code, done := parseComplexityArgs(args, stdout, stderr)
	if done {
		return code
	}
	if err := checkTargets(parsed.targets, DetectComplexityLang); err != nil {
		fmt.Fprintf(stderr, "review complexity: %v\n", err)
		return 2
	}
	th := complexityThresholds[parsed.threshold]
	extensions := parseExtensions(parsed.ext)
	files := gatherComplexityFiles(parsed.targets, extensions, stderr)
	if len(files) == 0 {
		return reportNoFiles(stdout, stderr, extensions, parsed.targets, parsed.jsonOut)
	}

	sort.Strings(files)
	results, failed := analyzeComplexityFiles(files, th, stderr)
	summary := buildComplexitySummary(results, parsed.threshold)
	if len(results) == 0 && failed > 0 {
		summary.Status, summary.Verdict = "error", "FAIL"
	}
	if parsed.jsonOut {
		b, err := PyMarshalIndent(summary)
		if err == nil {
			_, err = stdout.Write(b)
		}
		if err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
	} else {
		if err := printComplexityReport(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
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

func parseExtensions(list string) []string {
	var extensions []string
	for e := range strings.SplitSeq(list, ",") {
		e = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e != "" {
			extensions = append(extensions, e)
		}
	}
	return extensions
}

// gatherComplexityFiles collects the files under every target, deduplicated by resolved path.
func gatherComplexityFiles(targets, extensions []string, warn io.Writer) []string {
	seen := map[string]bool{}
	var files []string
	for _, target := range targets {
		for _, f := range collectComplexityFiles(target, extensions, warn) {
			key := resolvePath(f)
			if !seen[key] {
				seen[key] = true
				files = append(files, f)
			}
		}
	}
	return files
}

// analyzeComplexityFiles scores each file; failed counts files that could not be read or parsed.
func analyzeComplexityFiles(files []string, th ComplexityThresholds, stderr io.Writer) (results []FileResult, failed int) {
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

// reportNoFiles ports the argparse "no files matched" branch (advisory unless JSON output fails).
// json.dumps here has no indent= arg (compact, single line) unlike the summary path below.
func reportNoFiles(stdout, stderr io.Writer, extensions, targets []string, jsonOut bool) int {
	msg := fmt.Sprintf("No files matching %s under %s", pyListRepr(extensions), strings.Join(targets, " "))
	if jsonOut {
		// json.dumps with no indent= still separates with ", " / ": " (Go's compact form has no spaces).
		line := fmt.Sprintf(`{"status": %s, "message": %s}`, jsonStringLiteral("error"), jsonStringLiteral(msg))
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(stderr, "[error] %s\n", msg)
	}
	return 0
}

// jsonStringLiteral renders s as a Python-style json.dumps string literal (ensure_ascii, no HTML-escaping).
func jsonStringLiteral(s string) string {
	b, _ := textutil.MarshalNoEscape(s) // a string literal always marshals; nothing here can fail
	return string(pyEscapeNonASCII(b))
}

// pyListRepr mimics Python's str(list[str]): ['py', 'ts', ...] with single quotes.
func pyListRepr(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + s + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func hasComplexityExtension(path string, extensions []string) bool {
	for _, ext := range extensions {
		if strings.HasSuffix(path, "."+ext) {
			return true
		}
	}
	return false
}

// errGoSyntax marks Go source that does not parse; RunComplexity exits 1 for it.
var errGoSyntax = errors.New("go syntax error")

// analyzeGo scores Go source with go/ast; source that does not parse is an error, never a clean score.
func analyzeGo(path, text string, th ComplexityThresholds) ([]Finding, error) {
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

func goFunctionFindings(fset *token.FileSet, fn *ast.FuncDecl, th ComplexityThresholds) []Finding {
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
