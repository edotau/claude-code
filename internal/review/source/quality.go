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
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

// thresholds mirrors quality_core.THRESHOLDS.
var qualityThresholds = map[string]int{
	"long_function_lines": 50,
	"too_many_parameters": 5,
	"high_complexity":     10,
	"god_class_methods":   20,
	"max_imports":         15,
}

// Regexes compile lazily (sync.OnceValue): this binary runs once per hook/tool invocation.
var functionPatterns = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`def\s+(\w+)\s*\(([^)]*)\)`),
		"typescript": regexp.MustCompile(`(?:function\s+(\w+)|(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\([^)]*\)\s*=>)`),
		"javascript": regexp.MustCompile(`(?:function\s+(\w+)|(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\([^)]*\)\s*=>)`),
		"go":         regexp.MustCompile(`func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(([^)]*)\)`),
		"swift":      regexp.MustCompile(`func\s+(\w+)\s*\(([^)]*)\)`),
		"kotlin":     regexp.MustCompile(`fun\s+(\w+)\s*\(([^)]*)\)`),
	}
})

var classPatterns = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python":     regexp.MustCompile(`class\s+(\w+)`),
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
	return regexp.MustCompile(`(?i)\bif\b|\belif\b|\belse\b|\bfor\b|\bwhile\b|\bcase\b|\bcatch\b|\bexcept\b|\band\b|\bor\b|\|\||&&`)
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
	return regexp.MustCompile(`(?i)^\s*[#/]+\s*(if|for|while|def|function|class|const|let|var)\s`)
})
var magicNumberDigits = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\d{3,}`)
})

// detectLanguage mirrors quality_core.detect_language.
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	for _, lang := range languageOrder {
		for _, e := range Extensions[lang] {
			if e == ext {
				return lang
			}
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

// firstNonEmpty picks match.groups()'s first truthy value, same as Python's `next(g for g in ... if g)`.
func firstNonEmpty(groups []string) string {
	for _, g := range groups {
		if g != "" {
			return g
		}
	}
	return "anonymous"
}

// findFunctions ports quality_core.find_functions, including its group(2)-as-params quirk for TS/JS.
func findFunctions(content, language string) []FunctionInfo {
	pattern, ok := functionPatterns()[language]
	if !ok {
		pattern = functionPatterns()["python"]
	}
	var out []FunctionInfo
	for _, loc := range pattern.FindAllStringSubmatchIndex(content, -1) {
		groups := submatchStrings(content, loc, pattern)
		name := firstNonEmpty(groups)
		paramsStr := ""
		if len(groups) > 1 && groups[1] != "" {
			paramsStr = groups[1]
		}
		params := splitParams(paramsStr)
		matchEnd := loc[1]
		body := functionBody(content, matchEnd, pattern)
		out = append(out, FunctionInfo{
			Name:       name,
			Parameters: len(params),
			Lines:      len(strings.Split(body, "\n")),
			Complexity: calcCyclomaticComplexity(body),
		})
	}
	return out
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
	for _, p := range strings.Split(s, ",") {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// functionBody spans from matchEnd to the next match of pattern, or 2000 runes if there is none.
func functionBody(content string, matchEnd int, pattern *regexp.Regexp) string {
	remaining := content[matchEnd:]
	if loc := pattern.FindStringIndex(remaining); loc != nil {
		return remaining[:loc[0]]
	}
	// Python slices by rune (str index), not bytes; 2000 must be a rune count too.
	runes := []rune(remaining)
	if len(runes) > 2000 {
		return string(runes[:2000])
	}
	return remaining
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
		remaining := content[m[1]:]
		body := remaining
		if loc := pattern.FindStringIndex(remaining); loc != nil {
			body = remaining[:loc[0]]
		}
		out = append(out, ClassInfo{
			Name:    name,
			Methods: len(methodPattern.FindAllString(body, -1)),
			Lines:   len(strings.Split(body, "\n")),
		})
	}
	return out
}

// magicNumberOnLine ports the `\b(?<![.\"\'])\d{3,}\b(?!\.\d)` lookaround via manual boundary checks (RE2 has none).
func magicNumberOnLine(line string) (string, bool) {
	for _, loc := range magicNumberDigits().FindAllStringIndex(line, -1) {
		start, end := loc[0], loc[1]
		if start > 0 {
			prev := line[start-1]
			if isWordByte(prev) || prev == '.' || prev == '"' || prev == '\'' {
				continue
			}
		}
		if end < len(line) {
			next := line[end]
			if isWordByte(next) {
				continue
			}
			if next == '.' && end+1 < len(line) && line[end+1] >= '0' && line[end+1] <= '9' {
				continue
			}
		}
		return line[start:end], true
	}
	return "", false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// checkCodeSmells ports quality_core.check_code_smells.
func checkCodeSmells(content string, functions []FunctionInfo, classes []ClassInfo) []Smell {
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
	if lm.Total > 0 {
		ratio := float64(lm.Comment) / float64(lm.Total)
		if ratio >= 0.1 && ratio <= 0.3 {
			score += 5
		}
	}
	if len(functions) > 0 {
		total := 0
		for _, f := range functions {
			total += f.Lines
		}
		if float64(total)/float64(len(functions)) < 30 {
			score += 5
		}
	}
	if score > 100 {
		score = 100
	}
	if score < 0 {
		score = 0
	}
	return score
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
			return QualityReport{}, fmt.Errorf("Unsupported file type: %s", filepath.Ext(path))
		}
	}
	data, readErr := os.ReadFile(path)
	content := ""
	if readErr == nil {
		content = string(data)
	}
	if content == "" {
		return QualityReport{}, fmt.Errorf("Could not read file: %s", path)
	}

	lineMetrics := countLines(content)
	functions := findFunctions(content, lang)
	classes := findClasses(content, lang)
	smells := checkCodeSmells(content, functions, classes)
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
		Smells:          nonNilSmells(smells),
		SolidViolations: nonNilViolations(violations),
		FunctionDetails: capFunctions(functions, 10),
		ClassDetails:    capClasses(classes, 10),
	}, nil
}

// nonNilSmells/nonNilViolations keep the JSON output "[]" instead of "null" for empty slices.
func nonNilSmells(s []Smell) []Smell {
	if s == nil {
		return []Smell{}
	}
	return s
}
func nonNilViolations(v []SolidViolation) []SolidViolation {
	if v == nil {
		return []SolidViolation{}
	}
	return v
}
func capFunctions(f []FunctionInfo, n int) []FunctionInfo {
	if f == nil {
		return []FunctionInfo{}
	}
	if len(f) > n {
		return f[:n]
	}
	return f
}
func capClasses(c []ClassInfo, n int) []ClassInfo {
	if c == nil {
		return []ClassInfo{}
	}
	if len(c) > n {
		return c[:n]
	}
	return c
}

var skipParts = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".venv": true, "venv": true, "dist": true, "build": true}

// pathHasSkippedPart mirrors `SKIP_PARTS & set(filepath.parts)`.
func pathHasSkippedPart(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if skipParts[part] {
			return true
		}
	}
	return false
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

	var results []QualityReport
	for _, ext := range extensions {
		results = append(results, walkExtension(dir, ext)...)
	}
	if len(results) == 0 {
		return directoryReport{}, fmt.Errorf("No supported files found")
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
		TotalCodeSmells: smells, TotalSolidViolations: violations, Files: results,
	}, nil
}

// walkExtension collects files under dir named *ext (recursive), matching Path.glob(f"**/*{ext}").
func walkExtension(dir, ext string) []QualityReport {
	var matches []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || pathHasSkippedPart(p) {
			return nil
		}
		if strings.HasSuffix(p, ext) {
			matches = append(matches, p)
		}
		return nil
	})
	sort.Strings(matches)
	var out []QualityReport
	for _, m := range matches {
		if r, err := Quality(m, ""); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// writeJSON matches Python's json.dumps(v, indent=2): ensure_ascii (\uXXXX for non-ASCII, surrogate
// pairs above U+FFFF) and no HTML-escaping of <, >, & (Go's default does the opposite on both counts).
func writeJSON(w io.Writer, v interface{}) error {
	b, err := marshalPyJSON(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func marshalPyJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	compact := bytes.TrimRight(buf.Bytes(), "\n")
	escaped := escapeNonASCII(compact)
	var out bytes.Buffer
	if err := json.Indent(&out, escaped, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func escapeNonASCII(b []byte) []byte {
	var out bytes.Buffer
	for _, r := range string(b) {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", r1, r2)
		} else {
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.Bytes()
}
