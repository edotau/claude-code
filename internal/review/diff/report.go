package diff

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

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

// generateReport ports generate_report; prAnalysisOverride/qualityAnalysisOverride are non-nil
// when loaded from --pr-analysis/--quality-analysis JSON files.
func generateReport(repoPath, base, head string, prAnalysisOverride, qualityAnalysisOverride map[string]any) (Report, error) {
	var prReport *PRReport
	var prMap map[string]any
	if prAnalysisOverride != nil {
		prMap = prAnalysisOverride
	} else {
		r, err := AnalyzePR(repoPath, base, head)
		if err != nil {
			return Report{}, err
		}
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
			b, err := json.Marshal(summary)
			if err != nil {
				return Report{}, fmt.Errorf("marshal PR analysis summary: %w", err)
			}
			var ps PRSummary
			if err := json.Unmarshal(b, &ps); err != nil {
				return Report{}, fmt.Errorf("decode PR analysis summary: %w", err)
			}
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

	repoArg := "."
	if len(positional) > 0 {
		repoArg = positional[0]
	}
	repoPath, err := filepath.Abs(repoArg)
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
			fmt.Fprintf(stderr, "Warning: Could not load PR analysis from %s\n", *prAnalysisPath)
		} else {
			prOverride = m
		}
	}
	if *qualityAnalysisPath != "" {
		m, ok := loadJSONFile(*qualityAnalysisPath)
		if !ok {
			fmt.Fprintf(stderr, "Warning: Could not load quality analysis from %s\n", *qualityAnalysisPath)
		} else {
			qualityOverride = m
		}
	}

	report, err := generateReport(repoPath, *base, *head, prOverride, qualityOverride)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

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
