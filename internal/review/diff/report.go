package diff

import (
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func calculateReviewScore(pr *prInput, quality *qualityInput) int {
	counts := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	for _, sev := range severityOrder {
		counts[sev] += len(pr.Risks[sev])
	}
	for _, issue := range extractQualityIssues(quality) {
		if _, ok := counts[issue.Severity]; ok {
			counts[issue.Severity]++
		}
	}

	score := 100
	for sev, count := range counts {
		score -= min(count*deductionWeights[sev], deductionCaps[sev])
	}
	score -= complexityDeduction(pr.Complexity)
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
		if score >= t.minScore && (t.maxHigh < 0 || highCount <= t.maxHigh) {
			return t.name, verdictRationales[t.name]
		}
	}
	return "block", "Significant issues prevent approval"
}

func generateFindingsList(pr *prInput, quality *qualityInput) []Finding {
	findings := []Finding{}
	for _, sev := range severityOrder {
		for _, k := range pr.Risks[sev] {
			findings = append(findings, Finding{
				Source: "pr_analysis", Severity: sev, Category: orDefault(k.Name, "unknown"),
				Message: k.Message, File: k.File, Count: new(k.Count),
			})
		}
	}
	for _, issue := range extractQualityIssues(quality) {
		findings = append(findings, Finding{
			Source: "quality_analysis", Severity: issue.Severity, Category: orDefault(issue.Type, "unknown"),
			Message: issue.Message, File: issue.File, Line: new(issue.Line),
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
	idx := map[string]int{}
	for _, f := range findings {
		i, ok := idx[f.Category]
		if !ok {
			if len(items) >= 15 {
				continue
			}
			idx[f.Category] = len(items)
			items = append(items, ActionItem{Action: actionForFinding(f.Category, f.Message), FilesAffected: []string{}})
			i = len(items) - 1
		}
		it := &items[i]
		if sevRank(f.Severity) > sevRank(it.Severity) || it.Severity == "" {
			it.Severity, it.Priority = f.Severity, priorityFor(f.Severity)
		}
		if f.File != "" && !slices.Contains(it.FilesAffected, f.File) {
			it.FilesAffected = append(it.FilesAffected, f.File)
		}
	}
	return items
}

// priorityFor maps a severity to P0 (critical), P1 (high) or P2.
func priorityFor(sev string) string {
	switch sev {
	case "critical":
		return "P0"
	case "high":
		return "P1"
	}
	return "P2"
}

// sevRank orders severities critical > high > anything else.
func sevRank(sev string) int {
	switch sev {
	case "critical":
		return 2
	case "high":
		return 1
	}
	return 0
}

// loadPRAnalysis returns the override when given, else runs the PR analysis.
func loadPRAnalysis(repoPath, base, head string, override *prInput) (*prInput, error) {
	if override != nil {
		return override, nil
	}
	r, err := analyzePR(repoPath, base, head)
	if err != nil {
		return nil, err
	}
	return prInputOf(r), nil
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

// fillPRSummary sets the report's PR summary and review order from an analyzed PR analysis.
func fillPRSummary(report *Report, pr *prInput) error {
	if pr.Status != "analyzed" {
		return nil
	}
	if pr.summaryErr != nil {
		return pr.summaryErr
	}
	report.PRSummary = pr.Summary
	report.ReviewOrder = pr.ReviewOrder
	return nil
}

// generateReport ports generate_report; the overrides are non-nil when loaded from --pr-analysis/--quality-analysis.
func generateReport(repoPath, base, head string, prOverride *prInput, qualityOverride *qualityInput) (Report, error) {
	pr, err := loadPRAnalysis(repoPath, base, head, prOverride)
	if err != nil {
		return Report{}, err
	}

	quality := qualityOverride
	if quality == nil {
		var qErr error
		if quality, qErr = runQualityChecker(repoPath, base, head); qErr != nil {
			return Report{}, qErr
		}
	}

	findings := generateFindingsList(pr, quality)
	counts := countSeverities(findings)
	score := calculateReviewScore(pr, quality)
	verdict, rationale := determineVerdict(score, counts.Critical, counts.High)
	actionItems := generateActionItems(findings)

	report := Report{
		Metadata: ReportMetadata{GeneratedAt: time.Now().Format("2006-01-02T15:04:05.000000"), Repository: repoPath, Version: "1.0.0"},
		Summary:  ReportSummary{Score: score, Verdict: verdict, Rationale: rationale, IssueCounts: counts},
		Findings: findings, ActionItems: actionItems,
	}
	if err := fillPRSummary(&report, pr); err != nil {
		return Report{}, err
	}

	report.QualitySummary = quality.Summary
	return report, nil
}

// reportOpts are RunReport's parsed flags and the repo argument.
type reportOpts struct {
	repoArgs                        []string
	base, head, prPath, qualityPath string
	format, output                  string
}

// RunReport ports review_report_generator.py's CLI.
func RunReport(args []string, stdout, stderr io.Writer) int {
	fs := helptext.NewFlagSet("report", helptext.Page{
		Usage: "claude-code review report [<repo>] [flags]",
		About: "Combine the PR analysis and code-quality findings into one review report.",
	})
	fs.SetOutput(stderr)
	base := fs.String("base", "main", "Base branch for comparison")
	fs.StringVar(base, "b", "main", "shorthand for --base")
	head := fs.String("head", "HEAD", "Head branch/commit for comparison")
	prPath := fs.String("pr-analysis", "", "load the PR analysis from this JSON file instead of running it")
	qualityPath := fs.String("quality-analysis", "", "load the quality analysis from this JSON file")
	format := fs.String("format", "text", "text|markdown|json")
	fs.StringVar(format, "f", "text", "shorthand for --format")
	output := fs.String("output", "", "Write output to file")
	fs.StringVar(output, "o", "", "shorthand for --output")
	jsonOut := fs.Bool("json", false, "Output in JSON format (same as --format json)")
	positional, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *jsonOut {
		*format = "json"
	}
	o := reportOpts{positional, *base, *head, *prPath, *qualityPath, *format, *output}
	return runReport(o, stdout, stderr)
}

// runReport validates the options, builds the report and writes it in the chosen format.
func runReport(o reportOpts, stdout, stderr io.Writer) int {
	if !slices.Contains(reportFormats, o.format) {
		fmt.Fprintf(stderr, "claude-code: unknown --format %q; use one of %s\n", o.format, strings.Join(reportFormats, ", "))
		return 2
	}
	if err := checkRefs(o.base, o.head); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	repoPath, ok := resolveExistingPath(o.repoArgs, stderr)
	if !ok {
		return 1
	}
	prOverride, qualityOverride, code := loadOverrides(o.prPath, o.qualityPath, stderr)
	if code != 0 {
		return code
	}
	if prOverride == nil || qualityOverride == nil {
		if _, ok := resolveGitRepo([]string{repoPath}, stderr); !ok {
			return 1
		}
	}
	report, err := generateReport(repoPath, o.base, o.head, prOverride, qualityOverride)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	out, err := renderReport(report, o.format)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return writeOutput(out, true, o.output, "Report", stdout, stderr)
}

// resolveExistingPath returns the absolute path of the first positional (default "."), which must exist.
func resolveExistingPath(positional []string, stderr io.Writer) (string, bool) {
	repoPath, ok := absRepoArg(positional, stderr)
	if !ok {
		return "", false
	}
	if _, statErr := os.Stat(repoPath); statErr != nil {
		fmt.Fprintf(stderr, "Error: Path does not exist: %s\n", repoPath)
		return "", false
	}
	return repoPath, true
}

// loadOverrides loads the optional --pr-analysis/--quality-analysis files; an unreadable one refuses with exit 2.
func loadOverrides(prPath, qualityPath string, stderr io.Writer) (pr *prInput, quality *qualityInput, code int) {
	refuse := func(label, path, flag string, err error) int {
		fmt.Fprintf(stderr, "claude-code: cannot load %s from %s: %v; fix the file or drop %s to run it fresh\n", label, path, err, flag)
		return 2
	}
	var err error
	if prPath != "" {
		if pr, err = loadPRAnalysisFile(prPath); err != nil {
			return nil, nil, refuse("PR analysis", prPath, "--pr-analysis", err)
		}
	}
	if qualityPath != "" {
		if quality, err = loadQualityAnalysisFile(qualityPath); err != nil {
			return nil, nil, refuse("quality analysis", qualityPath, "--quality-analysis", err)
		}
	}
	return pr, quality, 0
}

// reportFormats are the values --format accepts.
var reportFormats = []string{"text", "markdown", "json"}

func renderReport(report Report, format string) (string, error) {
	switch format {
	case "json":
		b, err := source.PyMarshalIndent(report)
		return string(b), err
	case "markdown":
		return formatMarkdownReport(report), nil
	}
	return formatTextReport(report), nil
}
