package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
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
	"medium":  {10, 4, 50, 18, 3.0, 600},
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

// detectComplexityLang mirrors complexity_checker.detect_lang.
func detectComplexityLang(path string) string {
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
	imports := len(tsImportPattern().FindAllString(text, -1))
	if imports > th.MaxImports {
		findings = append(findings, Finding{Rule: "import-count", Severity: "warn",
			Message: fmt.Sprintf("%d imports (max %d). High coupling?", imports, th.MaxImports)})
	}
	classes := len(tsClassPattern().FindAllString(text, -1))
	if lineCount > 0 {
		density := float64(classes) / (float64(lineCount) / 100)
		if density > th.MaxClassesPer100Lines {
			findings = append(findings, Finding{Rule: "class-density", Severity: "warn",
				Message: fmt.Sprintf("%d classes in %d lines (%.1f/100). Premature abstraction?", classes, lineCount, density)})
		}
	}
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
	lang := detectComplexityLang(path)
	if lang == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	lineCount := len(strings.Split(text, "\n"))
	if text != "" && strings.HasSuffix(text, "\n") {
		lineCount-- // splitlines() drops a trailing newline's empty tail; strings.Split does not.
	}

	var findings []Finding
	if lineCount > th.MaxFileLines {
		findings = append(findings, Finding{Rule: "file-length", Severity: "warn",
			Message: fmt.Sprintf("%d lines (max %d). Consider splitting.", lineCount, th.MaxFileLines)})
	}
	switch lang {
	case "python":
		findings = append(findings, analyzePython(text, lineCount, th)...)
	case "go":
		findings = append(findings, analyzeGo(path, text, lineCount, th)...)
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
	return &FileResult{File: path, Language: lang, Lines: lineCount, Score: score, Findings: nonNilFindings(findings)}, nil
}

func nonNilFindings(f []Finding) []Finding {
	if f == nil {
		return []Finding{}
	}
	return f
}

// collectComplexityFiles ports complexity_checker.collect_files (a target may be a file or a directory).
func collectComplexityFiles(target string, extensions []string) []string {
	info, err := os.Stat(target)
	if err == nil && !info.IsDir() {
		return []string{target}
	}
	extensionSet := complexityExtensionSet(extensions)
	var files []string
	_ = filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if pathHasSkippedPart(p) {
			return nil
		}
		// Keep suffix matching: callers may request multi-part or overlapping extensions.
		if hasComplexityExtension(p, extensionSet) {
			files = append(files, p)
		}
		return nil
	})
	return files
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
		avg = round1(float64(sum) / float64(len(results)))
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
		TotalFindings: total, AverageScore: PyFloat(avg), Verdict: verdict, Results: nonNilResults(results)}
}

func nonNilResults(r []FileResult) []FileResult {
	if r == nil {
		return []FileResult{}
	}
	return r
}

func printComplexityReport(w io.Writer, s ComplexitySummary) {
	fmt.Fprintf(w, "Complexity Check — %d files, threshold %s\n", s.FilesAnalyzed, s.Threshold)
	fmt.Fprintf(w, "Average score: %.0f/100   Findings: %d\n\n", float64(s.AverageScore), s.TotalFindings)
	for _, r := range s.Results {
		if len(r.Findings) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s  (score %d/100)\n", r.File, r.Score)
		for _, f := range r.Findings {
			loc := ""
			if f.Line > 0 {
				loc = fmt.Sprintf("  line %d", f.Line)
			}
			fmt.Fprintf(w, "    [%s] %s%s: %s\n", strings.ToUpper(f.Severity), f.Rule, loc, f.Message)
		}
		fmt.Fprintln(w)
	}
	if s.TotalFindings == 0 {
		fmt.Fprintln(w, "  No findings. Code looks appropriately simple.")
	}
	fmt.Fprintf(w, "Verdict: %s\n", s.Verdict)
}

type complexityArgs struct {
	targets   []string
	threshold string
	ext       string
	jsonOut   bool
}

// parseComplexityArgs is a real FlagSet, so -h anywhere in args prints the usage page; targets (nargs="+")
// may still be interspersed with flags. Explicit -h prints to stdout and exits 0; a bad flag stays on
// stderr, exit 2.
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
	targets, err := outfmt.Parse(fs, args)
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

// RunComplexity is the complexity_checker.py CLI: findings are advisory; JSON output errors exit 1.
func RunComplexity(args []string, stdout, stderr io.Writer) int {
	parsed, code, done := parseComplexityArgs(args, stdout, stderr)
	if done {
		return code
	}
	th := complexityThresholds[parsed.threshold]
	var extensions []string
	for e := range strings.SplitSeq(parsed.ext, ",") {
		e = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e != "" {
			extensions = append(extensions, e)
		}
	}

	seen := map[string]bool{}
	var files []string
	for _, target := range parsed.targets {
		for _, f := range collectComplexityFiles(target, extensions) {
			key := resolvePath(f)
			if !seen[key] {
				seen[key] = true
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		return reportNoFiles(stdout, stderr, extensions, parsed.targets, parsed.jsonOut)
	}

	sort.Strings(files)
	var results []FileResult
	for _, f := range files {
		r, err := analyzeFileComplexity(f, th)
		if err != nil {
			fmt.Fprintf(stderr, "[warning] could not read %s: %v\n", f, err)
			continue
		}
		if r != nil {
			results = append(results, *r)
		}
	}
	summary := buildComplexitySummary(results, parsed.threshold)
	if parsed.jsonOut {
		if err := writeJSON(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
	} else {
		printComplexityReport(stdout, summary)
	}
	return 0
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string literal always marshals; nothing here can fail
	return string(escapeNonASCII(bytes.TrimRight(buf.Bytes(), "\n")))
}

// pyListRepr mimics Python's str(list[str]): ['py', 'ts', ...] with single quotes.
func pyListRepr(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + s + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func complexityExtensionSet(extensions []string) map[string]struct{} {
	set := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		set[ext] = struct{}{}
	}
	return set
}

func hasComplexityExtension(path string, extensions map[string]struct{}) bool {
	for ext := range extensions {
		if strings.HasSuffix(path, "."+ext) {
			return true
		}
	}
	return false
}
