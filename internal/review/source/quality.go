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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// thresholds mirrors quality_core.THRESHOLDS.
var qualityThresholds = map[string]int{
	"long_function_lines": 50,
	"too_many_parameters": 5,
	"high_complexity":     10,
	"god_class_methods":   20,
	"max_imports":         15,
}

// jsFunctionPattern has one (name, params) group pair per alternative: `function f(a)` or `const f = (a) =>`.
const jsFunctionPattern = `function\s+(\w+)\s*(?:<(?:[^<>]|<[^<>]*>)*>)?\s*\(([^)]*)\)|(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\(([^)]*)\)\s*=>`

// Regexes compile lazily (sync.OnceValue): this binary runs once per hook/tool invocation.
var functionPatterns = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`def\s+(\w+)\s*\(([^)]*)\)`),
		"typescript": regexp.MustCompile(jsFunctionPattern),
		"javascript": regexp.MustCompile(jsFunctionPattern),
		"go":         regexp.MustCompile(`func\s+(?:\([^)]+\)\s+)?(\w+)\s*(?:\[[^\]]*\])?\s*\(([^)]*)\)`),
		"swift":      regexp.MustCompile(`func\s+(\w+)\s*\(([^)]*)\)`),
		"kotlin":     regexp.MustCompile(`fun\s+(\w+)\s*\(([^)]*)\)`),
	}
})

var classPatterns = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`(?m)^\s*class\s+(\w+)`),
		"typescript": regexp.MustCompile(`class\s+(\w+)`),
		"javascript": regexp.MustCompile(`class\s+(\w+)`),
		"go":         regexp.MustCompile(`type\s+(\w+)\s+struct`),
		"swift":      regexp.MustCompile(`class\s+(\w+)`),
		"kotlin":     regexp.MustCompile(`class\s+(\w+)`),
	}
})

var methodPatterns = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`def\s+\w+\s*\(`),
		"typescript": regexp.MustCompile(`(?:public|private|protected)?\s*\w+\s*\([^)]*\)\s*[:{]`),
		"javascript": regexp.MustCompile(`\w+\s*\([^)]*\)\s*\{`),
		"go":         regexp.MustCompile(`func\s+\(`),
		"swift":      regexp.MustCompile(`func\s+\w+`),
		"kotlin":     regexp.MustCompile(`fun\s+\w+`),
	}
})

var cyclomaticKeywords = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\bif\b|\belif\b|\bfor\b|\bwhile\b|\bcase\b|\bcatch\b|\bexcept\b|\band\b|\bor\b|\|\||&&`)
})
var solidTypeCheck = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`isinstance\(|type\(.*\)\s*==|typeof\s+\w+\s*===`)
})
var solidNotImpl = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)raise\s+notimplementederror|not\s+implemented`)
})
var solidImports = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(?:import|from)\s+`)
})
var commentedCodePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*[#/]+\s*(if|for|while|def|function|class|const|let|var)\s.*[{:;(]\s*$`)
})
var magicNumberDigits = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\d{3,}`)
})

// detectLanguage mirrors quality_core.detect_language.
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	for _, lang := range languageOrder {
		if slices.Contains(Extensions[lang], ext) {
			return lang
		}
	}
	return ""
}

// countLines mirrors quality_core.count_lines.
func countLines(content string) LineMetrics {
	lines := strings.Split(content, "\n")
	blank, comment := 0, 0
	prefixes := []string{"#", "//", "/*", "'''", `"""`}
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" {
			blank++
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(s, p) {
				comment++
				break
			}
		}
	}
	total := len(lines)
	return LineMetrics{Total: total, Code: total - blank - comment, Blank: blank, Comment: comment}
}

// calcCyclomaticComplexity mirrors quality_core.calculate_cyclomatic_complexity.
func calcCyclomaticComplexity(content string) int {
	return 1 + len(cyclomaticKeywords().FindAllString(content, -1))
}

// functionNameParams reads a match's name and parameter text from (name, params) group pairs, first non-empty pair wins.
func functionNameParams(groups []string) (string, string) {
	for i := 0; i+1 < len(groups); i += 2 {
		if groups[i] != "" {
			return groups[i], groups[i+1]
		}
	}
	return "anonymous", ""
}

// findFunctions ports quality_core.find_functions; each language pattern yields (name, params) group pairs.
func findFunctions(content, language string) []FunctionInfo {
	pattern, ok := functionPatterns()[language]
	if !ok {
		pattern = functionPatterns()["python"]
	}
	var goEnds []int
	if language == "go" {
		goEnds = goFunctionEnds(content)
	}
	var out []FunctionInfo
	for _, loc := range pattern.FindAllStringSubmatchIndex(content, -1) {
		name, paramsStr := functionNameParams(submatchStrings(content, loc, pattern))
		params := splitParams(paramsStr)
		matchEnd := loc[1]
		body := functionBody(content, matchEnd, pattern)
		for _, end := range goEnds {
			if end > matchEnd { // ends ascend: the first one past the signature closes this func
				body = body[:min(len(body), end-matchEnd)]
				break
			}
		}
		out = append(out, FunctionInfo{
			Name:       name,
			Parameters: len(params),
			Lines:      len(strings.Split(body, "\n")),
			Complexity: calcCyclomaticComplexity(body),
		})
	}
	return out
}

func goFunctionEnds(content string) []int {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", content, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var ends []int
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			ends = append(ends, fset.Position(fn.End()).Offset)
		}
	}
	return ends
}

// submatchStrings returns capture groups 1..n as strings ("" when a group did not participate).
func submatchStrings(content string, loc []int, pattern *regexp.Regexp) []string {
	n := pattern.NumSubexp()
	out := make([]string, n)
	for i := 1; i <= n; i++ {
		s, e := loc[2*i], loc[2*i+1]
		if s >= 0 && e >= 0 {
			out[i-1] = content[s:e]
		}
	}
	return out
}

// splitParams mirrors `[p.strip() for p in params_str.split(",") if p.strip()]`.
func splitParams(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// functionBody spans from matchEnd to the next match of pattern, or to the end of content when there is none.
func functionBody(content string, matchEnd int, pattern *regexp.Regexp) string {
	remaining := content[matchEnd:]
	if loc := pattern.FindStringIndex(remaining); loc != nil {
		return remaining[:loc[0]]
	}
	return remaining
}

// classBody is the text a class match owns: Python ends it by indent, other languages at the next class match.
func classBody(content string, m []int, pattern *regexp.Regexp, language string) string {
	if language == "python" {
		return pythonClassBody(content, m[2], m[3])
	}
	remaining := content[m[1]:]
	if loc := pattern.FindStringIndex(remaining); loc != nil {
		return remaining[:loc[0]]
	}
	return remaining
}

// pythonClassBody returns the lines after the class header that are blank or indented deeper than it.
func pythonClassBody(content string, nameStart, nameEnd int) string {
	lineStart := strings.LastIndex(content[:nameStart], "\n") + 1
	indent := leadingWidth(content[lineStart:])
	headerEnd := pythonHeaderEnd(content, nameEnd)
	if headerEnd < 0 {
		return ""
	}
	_, after, ok := strings.Cut(content[headerEnd:], "\n")
	if !ok {
		return ""
	}
	lines := strings.Split(after, "\n")
	n := 0
	for n < len(lines) && (blankOrComment(lines[n]) || leadingWidth(lines[n]) > indent) {
		n++
	}
	return strings.Join(lines[:n], "\n")
}

// blankOrComment reports a line that must not end a Python class body (a column-0 comment is not dedent).
func blankOrComment(line string) bool {
	s := strings.TrimSpace(line)
	return s == "" || strings.HasPrefix(s, "#")
}

// pythonHeaderEnd returns the offset just past the ':' closing a class header (parens may span lines), or -1.
func pythonHeaderEnd(content string, from int) int {
	depth := 0
	for i := from; i < len(content); i++ {
		switch content[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '#': // comment: skip to its newline, which the loop then handles
			nl := strings.IndexByte(content[i:], '\n')
			if nl < 0 {
				return -1
			}
			i += nl - 1
		case '\'', '"':
			i = pythonStringEnd(content, i)
		case ':':
			if depth == 0 {
				return i + 1
			}
		case '\n':
			if depth == 0 {
				return -1
			}
		}
	}
	return -1
}

// pythonStringEnd returns the index of the quote closing the string opened at i (len(content) if unclosed).
func pythonStringEnd(content string, i int) int {
	q := content[i]
	if triple := strings.Repeat(string(q), 3); strings.HasPrefix(content[i:], triple) {
		end := strings.Index(content[i+3:], triple)
		if end < 0 {
			return len(content)
		}
		return i + 3 + end + 2
	}
	for j := i + 1; j < len(content); j++ {
		switch content[j] {
		case '\\':
			j++
		case q:
			return j
		case '\n': // single-line string: stop before the newline so the header scan sees it
			return j - 1
		}
	}
	return len(content)
}

// pythonMethodCount counts method defs in a class body, skipping the bodies of nested classes.
func pythonMethodCount(body string, methodPattern *regexp.Regexp) int {
	count, nestedIndent := 0, -1
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			w := leadingWidth(line)
			if nestedIndent >= 0 && w <= nestedIndent {
				nestedIndent = -1
			}
			if nestedIndent < 0 && pythonNestedClass().MatchString(line) {
				nestedIndent = w
				continue
			}
		}
		if nestedIndent < 0 {
			count += len(methodPattern.FindAllString(line, -1))
		}
	}
	return count
}

var pythonNestedClass = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\s*class\s+\w+`)
})

// goMethodCount counts `func (r *?Name[...]) ` receivers anywhere in content, since Go methods live outside the type.
func goMethodCount(content, name string) int {
	re := regexp.MustCompile(`(?m)^func\s+\(\s*(?:\w+\s+)?\*?` + regexp.QuoteMeta(name) + `(?:\[[^\]]*\])?\s*\)`)
	return len(re.FindAllStringIndex(content, -1))
}

// findClasses ports quality_core.find_classes.
func findClasses(content, language string) []ClassInfo {
	pattern, ok := classPatterns()[language]
	if !ok {
		pattern = classPatterns()["python"]
	}
	methodPattern, ok := methodPatterns()[language]
	if !ok {
		methodPattern = methodPatterns()["python"]
	}
	var out []ClassInfo
	for _, m := range pattern.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		body := classBody(content, m, pattern, language)
		methods := len(methodPattern.FindAllString(body, -1))
		switch language {
		case "go":
			methods = goMethodCount(content, name)
		case "python":
			methods = pythonMethodCount(body, methodPattern)
		}
		out = append(out, ClassInfo{
			Name:    name,
			Methods: methods,
			Lines:   len(strings.Split(body, "\n")),
		})
	}
	return out
}

// magicNumberOnLine ports the `\b(?<![.\"\'])\d{3,}\b(?!\.\d)` lookaround via manual boundary checks (RE2 has none).
func magicNumberOnLine(line string) (string, bool) {
	for _, loc := range magicNumberDigits().FindAllStringIndex(line, -1) {
		start, end := loc[0], loc[1]
		if magicNumberBounded(line, start, end) {
			return line[start:end], true
		}
	}
	return "", false
}

// magicNumberBounded applies the lookbehind/lookahead of the magic-number regex to the digits at line[start:end].
func magicNumberBounded(line string, start, end int) bool {
	if start > 0 {
		prev := line[start-1]
		if isWordByte(prev) || prev == '.' || prev == '"' || prev == '\'' {
			return false
		}
	}
	return end >= len(line) || !digitsContinue(line, end)
}

// digitsContinue reports a word byte, or a ".<digit>" fraction, right after the digits ending at end.
func digitsContinue(line string, end int) bool {
	next := line[end]
	if isWordByte(next) {
		return true
	}
	return next == '.' && end+1 < len(line) && line[end+1] >= '0' && line[end+1] <= '9'
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// checkCodeSmells ports quality_core.check_code_smells.
func checkCodeSmells(content, lang string, functions []FunctionInfo, classes []ClassInfo) []Smell {
	var smells []Smell
	for _, f := range functions {
		smells = append(smells, functionSmells(f)...)
	}
	for _, c := range classes {
		if c.Methods > qualityThresholds["god_class_methods"] {
			smells = append(smells, Smell{Type: "god_class", Severity: "high",
				Message:  fmt.Sprintf("Class '%s' has %d methods (max: %d)", c.Name, c.Methods, qualityThresholds["god_class_methods"]),
				Location: c.Name})
		}
	}
	smells = append(smells, lineSmells(content)...)
	return append(smells, commentSmells(content, lang)...)
}

// lineSmells flags magic numbers (outside comments and imports) and commented-out code, line by line.
func lineSmells(content string) []Smell {
	var smells []Smell
	for i, line := range strings.Split(content, "\n") {
		lineNo := i + 1
		trimmed := strings.TrimSpace(line)
		skip := strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "import") || strings.HasPrefix(trimmed, "from")
		if !skip {
			if m, ok := magicNumberOnLine(line); ok {
				smells = append(smells, Smell{Type: "magic_number", Severity: "low",
					Message: fmt.Sprintf("Magic number %s should be a named constant", m), Location: fmt.Sprintf("line %d", lineNo)})
			}
		}
		if commentedCodePattern().MatchString(line) {
			smells = append(smells, Smell{Type: "commented_code", Severity: "low",
				Message: "Commented-out code should be removed", Location: fmt.Sprintf("line %d", lineNo)})
		}
	}
	return smells
}

// functionSmells is the per-function slice of check_code_smells (kept separate to stay under 50 lines).
func functionSmells(f FunctionInfo) []Smell {
	var out []Smell
	if f.Lines > qualityThresholds["long_function_lines"] {
		out = append(out, Smell{Type: "long_function", Severity: "medium",
			Message:  fmt.Sprintf("Function '%s' has %d lines (max: %d)", f.Name, f.Lines, qualityThresholds["long_function_lines"]),
			Location: f.Name})
	}
	if f.Parameters > qualityThresholds["too_many_parameters"] {
		out = append(out, Smell{Type: "too_many_parameters", Severity: "low",
			Message:  fmt.Sprintf("Function '%s' has %d parameters (max: %d)", f.Name, f.Parameters, qualityThresholds["too_many_parameters"]),
			Location: f.Name})
	}
	if f.Complexity > qualityThresholds["high_complexity"] {
		severity := "medium"
		if f.Complexity > 20 {
			severity = "high"
		}
		out = append(out, Smell{Type: "high_complexity", Severity: severity,
			Message:  fmt.Sprintf("Function '%s' has complexity %d (max: %d)", f.Name, f.Complexity, qualityThresholds["high_complexity"]),
			Location: f.Name})
	}
	return out
}

// checkSolidViolations ports quality_core.check_solid_violations.
func checkSolidViolations(content string) []SolidViolation {
	var out []SolidViolation
	if n := len(solidTypeCheck().FindAllString(content, -1)); n > 2 {
		out = append(out, SolidViolation{Principle: "OCP", Name: "Open/Closed Principle", Severity: "medium",
			Message: fmt.Sprintf("Found %d type checks - consider using polymorphism", n)})
	}
	if n := len(solidNotImpl().FindAllString(content, -1)); n > 0 {
		out = append(out, SolidViolation{Principle: "LSP/ISP", Name: "Liskov/Interface Segregation", Severity: "low",
			Message: fmt.Sprintf("Found %d unimplemented methods - may indicate oversized interface", n)})
	}
	if n := len(solidImports().FindAllString(content, -1)); n > qualityThresholds["max_imports"] {
		out = append(out, SolidViolation{Principle: "DIP", Name: "Dependency Inversion Principle", Severity: "low",
			Message: fmt.Sprintf("File has %d imports - consider dependency injection", n)})
	}
	return out
}

// calculateQualityScore ports quality_core.calculate_quality_score.
func calculateQualityScore(lm LineMetrics, functions []FunctionInfo, smells []Smell, violations []SolidViolation) int {
	score := 100
	smellPenalty := map[string]int{"high": 10, "medium": 5, "low": 2}
	violationPenalty := map[string]int{"high": 8, "medium": 4, "low": 2}
	for _, s := range smells {
		score -= smellPenalty[s.Severity]
	}
	for _, v := range violations {
		score -= violationPenalty[v.Severity]
	}
	score += commentRatioBonus(lm) + shortFunctionBonus(functions)
	return min(max(score, 0), 100)
}

func commentRatioBonus(lm LineMetrics) int {
	if lm.Total == 0 {
		return 0
	}
	if ratio := float64(lm.Comment) / float64(lm.Total); ratio >= 0.1 && ratio <= 0.3 {
		return 5
	}
	return 0
}

func shortFunctionBonus(functions []FunctionInfo) int {
	if len(functions) == 0 {
		return 0
	}
	total := 0
	for _, f := range functions {
		total += f.Lines
	}
	if float64(total)/float64(len(functions)) < 30 {
		return 5
	}
	return 0
}

// getGrade ports quality_core.get_grade.
func getGrade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 80:
		return "B"
	case score >= 70:
		return "C"
	case score >= 60:
		return "D"
	default:
		return "F"
	}
}

// round1 matches Python's round(x, 1) (round-half-to-even at that digit, negligible in practice here).
func round1(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
	return v
}

// Quality is the analysis behind RunQuality / analyze_file. language == "" autodetects from the extension.
func Quality(path, language string) (QualityReport, error) {
	lang := language
	if lang == "" {
		lang = detectLanguage(path)
		if lang == "" {
			return QualityReport{}, fmt.Errorf("unsupported file type: %s", filepath.Ext(path))
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return QualityReport{}, fmt.Errorf("could not read file: %s: %w", path, err)
	}
	return QualityContent(path, string(data), lang)
}

// QualityContent is Quality over already-read content; lang must be set.
func QualityContent(path, content, lang string) (QualityReport, error) {
	lineMetrics := countLines(content)
	functions := findFunctions(content, lang)
	classes := findClasses(content, lang)
	smells := checkCodeSmells(content, lang, functions, classes)
	violations := checkSolidViolations(content)
	score := calculateQualityScore(lineMetrics, functions, smells, violations)

	avgComplexity := PyFloat(0)
	if len(functions) > 0 {
		total := 0
		for _, f := range functions {
			total += f.Complexity
		}
		avgComplexity = PyFloat(round1(float64(total) / float64(len(functions))))
	}

	return QualityReport{
		File:     path,
		Language: lang,
		Metrics: Metrics{
			Lines:         lineMetrics,
			Functions:     len(functions),
			Classes:       len(classes),
			AvgComplexity: avgComplexity,
		},
		QualityScore:    score,
		Grade:           getGrade(score),
		Smells:          nonNil(smells),
		SolidViolations: nonNil(violations),
		FunctionDetails: capSlice(functions, 10),
		ClassDetails:    capSlice(classes, 10),
	}, nil
}

// nonNil keeps the JSON output "[]" instead of "null" for empty slices.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// capSlice is nonNil plus truncation to n entries.
func capSlice[T any](s []T, n int) []T {
	return nonNil(s[:min(len(s), n)])
}

var skipParts = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".venv": true, "venv": true, "dist": true, "build": true}

// pathHasSkippedPart mirrors `SKIP_PARTS & set(filepath.parts)`.
func pathHasSkippedPart(path string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if skipParts[part] {
			return true
		}
	}
	return false
}

// walkSourceFiles calls fn for each non-skipped file under root and returns the paths the walk could not read.
func walkSourceFiles(root string, fn func(p string)) []string {
	var unreadable []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if rel, rerr := filepath.Rel(root, p); rerr != nil || pathHasSkippedPart(rel) {
			return nil
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping unreadable path %s: %v\n", p, err)
			unreadable = append(unreadable, p)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fn(p)
		return nil
	})
	return unreadable
}

// analyzeDirectory ports code_quality_checker.analyze_directory.
func analyzeDirectory(dir, language string) (directoryReport, error) {
	var extensions []string
	if language != "" {
		extensions = Extensions[language]
	} else {
		for _, lang := range languageOrder {
			extensions = append(extensions, Extensions[lang]...)
		}
	}

	results, failed := walkExtensions(dir, extensions)
	if len(results) == 0 && len(failed) > 0 {
		return directoryReport{}, fmt.Errorf("analysis failed for %d files, e.g. %s", len(failed), failed[0])
	}
	if len(results) == 0 {
		return directoryReport{}, errors.New("no supported files found")
	}

	total := 0.0
	for _, r := range results {
		total += float64(r.QualityScore)
	}
	avg := PyFloat(round1(total / float64(len(results))))
	smells, violations := 0, 0
	for _, r := range results {
		smells += len(r.Smells)
		violations += len(r.SolidViolations)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].QualityScore < results[j].QualityScore })

	return directoryReport{
		Directory: dir, FilesAnalyzed: len(results), AverageScore: avg, OverallGrade: getGrade(int(avg)),
		TotalCodeSmells: smells, TotalSolidViolations: violations, Files: results, FilesFailed: failed,
	}, nil
}

// walkExtensions collects supported files in one traversal, retaining extension and path ordering; failed lists
// the files whose analysis errored.
func walkExtensions(dir string, extensions []string) (out []QualityReport, failed []string) {
	byExtension := make(map[string][]string, len(extensions))
	for _, ext := range extensions {
		byExtension[ext] = nil
	}
	failed = walkSourceFiles(dir, func(p string) {
		for ext := range byExtension {
			if strings.HasSuffix(p, ext) {
				byExtension[ext] = append(byExtension[ext], p)
			}
		}
	})
	for _, ext := range extensions {
		matches := byExtension[ext]
		sort.Strings(matches)
		for _, m := range matches {
			r, err := Quality(m, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: analysis failed: %v\n", err)
				failed = append(failed, m)
				continue
			}
			out = append(out, r)
		}
	}
	return out, failed
}

// writeJSON matches Python's json.dumps(v, indent=2): ensure_ascii (\uXXXX for non-ASCII, surrogate
// pairs above U+FFFF) and no HTML-escaping of <, >, & (Go's default does the opposite on both counts).
func writeJSON(w io.Writer, v any) error {
	b, err := PyMarshalIndent(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
