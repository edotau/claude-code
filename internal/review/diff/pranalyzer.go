package diff

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type fileCategory struct {
	patterns []*regexp.Regexp
	weight   int
}

// fileCategoryOrder preserves Python dict insertion order (critical, high, medium, low) for iteration.
var fileCategoryOrder = []string{"critical", "high", "medium", "low"}

var fileCategories = map[string]fileCategory{
	"critical": {compileAll("auth", "security", "password", "token", "secret", "payment", "billing", "crypto", "encrypt"), 5},
	"high":     {compileAll("api", "database", "migration", "schema", "model", "config", "env", "middleware"), 4},
	"medium":   {compileAll("service", "controller", "handler", "util", "helper"), 3},
	"low":      {compileAll("test", "spec", "mock", "fixture", "story", "readme", "docs", `\.md$`), 1},
}

func compileAll(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

type riskPattern struct {
	name     string
	pattern  *regexp.Regexp
	severity string
	message  string
}

var riskPatterns = []riskPattern{
	{"hardcoded_secrets", regexp.MustCompile(`(?i)(password|secret|api_key|token)\s*[=:]\s*['"][^'"]+['"]`), "critical", "Potential hardcoded secret detected"},
	{"todo_fixme", regexp.MustCompile(`(?i)(TODO|FIXME|HACK|XXX):`), "low", "TODO/FIXME comment found"},
	{"console_log", regexp.MustCompile(`(?i)console\.(log|debug|info|warn|error)\(`), "medium", "Console statement found (remove for production)"},
	{"debugger", regexp.MustCompile(`(?i)\bdebugger\b`), "high", "Debugger statement found"},
	{"disable_eslint", regexp.MustCompile(`(?i)eslint-disable`), "medium", "ESLint rule disabled"},
	{"any_type", regexp.MustCompile(`(?i):\s*any\b`), "medium", "TypeScript 'any' type used"},
	{"sql_concatenation", regexp.MustCompile(`(?i)(SELECT|INSERT|UPDATE|DELETE).*\+.*['"]`), "critical", "Potential SQL injection (string concatenation in query)"},
}

var commitPrefixRe = regexp.MustCompile(`^(feat|fix|docs|style|refactor|test|chore|perf|ci|build|revert)(\(.+\))?:`)

// PRRisk is a single risky-pattern hit found in the diff for a file.
type PRRisk struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	File     string `json:"file"`
	Count    int    `json:"count"`
}

// PRFileAnalysis is the per-file entry in a PRReport.
type PRFileAnalysis struct {
	Path           string   `json:"path"`
	Status         string   `json:"status"`
	Category       string   `json:"category"`
	PriorityWeight int      `json:"priority_weight"`
	Additions      int      `json:"additions"`
	Deletions      int      `json:"deletions"`
	Risks          []PRRisk `json:"risks"`
}

// CommitIssue flags a single commit message that fails convention checks.
type CommitIssue struct {
	Commit string `json:"commit"`
	Issue  string `json:"issue"`
}

// PRSummary is the top-level summary block of a PRReport.
type PRSummary struct {
	FilesChanged    int    `json:"files_changed"`
	TotalAdditions  int    `json:"total_additions"`
	TotalDeletions  int    `json:"total_deletions"`
	ComplexityScore int    `json:"complexity_score"`
	ComplexityLabel string `json:"complexity_label"`
	Commits         int    `json:"commits"`
}

// PRRisks buckets PRRisk findings by severity.
type PRRisks struct {
	Critical []PRRisk `json:"critical"`
	High     []PRRisk `json:"high"`
	Medium   []PRRisk `json:"medium"`
	Low      []PRRisk `json:"low"`
}

// PRReport is the full pr_analyzer.py output shape (analyzed or no_changes).
type PRReport struct {
	Status       string           `json:"status"`
	Message      string           `json:"message,omitempty"`
	Summary      *PRSummary       `json:"summary,omitempty"`
	Risks        *PRRisks         `json:"risks,omitempty"`
	Files        []PRFileAnalysis `json:"files,omitempty"`
	CommitIssues []CommitIssue    `json:"commit_issues,omitempty"`
	ReviewOrder  []string         `json:"review_order,omitempty"`
}

func runGit(repo string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full := append([]string{"-C", repo}, args...)
	out, err := execCommand(ctx, "git", full...)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func changedFiles(repo, base, head string) []PRFileAnalysis {
	statusMap := map[string]string{"A": "added", "M": "modified", "D": "deleted", "R": "renamed", "C": "copied"}
	out, ok := runGit(repo, "diff", "--name-status", base+"..."+head)
	if !ok || out == "" {
		out, ok = runGit(repo, "diff", "--name-status", base, head)
	}
	if !ok || out == "" {
		out, ok = runGit(repo, "diff", "--name-status", "--cached")
	}
	var files []PRFileAnalysis
	if !ok {
		return files
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status := statusMap[string(parts[0][0])]
		if status == "" {
			status = "modified"
		}
		files = append(files, PRFileAnalysis{Path: parts[len(parts)-1], Status: status})
	}
	return files
}

func fileDiff(repo, filepath, base, head string) string {
	out, ok := runGit(repo, "diff", base+"..."+head, "--", filepath)
	if !ok {
		out, ok = runGit(repo, "diff", "--cached", "--", filepath)
	}
	if !ok {
		return ""
	}
	return out
}

func categorizeFile(filepath string) (string, int) {
	lower := strings.ToLower(filepath)
	for _, cat := range fileCategoryOrder {
		info := fileCategories[cat]
		for _, re := range info.patterns {
			if re.MatchString(lower) {
				return cat, info.weight
			}
		}
	}
	return "medium", 2
}

func analyzeDiffForRisks(diffContent, filepath string) []PRRisk {
	risks := []PRRisk{}
	var added []string
	for _, line := range strings.Split(diffContent, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added = append(added, line[1:])
		}
	}
	content := strings.Join(added, "\n")
	for _, rp := range riskPatterns {
		matches := rp.pattern.FindAllString(content, -1)
		if len(matches) > 0 {
			risks = append(risks, PRRisk{Name: rp.name, Severity: rp.severity, Message: rp.message, File: filepath, Count: len(matches)})
		}
	}
	return risks
}

func countChanges(diffContent string) (additions, deletions int) {
	for _, line := range strings.Split(diffContent, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			additions++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			deletions++
		}
	}
	return
}

func calculateComplexityScore(files []PRFileAnalysis, allRisks []PRRisk) int {
	score := 0
	fileCount := len(files)
	switch {
	case fileCount > 20:
		score += 3
	case fileCount > 10:
		score += 2
	case fileCount > 5:
		score += 1
	}
	total := 0
	for _, f := range files {
		total += f.Additions + f.Deletions
	}
	switch {
	case total > 500:
		score += 3
	case total > 200:
		score += 2
	case total > 50:
		score += 1
	}
	critical, high := 0, 0
	for _, r := range allRisks {
		if r.Severity == "critical" {
			critical++
		} else if r.Severity == "high" {
			high++
		}
	}
	score += int(math.Min(2, float64(critical)))
	score += int(math.Min(2, float64(high)))
	if score > 10 {
		score = 10
	}
	if score < 1 {
		score = 1
	}
	return score
}

func analyzeCommitMessages(repo, base, head string) (int, []CommitIssue) {
	out, ok := runGit(repo, "log", "--oneline", base+"..."+head)
	issues := []CommitIssue{}
	if !ok || out == "" {
		return 0, issues
	}
	commits := strings.Split(out, "\n")
	for _, commit := range commits {
		sha, message, found := strings.Cut(commit, " ")
		if !found || message == "" {
			continue
		}
		if !commitPrefixRe.MatchString(message) {
			issues = append(issues, CommitIssue{Commit: sha, Issue: "Does not follow conventional commit format"})
		}
		if len(message) > 72 {
			issues = append(issues, CommitIssue{Commit: sha, Issue: "Commit message exceeds 72 characters"})
		}
	}
	return len(commits), issues
}

func complexityLabel(score int) string {
	switch {
	case score <= 2:
		return "Simple"
	case score <= 4:
		return "Moderate"
	case score <= 6:
		return "Complex"
	case score <= 8:
		return "Very Complex"
	default:
		return "Critical"
	}
}

// AnalyzePR ports pr_analyzer.py's analyze_pr: full complexity/risk analysis for base...head.
func AnalyzePR(repo, base, head string) (PRReport, error) {
	files := changedFiles(repo, base, head)
	if len(files) == 0 {
		return PRReport{Status: "no_changes", Message: "No changes detected between branches"}, nil
	}

	var allRisks []PRRisk
	for i := range files {
		category, weight := categorizeFile(files[i].Path)
		d := fileDiff(repo, files[i].Path, base, head)
		additions, deletions := countChanges(d)
		risks := analyzeDiffForRisks(d, files[i].Path)
		allRisks = append(allRisks, risks...)
		files[i].Category = category
		files[i].PriorityWeight = weight
		files[i].Additions = additions
		files[i].Deletions = deletions
		files[i].Risks = risks
	}

	sort.SliceStable(files, func(a, b int) bool {
		if files[a].PriorityWeight != files[b].PriorityWeight {
			return files[a].PriorityWeight > files[b].PriorityWeight
		}
		return files[a].Path < files[b].Path
	})

	commitCount, commitIssues := analyzeCommitMessages(repo, base, head)
	complexity := calculateComplexityScore(files, allRisks)

	totalAdditions, totalDeletions := 0, 0
	for _, f := range files {
		totalAdditions += f.Additions
		totalDeletions += f.Deletions
	}

	risksBySeverity := PRRisks{Critical: []PRRisk{}, High: []PRRisk{}, Medium: []PRRisk{}, Low: []PRRisk{}}
	for _, r := range allRisks {
		switch r.Severity {
		case "critical":
			risksBySeverity.Critical = append(risksBySeverity.Critical, r)
		case "high":
			risksBySeverity.High = append(risksBySeverity.High, r)
		case "medium":
			risksBySeverity.Medium = append(risksBySeverity.Medium, r)
		case "low":
			risksBySeverity.Low = append(risksBySeverity.Low, r)
		}
	}

	reviewOrder := []string{}
	for i, f := range files {
		if i >= 10 {
			break
		}
		reviewOrder = append(reviewOrder, f.Path)
	}

	return PRReport{
		Status: "analyzed",
		Summary: &PRSummary{
			FilesChanged: len(files), TotalAdditions: totalAdditions, TotalDeletions: totalDeletions,
			ComplexityScore: complexity, ComplexityLabel: complexityLabel(complexity), Commits: commitCount,
		},
		Risks:        &risksBySeverity,
		Files:        files,
		CommitIssues: commitIssues,
		ReviewOrder:  reviewOrder,
	}, nil
}

func printPRReport(w io.Writer, r PRReport) {
	if r.Status == "no_changes" {
		fmt.Fprintln(w, "No changes detected.")
		return
	}
	s := r.Summary
	fmt.Fprintln(w, strings.Repeat("=", 60))
	fmt.Fprintln(w, "PR ANALYSIS REPORT")
	fmt.Fprintln(w, strings.Repeat("=", 60))
	fmt.Fprintf(w, "\nComplexity: %d/10 (%s)\n", s.ComplexityScore, s.ComplexityLabel)
	fmt.Fprintf(w, "Files Changed: %d\n", s.FilesChanged)
	fmt.Fprintf(w, "Lines: +%d / -%d\n", s.TotalAdditions, s.TotalDeletions)
	fmt.Fprintf(w, "Commits: %d\n", s.Commits)

	fmt.Fprintln(w, "\n--- RISK SUMMARY ---")
	fmt.Fprintf(w, "Critical: %d\n", len(r.Risks.Critical))
	fmt.Fprintf(w, "High: %d\n", len(r.Risks.High))
	fmt.Fprintf(w, "Medium: %d\n", len(r.Risks.Medium))
	fmt.Fprintf(w, "Low: %d\n", len(r.Risks.Low))

	if len(r.Risks.Critical) > 0 {
		fmt.Fprintln(w, "\n--- CRITICAL RISKS ---")
		for _, risk := range r.Risks.Critical {
			fmt.Fprintf(w, "  [%s] %s (x%d)\n", risk.File, risk.Message, risk.Count)
		}
	}
	if len(r.Risks.High) > 0 {
		fmt.Fprintln(w, "\n--- HIGH RISKS ---")
		for _, risk := range r.Risks.High {
			fmt.Fprintf(w, "  [%s] %s (x%d)\n", risk.File, risk.Message, risk.Count)
		}
	}

	if len(r.CommitIssues) > 0 {
		fmt.Fprintln(w, "\n--- COMMIT MESSAGE ISSUES ---")
		for i, issue := range r.CommitIssues {
			if i >= 5 {
				break
			}
			fmt.Fprintf(w, "  %s: %s\n", issue.Commit, issue.Issue)
		}
	}

	fmt.Fprintln(w, "\n--- SUGGESTED REVIEW ORDER ---")
	for i, filepath := range r.ReviewOrder {
		for _, f := range r.Files {
			if f.Path == filepath {
				fmt.Fprintf(w, "  %d. [%s] %s\n", i+1, strings.ToUpper(f.Category), filepath)
				break
			}
		}
	}
	fmt.Fprintln(w, "\n"+strings.Repeat("=", 60))
}

// RunPR ports pr_analyzer.py's CLI.
func RunPR(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, map[string]bool{"json": true})
	fs := flag.NewFlagSet("pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "main", "Base branch for comparison (default: main)")
	fs.StringVar(base, "b", "main", "Base branch for comparison (default: main)")
	head := fs.String("head", "HEAD", "Head branch/commit for comparison (default: HEAD)")
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	output := fs.String("output", "", "Write output to file")
	fs.StringVar(output, "o", "", "Write output to file")
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
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		fmt.Fprintf(stderr, "Error: %s is not a git repository\n", repoPath)
		return 1
	}

	report, _ := AnalyzePR(repoPath, *base, *head)

	if *jsonOut {
		out := mustIndentJSON(report)
		if *output != "" {
			if err := os.WriteFile(*output, []byte(out), 0o644); err != nil {
				fmt.Fprintf(stderr, "Error: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "Results written to %s\n", *output)
		} else {
			fmt.Fprintln(stdout, out)
		}
		return 0
	}
	printPRReport(stdout, report)
	return 0
}
