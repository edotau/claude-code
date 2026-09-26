package diff

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/review/source"
)

var severityWeights = map[string]int{"critical": 100, "high": 75, "medium": 50, "low": 25, "info": 10}

type verdictThreshold struct {
	name        string
	minScore    int
	maxCritical int
	maxHigh     int // -1 means unbounded (Python's float("inf"))
}

// verdictThresholds is checked in order; first match wins (mirrors VERDICT_THRESHOLDS dict order).
var verdictThresholds = []verdictThreshold{
	{"approve", 90, 0, 0},
	{"approve_with_suggestions", 75, 0, 2},
	{"request_changes", 50, 0, -1},
}

var verdictRationales = map[string]string{
	"approve":                  "Code meets quality standards",
	"approve_with_suggestions": "Minor improvements recommended",
	"request_changes":          "Several issues need to be addressed",
}

var deductionWeights = map[string]int{"critical": 15, "high": 8, "medium": 3, "low": 1}
var deductionCaps = map[string]int{"critical": 60, "high": 40, "medium": 20, "low": 10}
var severityOrder = []string{"critical", "high", "medium", "low"}

// Finding is a report line item; Count is set for pr_analysis risks, Line for quality issues
// (never both) — pointers so the omitted one matches Python's absent dict key exactly.
type Finding struct {
	Source   string  `json:"source"`
	Severity string  `json:"severity"`
	Category string  `json:"category"`
	Message  string  `json:"message"`
	File     string  `json:"file"`
	Count    *int    `json:"count,omitempty"`
	Line     *string `json:"line,omitempty"`
}

// ActionItem is a prioritized, deduped recommendation derived from findings.
type ActionItem struct {
	Priority      string   `json:"priority"`
	Action        string   `json:"action"`
	Severity      string   `json:"severity"`
	FilesAffected []string `json:"files_affected"`
}

// ReportIssueCounts buckets finding counts by severity for the summary block.
type ReportIssueCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// ReportSummary is the review_report_generator.py "summary" block.
type ReportSummary struct {
	Score       int               `json:"score"`
	Verdict     string            `json:"verdict"`
	Rationale   string            `json:"rationale"`
	IssueCounts ReportIssueCounts `json:"issue_counts"`
}

// ReportMetadata is the review_report_generator.py "metadata" block.
type ReportMetadata struct {
	GeneratedAt string `json:"generated_at"`
	Repository  string `json:"repository"`
	Version     string `json:"version"`
}

// QualitySummary re-derives files_analyzed/average_score/etc. in a fixed key order.
type QualitySummary struct {
	FilesAnalyzed        int     `json:"files_analyzed"`
	AverageScore         pyFloat `json:"average_score"`
	TotalCodeSmells      int     `json:"total_code_smells"`
	TotalSolidViolations int     `json:"total_solid_violations"`
}

// Report is the full review_report_generator.py output shape.
type Report struct {
	Metadata       ReportMetadata  `json:"metadata"`
	Summary        ReportSummary   `json:"summary"`
	Findings       []Finding       `json:"findings"`
	ActionItems    []ActionItem    `json:"action_items"`
	PRSummary      *PRSummary      `json:"pr_summary,omitempty"`
	ReviewOrder    []string        `json:"review_order,omitempty"`
	QualitySummary *QualitySummary `json:"quality_summary,omitempty"`
}

func loadJSONFile(path string) (map[string]any, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}

func toGenericMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func mStr(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// mFloat handles both json.Unmarshal's float64 and native int/float64 from our own runQualityChecker map.
func mFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return 0
	}
}

func mInt(m map[string]any, key string) int {
	return int(mFloat(m, key))
}

func mSlice(m map[string]any, key string) []any {
	if s, ok := m[key].([]any); ok {
		return s
	}
	return nil
}

func mMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// getChangedSourceFiles ports get_changed_source_files: git diff --name-only, filtered to
// existing files whose extension appears in source.Extensions.
func getChangedSourceFiles(repo, base, head string) []string {
	exts := map[string]bool{}
	for _, list := range source.Extensions {
		for _, e := range list {
			exts[strings.ToLower(e)] = true
		}
	}
	tryArgs := [][]string{{base + "..." + head}, {base, head}}
	for _, extra := range tryArgs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		full := append([]string{"-C", repo, "diff", "--name-only"}, extra...)
		out, err := execCommand(ctx, "git", full...)
		cancel()
		if err != nil {
			continue
		}
		var files []string
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			ext := extAt(line)
			if !exts[ext] {
				continue
			}
			full := repo + "/" + line
			if st, statErr := os.Stat(full); statErr == nil && !st.IsDir() {
				files = append(files, full)
			}
		}
		return files
	}
	return nil
}

func extAt(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx == -1 {
		return ""
	}
	return strings.ToLower(path[idx:])
}

func languageForExt(ext string) string {
	for lang, exts := range source.Extensions {
		for _, e := range exts {
			if strings.ToLower(e) == ext {
				return lang
			}
		}
	}
	return ""
}

// runQualityChecker ports run_quality_checker: source.Quality over each changed source file.
func runQualityChecker(repo, base, head string) map[string]any {
	var results []map[string]any
	for _, path := range getChangedSourceFiles(repo, base, head) {
		lang := languageForExt(extAt(path))
		qr, err := source.Quality(path, lang)
		if err != nil {
			continue
		}
		m := toGenericMap(qr)
		if _, isErr := m["error"]; isErr {
			continue
		}
		results = append(results, m)
	}

	avg := 100.0
	totalSmells, totalViolations := 0, 0
	if len(results) > 0 {
		sum := 0.0
		for _, r := range results {
			sum += mFloat(r, "quality_score")
			totalSmells += len(mSlice(r, "smells"))
			totalViolations += len(mSlice(r, "solid_violations"))
		}
		avg = roundTo(sum/float64(len(results)), 1)
	}
	filesAny := make([]any, len(results))
	for i, r := range results {
		filesAny[i] = r
	}
	return map[string]any{
		"files_analyzed":         len(results),
		"average_score":          avg,
		"total_code_smells":      totalSmells,
		"total_solid_violations": totalViolations,
		"files":                  filesAny,
	}
}

// extractQualityIssues flattens quality_analysis["files"] (or itself, single-file shape) smells
// and solid_violations into a uniform {severity,type,message,file,line} list.
func extractQualityIssues(qualityAnalysis map[string]any) []map[string]string {
	var issues []map[string]string
	fileResults := mSlice(qualityAnalysis, "files")
	var frs []map[string]any
	if fileResults != nil {
		for _, f := range fileResults {
			if m := asMap(f); m != nil {
				frs = append(frs, m)
			}
		}
	} else {
		frs = []map[string]any{qualityAnalysis}
	}
	for _, fr := range frs {
		path := mStr(fr, "file")
		for _, s := range mSlice(fr, "smells") {
			sm := asMap(s)
			severity := mStr(sm, "severity")
			if severity == "" {
				severity = "medium"
			}
			typ := mStr(sm, "type")
			if typ == "" {
				typ = "smell"
			}
			issues = append(issues, map[string]string{
				"severity": severity, "type": typ, "message": mStr(sm, "message"), "file": path, "line": mStr(sm, "location"),
			})
		}
		for _, v := range mSlice(fr, "solid_violations") {
			vm := asMap(v)
			severity := mStr(vm, "severity")
			if severity == "" {
				severity = "medium"
			}
			principle := mStr(vm, "principle")
			if principle == "" {
				principle = "solid"
			}
			issues = append(issues, map[string]string{
				"severity": severity, "type": principle, "message": mStr(vm, "message"), "file": path, "line": "",
			})
		}
	}
	return issues
}

func calculateReviewScore(prAnalysis, qualityAnalysis map[string]any) int {
	counts := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	risks := mMap(prAnalysis, "risks")
	for _, sev := range severityOrder {
		if risks != nil {
			counts[sev] += len(mSlice(risks, sev))
		}
	}
	for _, issue := range extractQualityIssues(qualityAnalysis) {
		sev := issue["severity"]
		if _, ok := counts[sev]; ok {
			counts[sev]++
		}
	}

	score := 100
	for sev, count := range counts {
		d := count * deductionWeights[sev]
		if cap := deductionCaps[sev]; d > cap {
			d = cap
		}
		score -= d
	}

	if summary := mMap(prAnalysis, "summary"); summary != nil {
		complexity := mInt(summary, "complexity_score")
		if complexity > 7 {
			score -= 10
		} else if complexity > 5 {
			score -= 5
		}
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

func determineVerdict(score, criticalCount, highCount int) (string, string) {
	if criticalCount > 0 {
		return "block", "Critical issues must be resolved before merge"
	}
	for _, t := range verdictThresholds {
		if score >= t.minScore && criticalCount <= t.maxCritical && (t.maxHigh < 0 || highCount <= t.maxHigh) {
			return t.name, verdictRationales[t.name]
		}
	}
	return "block", "Significant issues prevent approval"
}

func intPtr(i int) *int       { return &i }
func strPtr(s string) *string { return &s }

func generateFindingsList(prAnalysis, qualityAnalysis map[string]any) []Finding {
	findings := []Finding{}
	if risks := mMap(prAnalysis, "risks"); risks != nil {
		for _, sev := range severityOrder {
			for _, item := range mSlice(risks, sev) {
				im := asMap(item)
				count := mInt(im, "count")
				if _, has := im["count"]; !has {
					count = 1
				}
				findings = append(findings, Finding{
					Source: "pr_analysis", Severity: sev, Category: orDefault(mStr(im, "name"), "unknown"),
					Message: mStr(im, "message"), File: mStr(im, "file"), Count: intPtr(count),
				})
			}
		}
	}
	for _, issue := range extractQualityIssues(qualityAnalysis) {
		findings = append(findings, Finding{
			Source: "quality_analysis", Severity: issue["severity"], Category: orDefault(issue["type"], "unknown"),
			Message: issue["message"], File: issue["file"], Line: strPtr(issue["line"]),
		})
	}
	sort.SliceStable(findings, func(i, j int) bool {
		return severityWeights[findings[i].Severity] > severityWeights[findings[j].Severity]
	})
	return findings
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var actionForCategory = map[string]string{
	"hardcoded_secrets":      "Remove hardcoded credentials and use environment variables or a secrets manager",
	"sql_concatenation":      "Use parameterized queries to prevent SQL injection",
	"debugger":               "Remove debugger statements before merging",
	"console_log":            "Remove or replace console statements with proper logging",
	"todo_fixme":             "Address TODO/FIXME comments or create tracking issues",
	"disable_eslint":         "Address the underlying issue instead of disabling lint rules",
	"any_type":               "Replace 'any' types with proper type definitions",
	"long_function":          "Break down function into smaller, focused units",
	"god_class":              "Split class into smaller, single-responsibility classes",
	"too_many_parameters":    "Use parameter objects or builder pattern",
	"deep_nesting":           "Refactor using early returns, guard clauses, or extraction",
	"high_complexity":        "Reduce cyclomatic complexity through refactoring",
	"missing_error_handling": "Add proper error handling and recovery logic",
	"duplicate_code":         "Extract duplicate code into shared functions",
	"magic_number":           "Replace magic numbers with named constants",
	"commented_code":         "Remove commented-out code (version control preserves history)",
	"large_file":             "Consider splitting into multiple smaller modules",
}

func actionForFinding(category, message string) string {
	if a, ok := actionForCategory[category]; ok {
		return a
	}
	if message == "" {
		message = category
	}
	return "Review and address: " + message
}

func generateActionItems(findings []Finding) []ActionItem {
	items := []ActionItem{}
	seen := map[string]bool{}
	for _, f := range findings {
		if seen[f.Category] {
			continue
		}
		priority := "P2"
		if f.Severity == "critical" {
			priority = "P0"
		} else if f.Severity == "high" {
			priority = "P1"
		}
		filesAffected := []string{}
		if f.File != "" {
			filesAffected = []string{f.File}
		}
		items = append(items, ActionItem{Priority: priority, Action: actionForFinding(f.Category, f.Message), Severity: f.Severity, FilesAffected: filesAffected})
		seen[f.Category] = true
		if len(items) >= 15 {
			break
		}
	}
	return items
}

// generateReport ports generate_report; prAnalysisOverride/qualityAnalysisOverride are non-nil
// when loaded from --pr-analysis/--quality-analysis JSON files.
func generateReport(repoPath, base, head string, prAnalysisOverride, qualityAnalysisOverride map[string]any) Report {
	var prReport *PRReport
	var prMap map[string]any
	if prAnalysisOverride != nil {
		prMap = prAnalysisOverride
	} else {
		r, _ := AnalyzePR(repoPath, base, head)
		prReport = &r
		prMap = toGenericMap(r)
	}

	var qualityMap map[string]any
	if qualityAnalysisOverride != nil {
		qualityMap = qualityAnalysisOverride
	} else {
		qualityMap = runQualityChecker(repoPath, base, head)
	}

	findings := generateFindingsList(prMap, qualityMap)
	counts := ReportIssueCounts{}
	for _, f := range findings {
		switch f.Severity {
		case "critical":
			counts.Critical++
		case "high":
			counts.High++
		case "medium":
			counts.Medium++
		case "low":
			counts.Low++
		}
	}

	score := calculateReviewScore(prMap, qualityMap)
	verdict, rationale := determineVerdict(score, counts.Critical, counts.High)
	actionItems := generateActionItems(findings)

	report := Report{
		Metadata: ReportMetadata{GeneratedAt: time.Now().Format("2006-01-02T15:04:05.000000"), Repository: repoPath, Version: "1.0.0"},
		Summary:  ReportSummary{Score: score, Verdict: verdict, Rationale: rationale, IssueCounts: counts},
		Findings: findings, ActionItems: actionItems,
	}

	if mStr(prMap, "status") == "analyzed" {
		if prReport != nil {
			report.PRSummary = prReport.Summary
			report.ReviewOrder = prReport.ReviewOrder
		} else if summary := mMap(prMap, "summary"); summary != nil {
			b, _ := json.Marshal(summary)
			var ps PRSummary
			json.Unmarshal(b, &ps)
			report.PRSummary = &ps
			ro := []string{}
			for _, v := range mSlice(prMap, "review_order") {
				if s, ok := v.(string); ok {
					ro = append(ro, s)
				}
			}
			report.ReviewOrder = ro
		}
	}

	if _, hasFiles := qualityMap["files"]; hasFiles {
		report.QualitySummary = &QualitySummary{
			FilesAnalyzed: mInt(qualityMap, "files_analyzed"), AverageScore: pyFloat(mFloat(qualityMap, "average_score")),
			TotalCodeSmells: mInt(qualityMap, "total_code_smells"), TotalSolidViolations: mInt(qualityMap, "total_solid_violations"),
		}
	} else if _, hasFile := qualityMap["file"]; hasFile {
		report.QualitySummary = &QualitySummary{
			FilesAnalyzed: mInt(qualityMap, "files_analyzed"), AverageScore: pyFloat(mFloat(qualityMap, "average_score")),
			TotalCodeSmells: mInt(qualityMap, "total_code_smells"), TotalSolidViolations: mInt(qualityMap, "total_solid_violations"),
		}
	}

	return report
}

// RunReport ports review_report_generator.py's CLI.
func RunReport(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, map[string]bool{"json": true})
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "main", "")
	fs.StringVar(base, "b", "main", "")
	head := fs.String("head", "HEAD", "")
	prAnalysisPath := fs.String("pr-analysis", "", "")
	qualityAnalysisPath := fs.String("quality-analysis", "", "")
	format := fs.String("format", "text", "")
	fs.StringVar(format, "f", "text", "")
	output := fs.String("output", "", "")
	fs.StringVar(output, "o", "", "")
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}

	repoArg := "."
	if len(positional) > 0 {
		repoArg = positional[0]
	}
	repoPath, err := absPath(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if _, statErr := os.Stat(repoPath); statErr != nil {
		fmt.Fprintf(stderr, "Error: Path does not exist: %s\n", repoPath)
		return 1
	}

	var prOverride, qualityOverride map[string]any
	if *prAnalysisPath != "" {
		m, ok := loadJSONFile(*prAnalysisPath)
		if !ok {
			fmt.Fprintf(stdout, "Warning: Could not load PR analysis from %s\n", *prAnalysisPath)
		} else {
			prOverride = m
		}
	}
	if *qualityAnalysisPath != "" {
		m, ok := loadJSONFile(*qualityAnalysisPath)
		if !ok {
			fmt.Fprintf(stdout, "Warning: Could not load quality analysis from %s\n", *qualityAnalysisPath)
		} else {
			qualityOverride = m
		}
	}

	report := generateReport(repoPath, *base, *head, prOverride, qualityOverride)

	outputFormat := *format
	if *jsonOut {
		outputFormat = "json"
	}

	var out string
	switch outputFormat {
	case "json":
		out = mustIndentJSON(report)
	case "markdown":
		out = formatMarkdownReport(report)
	default:
		out = formatTextReport(report)
	}

	if *output != "" {
		if err := os.WriteFile(*output, []byte(out), 0o644); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Report written to %s\n", *output)
	} else {
		fmt.Fprintln(stdout, out)
	}
	return 0
}
