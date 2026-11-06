package diff

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

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
		score -= min(count*deductionWeights[sev], deductionCaps[sev])
	}
	if summary := mMap(prAnalysis, "summary"); summary != nil {
		score -= complexityDeduction(mInt(summary, "complexity_score"))
	}
	return min(max(score, 0), 100)
}

func complexityDeduction(complexity int) int {
	switch {
	case complexity > 7:
		return 10
	case complexity > 5:
		return 5
	}
	return 0
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
					Message: mStr(im, "message"), File: mStr(im, "file"), Count: new(count),
				})
			}
		}
	}
	for _, issue := range extractQualityIssues(qualityAnalysis) {
		findings = append(findings, Finding{
			Source: "quality_analysis", Severity: issue["severity"], Category: orDefault(issue["type"], "unknown"),
			Message: issue["message"], File: issue["file"], Line: new(issue["line"]),
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
		switch f.Severity {
		case "critical":
			priority = "P0"
		case "high":
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

// loadPRAnalysis returns the PR analysis map; prReport is non-nil only when the analysis ran here (no override).
func loadPRAnalysis(repoPath, base, head string, override map[string]any) (*PRReport, map[string]any, error) {
	if override != nil {
		return nil, override, nil
	}
	r, err := AnalyzePR(repoPath, base, head)
	if err != nil {
		return nil, nil, err
	}
	return &r, toGenericMap(r), nil
}

func countSeverities(findings []Finding) ReportIssueCounts {
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
	return counts
}

// fillPRSummary sets the report's PR summary and review order from the fresh analysis or the loaded JSON map.
func fillPRSummary(report *Report, prMap map[string]any, prReport *PRReport) error {
	if mStr(prMap, "status") != "analyzed" {
		return nil
	}
	if prReport != nil {
		report.PRSummary = prReport.Summary
		report.ReviewOrder = prReport.ReviewOrder
		return nil
	}
	summary := mMap(prMap, "summary")
	if summary == nil {
		return nil
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("marshal PR analysis summary: %w", err)
	}
	var ps PRSummary
	if err := json.Unmarshal(b, &ps); err != nil {
		return fmt.Errorf("decode PR analysis summary: %w", err)
	}
	report.PRSummary = &ps
	ro := []string{}
	for _, v := range mSlice(prMap, "review_order") {
		if s, ok := v.(string); ok {
			ro = append(ro, s)
		}
	}
	report.ReviewOrder = ro
	return nil
}

// generateReport ports generate_report; prAnalysisOverride/qualityAnalysisOverride are non-nil
// when loaded from --pr-analysis/--quality-analysis JSON files.
func generateReport(repoPath, base, head string, prAnalysisOverride, qualityAnalysisOverride map[string]any) (Report, error) {
	prReport, prMap, err := loadPRAnalysis(repoPath, base, head, prAnalysisOverride)
	if err != nil {
		return Report{}, err
	}

	qualityMap := qualityAnalysisOverride
	if qualityMap == nil {
		qualityMap = runQualityChecker(repoPath, base, head)
	}

	findings := generateFindingsList(prMap, qualityMap)
	counts := countSeverities(findings)
	score := calculateReviewScore(prMap, qualityMap)
	verdict, rationale := determineVerdict(score, counts.Critical, counts.High)
	actionItems := generateActionItems(findings)

	report := Report{
		Metadata: ReportMetadata{GeneratedAt: time.Now().Format("2006-01-02T15:04:05.000000"), Repository: repoPath, Version: "1.0.0"},
		Summary:  ReportSummary{Score: score, Verdict: verdict, Rationale: rationale, IssueCounts: counts},
		Findings: findings, ActionItems: actionItems,
	}
	if err := fillPRSummary(&report, prMap, prReport); err != nil {
		return Report{}, err
	}

	_, hasFiles := qualityMap["files"]
	_, hasFile := qualityMap["file"]
	if hasFiles || hasFile {
		report.QualitySummary = &QualitySummary{
			FilesAnalyzed: mInt(qualityMap, "files_analyzed"), AverageScore: pyFloat(mFloat(qualityMap, "average_score")),
			TotalCodeSmells: mInt(qualityMap, "total_code_smells"), TotalSolidViolations: mInt(qualityMap, "total_solid_violations"),
		}
	}

	return report, nil
}

// RunReport ports review_report_generator.py's CLI.
func RunReport(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, map[string]bool{"json": true, "h": true, "help": true})
	fs := helptext.NewFlagSet("report", helptext.Page{
		Usage: "claude-code review report [<repo>] [flags]",
		About: "Combine the PR analysis and code-quality findings into one review report.",
	})
	fs.SetOutput(stderr)
	base := fs.String("base", "main", "Base branch for comparison")
	fs.StringVar(base, "b", "main", "shorthand for --base")
	head := fs.String("head", "HEAD", "Head branch/commit for comparison")
	prAnalysisPath := fs.String("pr-analysis", "", "load the PR analysis from this JSON file instead of running it")
	qualityAnalysisPath := fs.String("quality-analysis", "", "load the quality analysis from this JSON file")
	format := fs.String("format", "text", "text|markdown|json")
	fs.StringVar(format, "f", "text", "shorthand for --format")
	output := fs.String("output", "", "Write output to file")
	fs.StringVar(output, "o", "", "shorthand for --output")
	jsonOut := fs.Bool("json", false, "Output in JSON format (same as --format json)")
	if code, done := helptext.Parse(fs, flagArgs); done {
		return code
	}

	if err := checkRefs(*base, *head); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	repoPath, ok := resolveExistingPath(positional, stderr)
	if !ok {
		return 1
	}

	prOverride := loadAnalysisOverride(*prAnalysisPath, "PR analysis", stderr)
	qualityOverride := loadAnalysisOverride(*qualityAnalysisPath, "quality analysis", stderr)

	report, err := generateReport(repoPath, *base, *head, prOverride, qualityOverride)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	outputFormat := *format
	if *jsonOut {
		outputFormat = "json"
	}

	out, err := renderReport(report, outputFormat)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	return writeReport(out, *output, stdout, stderr)
}

// resolveExistingPath returns the absolute path of the first positional (default "."), which must exist.
func resolveExistingPath(positional []string, stderr io.Writer) (string, bool) {
	repoArg := "."
	if len(positional) > 0 {
		repoArg = positional[0]
	}
	repoPath, err := filepath.Abs(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", false
	}
	if _, statErr := os.Stat(repoPath); statErr != nil {
		fmt.Fprintf(stderr, "Error: Path does not exist: %s\n", repoPath)
		return "", false
	}
	return repoPath, true
}

func writeReport(out, output string, stdout, stderr io.Writer) int {
	if output == "" {
		fmt.Fprintln(stdout, out)
		return 0
	}
	if err := paths.AtomicWrite(output, []byte(out), 0o644); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Report written to %s\n", output)
	return 0
}

// loadAnalysisOverride loads an optional analysis JSON file, warning on stderr when it cannot be read.
func loadAnalysisOverride(path, label string, stderr io.Writer) map[string]any {
	if path == "" {
		return nil
	}
	m, ok := loadJSONFile(path)
	if !ok {
		fmt.Fprintf(stderr, "Warning: Could not load %s from %s\n", label, path)
		return nil
	}
	return m
}

func renderReport(report Report, format string) (string, error) {
	switch format {
	case "json":
		return indentJSON(report)
	case "markdown":
		return formatMarkdownReport(report), nil
	}
	return formatTextReport(report), nil
}
