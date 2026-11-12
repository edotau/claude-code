package diff

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/textutil"
)

func roundTo(x float64, places int) float64 {
	mult := math.Pow(10, float64(places))
	return math.Round(x*mult) / mult
}

func pyPercent(x float64) string {
	return fmt.Sprintf("%.0f%%", x*100)
}

var severityWeights = map[string]int{"critical": 100, "high": 75, "medium": 50, "low": 25, "info": 10}

type verdictThreshold struct {
	name     string
	minScore int
	maxHigh  int // -1 means unbounded (Python's float("inf"))
}

// verdictThresholds is checked in order; first match wins (mirrors VERDICT_THRESHOLDS dict order).
var verdictThresholds = []verdictThreshold{
	{"approve", 90, 0},
	{"approve_with_suggestions", 75, 2},
	{"request_changes", 50, -1},
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
	FilesAnalyzed        int            `json:"files_analyzed"`
	AverageScore         source.PyFloat `json:"average_score"`
	TotalCodeSmells      int            `json:"total_code_smells"`
	TotalSolidViolations int            `json:"total_solid_violations"`
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

// changedSourceFiles lists the source files added, copied, modified or renamed in base...head, relative to repo.
// -z keeps non-ASCII and space-containing names unquoted; deleted files are excluded (they have no head content).
func changedSourceFiles(repo, base, head string) ([]string, error) {
	exts := map[string]bool{}
	for _, list := range source.Extensions {
		for _, e := range list {
			exts[strings.ToLower(e)] = true
		}
	}
	out, err := refDiff(repo, base, head, []string{"-z", "--name-only", "--relative", "--diff-filter=ACMR"})
	if err != nil {
		return nil, fmt.Errorf("git diff %s...%s failed, no files reviewed: %w", base, head, err)
	}
	var files []string
	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" && exts[strings.ToLower(filepath.Ext(name))] {
			files = append(files, name)
		}
	}
	return files, nil
}

// headContent returns the bytes of the file name (relative to repo) at rev, so a score matches the reviewed commit.
func headContent(repo, rev, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return proc.RunGitRaw(ctx, repo, "show", rev+":./"+name)
}

// runQualityChecker ports run_quality_checker: source.QualityContent over each changed file as of the head commit.
func runQualityChecker(repo, base, head string) (*qualityInput, error) {
	changed, err := changedSourceFiles(repo, base, head)
	if err != nil {
		return nil, err
	}
	var reports []source.QualityReport
	for _, name := range changed {
		content, err := headContent(repo, head, name)
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, head, err)
		}
		qr, err := source.QualityContent(filepath.Join(repo, name), content, source.DetectLanguage(name))
		if err != nil {
			return nil, fmt.Errorf("quality analysis of %s at %s: %w", name, head, err)
		}
		reports = append(reports, qr)
	}
	return qualityInputOf(reports), nil
}

var verdictEmoji = map[string]string{"approve": "✅", "approve_with_suggestions": "✅", "request_changes": "⚠️", "block": "❌"}

func formatMarkdownReport(r Report) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Code Review Report")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "**Generated:** %s\n", r.Metadata.GeneratedAt)
	fmt.Fprintf(&b, "**Repository:** %s\n", r.Metadata.Repository)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Executive Summary")
	fmt.Fprintln(&b)
	emoji := verdictEmoji[r.Summary.Verdict]
	if emoji == "" {
		emoji = "❓"
	}
	fmt.Fprintf(&b, "**Verdict:** %s %s\n", emoji, strings.ToUpper(strings.ReplaceAll(r.Summary.Verdict, "_", " ")))
	fmt.Fprintf(&b, "**Score:** %d/100\n", r.Summary.Score)
	fmt.Fprintf(&b, "**Rationale:** %s\n", r.Summary.Rationale)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "### Issue Summary")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Severity | Count |")
	fmt.Fprintln(&b, "|----------|-------|")
	for _, sev := range severityOrder {
		fmt.Fprintf(&b, "| %s | %d |\n", textutil.Title(sev), severityCount(r.Summary.IssueCounts, sev))
	}
	fmt.Fprintln(&b)

	renderMarkdownChangeStats(&b, r.PRSummary)

	renderMarkdownActionItems(&b, r.ActionItems)
	renderMarkdownFindings(&b, "Critical Issues (Must Fix)", filterFindings(r.Findings, "critical"))
	high := filterFindings(r.Findings, "high")
	if len(high) > 10 {
		high = high[:10]
	}
	renderMarkdownFindings(&b, "High Priority Issues", high)

	renderMarkdownReviewOrder(&b, r.ReviewOrder)

	fmt.Fprintln(&b, "---")
	fmt.Fprint(&b, "*Generated by Code Reviewer*")
	return b.String()
}

func renderMarkdownChangeStats(b *strings.Builder, s *PRSummary) {
	if s == nil {
		return
	}
	fmt.Fprintln(b, "### Change Statistics")
	fmt.Fprintln(b)
	fmt.Fprintf(b, "- **Files Changed:** %d\n", s.FilesChanged)
	fmt.Fprintf(b, "- **Lines Added:** +%d\n", s.TotalAdditions)
	fmt.Fprintf(b, "- **Lines Removed:** -%d\n", s.TotalDeletions)
	fmt.Fprintf(b, "- **Complexity:** %s\n", s.ComplexityLabel)
	fmt.Fprintln(b)
}

func renderMarkdownReviewOrder(b *strings.Builder, order []string) {
	if len(order) == 0 {
		return
	}
	fmt.Fprintln(b, "## Suggested Review Order")
	fmt.Fprintln(b)
	for i, fp := range order {
		if i >= 10 {
			break
		}
		fmt.Fprintf(b, "%d. `%s`\n", i+1, fp)
	}
	fmt.Fprintln(b)
}

func renderMarkdownActionItems(b *strings.Builder, items []ActionItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(b, "## Action Items")
	fmt.Fprintln(b)
	for i, item := range items {
		e := "🟡"
		switch item.Priority {
		case "P0":
			e = "🔴"
		case "P1":
			e = "🟠"
		}
		fmt.Fprintf(b, "%d. %s **[%s]** %s\n", i+1, e, item.Priority, item.Action)
		if len(item.FilesAffected) > 0 {
			n := item.FilesAffected
			if len(n) > 3 {
				n = n[:3]
			}
			fmt.Fprintf(b, "   - Files: %s\n", strings.Join(n, ", "))
		}
	}
	fmt.Fprintln(b)
}

func renderMarkdownFindings(b *strings.Builder, title string, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", title)
	for _, f := range findings {
		fmt.Fprintf(b, "- **%s** in `%s`\n", f.Category, orDefault(f.File, "unknown"))
		fmt.Fprintf(b, "  - %s\n", f.Message)
	}
	fmt.Fprintln(b)
}

func severityCount(c ReportIssueCounts, sev string) int {
	switch sev {
	case "critical":
		return c.Critical
	case "high":
		return c.High
	case "medium":
		return c.Medium
	default:
		return c.Low
	}
}

func filterFindings(findings []Finding, sev string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Severity == sev {
			out = append(out, f)
		}
	}
	return out
}

func formatTextReport(r Report) string {
	var b strings.Builder
	line60 := strings.Repeat("=", 60)
	fmt.Fprintln(&b, line60)
	fmt.Fprintln(&b, "CODE REVIEW REPORT")
	fmt.Fprintln(&b, line60)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Generated: %s\n", r.Metadata.GeneratedAt)
	fmt.Fprintf(&b, "Repository: %s\n", r.Metadata.Repository)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "VERDICT: %s\n", strings.ToUpper(strings.ReplaceAll(r.Summary.Verdict, "_", " ")))
	fmt.Fprintf(&b, "SCORE: %d/100\n", r.Summary.Score)
	fmt.Fprintf(&b, "RATIONALE: %s\n", r.Summary.Rationale)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "--- ISSUE SUMMARY ---")
	for _, sev := range severityOrder {
		fmt.Fprintf(&b, "  %s: %d\n", textutil.Title(sev), severityCount(r.Summary.IssueCounts, sev))
	}
	fmt.Fprintln(&b)

	if len(r.ActionItems) > 0 {
		fmt.Fprintln(&b, "--- ACTION ITEMS ---")
		for i, item := range r.ActionItems {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "  %d. [%s] %s\n", i+1, item.Priority, item.Action)
		}
		fmt.Fprintln(&b)
	}

	critical := filterFindings(r.Findings, "critical")
	if len(critical) > 0 {
		fmt.Fprintln(&b, "--- CRITICAL ISSUES ---")
		for _, f := range critical {
			fmt.Fprintf(&b, "  [%s] %s\n", orDefault(f.File, "unknown"), f.Message)
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprint(&b, line60)
	return b.String()
}
