package diff

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

type fileCategory struct {
	patterns []*regexp.Regexp
	weight   int
}

// fileCategoryOrder preserves Python dict insertion order (critical, high, medium, low) for iteration.
var fileCategoryOrder = []string{"critical", "high", "medium", "low"}

var fileCategories = sync.OnceValue(func() map[string]fileCategory {
	return map[string]fileCategory{
		"critical": {compileAll("auth", "security", "password", "token", "secret", "payment", "billing", "crypto", "encrypt"), 5},
		"high":     {compileAll("api", "database", "migration", "schema", "model", "config", "env", "middleware"), 4},
		"medium":   {compileAll("service", "controller", "handler", "util", "helper"), 3},
		"low":      {compileAll("test", "spec", "mock", "fixture", "story", "readme", "docs", `\.md$`), 1},
	}
})

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

var riskPatterns = sync.OnceValue(func() []riskPattern {
	return []riskPattern{
		{"hardcoded_secrets", regexp.MustCompile(`(?i)(password|secret|api_key|token)\s*[=:]\s*['"][^'"]+['"]`), "critical", "Potential hardcoded secret detected"},
		{"todo_fixme", regexp.MustCompile(`(?i)(TODO|FIXME|HACK|XXX):`), "low", "TODO/FIXME comment found"},
		{"console_log", regexp.MustCompile(`(?i)console\.(log|debug|info|warn|error)\(`), "medium", "Console statement found (remove for production)"},
		{"debugger", regexp.MustCompile(`(?i)\bdebugger\b`), "high", "Debugger statement found"},
		{"disable_eslint", regexp.MustCompile(`(?i)eslint-disable`), "medium", "ESLint rule disabled"},
		{"any_type", regexp.MustCompile(`(?i):\s*any\b`), "medium", "TypeScript 'any' type used"},
		{"sql_concatenation", regexp.MustCompile(`(?i)(SELECT|INSERT|UPDATE|DELETE).*\+.*['"]`), "critical", "Potential SQL injection (string concatenation in query)"},
	}
})

var commitPrefixRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(feat|fix|docs|style|refactor|test|chore|perf|ci|build|revert)(\(.+\))?:`)
})

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

// gitInherit runs git in repo under a 30s timeout; no output here starts a line with whitespace (proc trims right).
func gitInherit(repo string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return proc.RunGitInherit(ctx, repo, args...)
}

// refDiff runs `git diff <opts> base...head [-- paths]`, retrying two-dot only when the merge-base form errors.
func refDiff(repo, base, head string, opts []string, paths ...string) (string, error) {
	run := func(revs ...string) (string, error) {
		args := append(append([]string{"diff"}, opts...), revs...)
		if len(paths) > 0 {
			args = append(append(args, "--"), paths...)
		}
		return gitInherit(repo, args...)
	}
	out, err := run(base + "..." + head)
	if err != nil {
		out, err = run(base, head)
	}
	return out, err
}

// changedFiles lists the base...head changes; an empty branch is not an error and the index is never consulted.
func changedFiles(repo, base, head string) ([]PRFileAnalysis, error) {
	out, err := refDiff(repo, base, head, []string{"--name-status", "-z"})
	if err != nil {
		return nil, err
	}
	return parseNameStatus(out), nil
}

// parseNameStatus turns `git diff --name-status -z` output into files; unknown status letters count as modified.
func parseNameStatus(out string) []PRFileAnalysis {
	statusMap := map[string]string{"A": "added", "M": "modified", "D": "deleted", "R": "renamed", "C": "copied"}
	var files []PRFileAnalysis
	tok := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i+1 < len(tok); {
		code, n := tok[i], 1
		if code != "" && (code[0] == 'R' || code[0] == 'C') {
			n = 2
		}
		if i+n >= len(tok) {
			break
		}
		status := ""
		if code != "" {
			status = statusMap[code[:1]]
		}
		if status == "" {
			status = "modified"
		}
		files = append(files, PRFileAnalysis{Path: tok[i+n], Status: status})
		i += n + 1
	}
	return files
}

// patchFor returns the base...head patch for one file; a git failure is an error, never an empty diff.
// The pathspec is top-relative and literal: name-status paths are repo-root relative, and glob characters match.
func patchFor(repo, path, base, head string) (string, error) {
	return refDiff(repo, base, head, nil, ":(top,literal)"+path)
}

func categorizeFile(filepath string) (string, int) {
	lower := strings.ToLower(filepath)
	for _, cat := range fileCategoryOrder {
		info := fileCategories()[cat]
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
	for _, line := range diffBodyLines(diffContent) {
		if strings.HasPrefix(line, "+") {
			added = append(added, line[1:])
		}
	}
	content := strings.Join(added, "\n")
	for _, rp := range riskPatterns() {
		matches := rp.pattern.FindAllString(content, -1)
		if len(matches) > 0 {
			risks = append(risks, PRRisk{Name: rp.name, Severity: rp.severity, Message: rp.message, File: filepath, Count: len(matches)})
		}
	}
	return risks
}

func countChanges(diffContent string) (additions, deletions int) {
	for _, line := range diffBodyLines(diffContent) {
		if strings.HasPrefix(line, "+") {
			additions++
		} else if strings.HasPrefix(line, "-") {
			deletions++
		}
	}
	return
}

// diffBodyLines returns the lines inside hunks, so `+++i;` and `--- x` bodies are not mistaken for file headers.
func diffBodyLines(diffContent string) []string {
	var body []string
	inHunk := false
	for line := range strings.SplitSeq(diffContent, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk:
			body = append(body, line)
		}
	}
	return body
}

// checkRefs rejects refs that git would parse as options (e.g. --output=FILE).
func checkRefs(refs ...string) error {
	for _, r := range refs {
		if strings.HasPrefix(r, "-") {
			return fmt.Errorf("invalid ref %q: must not start with '-'", r)
		}
	}
	return nil
}

func calculateComplexityScore(files []PRFileAnalysis, allRisks []PRRisk) int {
	total := 0
	for _, f := range files {
		total += f.Additions + f.Deletions
	}
	critical, high := 0, 0
	for _, r := range allRisks {
		switch r.Severity {
		case "critical":
			critical++
		case "high":
			high++
		}
	}
	score := tierPoints(len(files), 20, 10, 5) + tierPoints(total, 500, 200, 50)
	score += min(2, critical) + min(2, high)
	return min(max(score, 1), 10)
}

// tierPoints scores n 3/2/1 for exceeding the top/mid/low thresholds, else 0.
func tierPoints(n, top, mid, low int) int {
	switch {
	case n > top:
		return 3
	case n > mid:
		return 2
	case n > low:
		return 1
	}
	return 0
}

func analyzeCommitMessages(repo, base, head string) (int, []CommitIssue, error) {
	out, err := gitInherit(repo, "log", "--oneline", base+".."+head)
	issues := []CommitIssue{}
	if err != nil {
		return 0, nil, err
	}
	if out == "" {
		return 0, issues, nil
	}
	commits := strings.Split(out, "\n")
	for _, commit := range commits {
		sha, message, found := strings.Cut(commit, " ")
		if !found || message == "" {
			continue
		}
		if !commitPrefixRe().MatchString(message) {
			issues = append(issues, CommitIssue{Commit: sha, Issue: "Does not follow conventional commit format"})
		}
		if len(message) > 72 {
			issues = append(issues, CommitIssue{Commit: sha, Issue: "Commit message exceeds 72 characters"})
		}
	}
	return len(commits), issues, nil
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

// analyzePR ports pr_analyzer.py's analyze_pr: full complexity/risk analysis for base...head.
func analyzePR(repo, base, head string) (PRReport, error) {
	if err := checkRefs(base, head); err != nil {
		return PRReport{}, err
	}
	files, err := changedFiles(repo, base, head)
	if err != nil {
		return PRReport{}, fmt.Errorf("git diff %s...%s: %w", base, head, err)
	}
	if len(files) == 0 {
		return PRReport{Status: "no_changes", Message: "No changes detected between branches"}, nil
	}

	allRisks, err := analyzeFiles(repo, base, head, files)
	if err != nil {
		return PRReport{}, err
	}
	sortFilesByPriority(files)

	commitCount, commitIssues, err := analyzeCommitMessages(repo, base, head)
	if err != nil {
		return PRReport{}, fmt.Errorf("git log %s..%s: %w", base, head, err)
	}
	complexity := calculateComplexityScore(files, allRisks)

	totalAdditions, totalDeletions := 0, 0
	for _, f := range files {
		totalAdditions += f.Additions
		totalDeletions += f.Deletions
	}

	risksBySeverity := groupRisks(allRisks)

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

// analyzeFiles fills each file's category, weight, change counts and risks in place and returns every risk found.
// A file whose patch cannot be read fails the whole analysis instead of counting as clean.
func analyzeFiles(repo, base, head string, files []PRFileAnalysis) ([]PRRisk, error) {
	var allRisks []PRRisk
	for i := range files {
		category, weight := categorizeFile(files[i].Path)
		d, err := patchFor(repo, files[i].Path, base, head)
		if err != nil {
			return nil, fmt.Errorf("diff %s: %w", files[i].Path, err)
		}
		additions, deletions := countChanges(d)
		risks := analyzeDiffForRisks(d, files[i].Path)
		allRisks = append(allRisks, risks...)
		files[i].Category = category
		files[i].PriorityWeight = weight
		files[i].Additions = additions
		files[i].Deletions = deletions
		files[i].Risks = risks
	}
	return allRisks, nil
}

func sortFilesByPriority(files []PRFileAnalysis) {
	sort.SliceStable(files, func(a, b int) bool {
		if files[a].PriorityWeight != files[b].PriorityWeight {
			return files[a].PriorityWeight > files[b].PriorityWeight
		}
		return files[a].Path < files[b].Path
	})
}

func groupRisks(allRisks []PRRisk) PRRisks {
	g := PRRisks{Critical: []PRRisk{}, High: []PRRisk{}, Medium: []PRRisk{}, Low: []PRRisk{}}
	for _, r := range allRisks {
		switch r.Severity {
		case "critical":
			g.Critical = append(g.Critical, r)
		case "high":
			g.High = append(g.High, r)
		case "medium":
			g.Medium = append(g.Medium, r)
		case "low":
			g.Low = append(g.Low, r)
		}
	}
	return g
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

	printRiskList(w, "CRITICAL", r.Risks.Critical)
	printRiskList(w, "HIGH", r.Risks.High)
	printCommitIssues(w, r.CommitIssues)
	printReviewOrder(w, r)
	fmt.Fprintln(w, "\n"+strings.Repeat("=", 60))
}

func printRiskList(w io.Writer, label string, risks []PRRisk) {
	if len(risks) == 0 {
		return
	}
	fmt.Fprintf(w, "\n--- %s RISKS ---\n", label)
	for _, risk := range risks {
		fmt.Fprintf(w, "  [%s] %s (x%d)\n", risk.File, risk.Message, risk.Count)
	}
}

func printCommitIssues(w io.Writer, issues []CommitIssue) {
	if len(issues) == 0 {
		return
	}
	fmt.Fprintln(w, "\n--- COMMIT MESSAGE ISSUES ---")
	for i, issue := range issues {
		if i >= 5 {
			break
		}
		fmt.Fprintf(w, "  %s: %s\n", issue.Commit, issue.Issue)
	}
}

func printReviewOrder(w io.Writer, r PRReport) {
	fmt.Fprintln(w, "\n--- SUGGESTED REVIEW ORDER ---")
	for i, filepath := range r.ReviewOrder {
		for _, f := range r.Files {
			if f.Path == filepath {
				fmt.Fprintf(w, "  %d. [%s] %s\n", i+1, strings.ToUpper(f.Category), filepath)
				break
			}
		}
	}
}

// RunPR ports pr_analyzer.py's CLI.
func RunPR(args []string, stdout, stderr io.Writer) int {
	fs := helptext.NewFlagSet("pr", helptext.Page{
		Usage: "claude-code review pr [<repo>] [flags]",
		About: "Summarize a branch's changes against a base: files by category, risk, review order.",
	})
	fs.SetOutput(stderr)
	base := fs.String("base", "main", "Base branch for comparison (default: main)")
	fs.StringVar(base, "b", "main", "shorthand for --base")
	head := fs.String("head", "HEAD", "Head branch/commit for comparison (default: HEAD)")
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	output := fs.String("output", "", "Write output to file")
	fs.StringVar(output, "o", "", "shorthand for --output")
	positional, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}

	repoPath, ok := resolveGitRepo(positional, stderr)
	if !ok {
		return 1
	}

	report, err := analyzePR(repoPath, *base, *head)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	var buf strings.Builder
	if *jsonOut {
		js, jsErr := indentJSON(report)
		if jsErr != nil {
			fmt.Fprintf(stderr, "Error: %v\n", jsErr)
			return 1
		}
		buf.WriteString(js)
		buf.WriteString("\n")
	} else {
		printPRReport(&buf, report)
	}
	return writeOutput(buf.String(), false, *output, "Results", stdout, stderr)
}

// absRepoArg returns the absolute path of the first positional (default "."), reporting failures to stderr.
func absRepoArg(positional []string, stderr io.Writer) (string, bool) {
	repoArg := "."
	if len(positional) > 0 {
		repoArg = positional[0]
	}
	repoPath, err := filepath.Abs(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", false
	}
	return repoPath, true
}

// resolveGitRepo returns the repo's top-level dir for the first positional (default "."), reporting failures to stderr.
func resolveGitRepo(positional []string, stderr io.Writer) (string, bool) {
	repoPath, ok := absRepoArg(positional, stderr)
	if !ok {
		return "", false
	}
	top, err := gitInherit(repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintf(stderr, "Error: %s is not a git repository\n", repoPath)
		return "", false
	}
	return top, true
}

// writeOutput prints text (plus a newline when ln) to stdout, or writes it to the output file and confirms with label.
func writeOutput(text string, ln bool, output, label string, stdout, stderr io.Writer) int {
	if output == "" {
		emit := fmt.Fprint
		if ln {
			emit = fmt.Fprintln
		}
		if _, err := emit(stdout, text); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}
	if err := paths.AtomicWrite(output, []byte(text), 0o644); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s written to %s\n", label, output)
	return 0
}
