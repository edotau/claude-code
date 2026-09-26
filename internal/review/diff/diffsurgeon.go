// Package diff ports diff_surgeon.py, pr_analyzer.py, review_report_generator.py and
// code-review-gate.sh to Go (stdlib only). Text and --json output match the Python byte-for-byte.
package diff

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	whitespaceOnlyRe = regexp.MustCompile(`^[+-]\s*$`)
	commentOnlyRe    = regexp.MustCompile(`^[+-]\s*(?:#|//|/\*|\*/|\*|<!--)`)
	docstringAddRe   = regexp.MustCompile(`^[+](?:"""|''')`)
)

// diffFinding mirrors a single classified line in the Python dict shape.
type diffFinding struct {
	Category string `json:"category"`
	Line     string `json:"line"`
}

type diffFileResult struct {
	File     string        `json:"file"`
	Findings []diffFinding `json:"findings"`
}

type diffSurgeonResult struct {
	Status          string           `json:"status"`
	FilesInDiff     int              `json:"files_in_diff"`
	TotalChangeLine int              `json:"total_change_lines"`
	NoiseLines      int              `json:"noise_lines"`
	NoiseRatio      pyFloat          `json:"noise_ratio"`
	Verdict         string           `json:"verdict"`
	FileResults     []diffFileResult `json:"file_results"`
}

// noChangesResult mirrors the shortcut dict emitted when the diff is empty.
type noChangesResult struct {
	Status  string `json:"status"`
	Files   int    `json:"files"`
	Noise   int    `json:"noise_lines"`
	Verdict string `json:"verdict"`
	Message string `json:"message"`
}

type diffFileEntry struct {
	file    string
	changes []string
}

func runGitDiff(diffRange string, stderr io.Writer) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"diff", "--cached"}
	if diffRange != "" {
		args = []string{"diff", diffRange}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.Output()
	// Like Python's check=False: git's own non-zero exit (not a repo, bad range) is silent; only a missing git or timeout errors.
	if err != nil && (ctx.Err() != nil || !errors.As(err, new(*exec.ExitError))) {
		fmt.Fprintf(stderr, "[error] git diff failed: %v\n", err)
		return "", false
	}
	return string(out), true
}

// getDiff returns (text, exitCode); exitCode is 0 on success, 1 if --file doesn't exist (a real
// usage error, unlike a git-diff failure which stays advisory-0 per the Python script).
func getDiff(diffRange, file string, stderr io.Writer) (string, int) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "[error] %s not found\n", file)
			return "", 1
		}
		return string(b), 0
	}
	text, ok := runGitDiff(diffRange, stderr)
	if !ok {
		return "", 0
	}
	return text, 0
}

// parseDiffFiles splits a unified diff into per-file lists of +/- body lines.
func parseDiffFiles(diffText string) []*diffFileEntry {
	var files []*diffFileEntry
	var current *diffFileEntry
	for _, line := range strings.Split(diffText, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			idx := strings.LastIndex(line, " b/")
			name := line
			if idx != -1 {
				name = line[idx+3:]
			}
			current = &diffFileEntry{file: name}
			files = append(files, current)
		case current == nil, strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"):
			continue
		case line != "" && (line[0] == '+' || line[0] == '-'):
			current.changes = append(current.changes, line)
		}
	}
	return files
}

// classifyLine returns a noise category for a changed line, or "" if it looks intentional.
func classifyLine(line string) string {
	if whitespaceOnlyRe.MatchString(line) {
		return "whitespace"
	}
	if commentOnlyRe.MatchString(line) {
		return "comment-only"
	}
	if docstringAddRe.MatchString(line) {
		return "docstring-addition"
	}
	return ""
}

// detectQuoteSwaps finds -/+ pairs identical except for quote characters (sorted-zip, matches Python).
func detectQuoteSwaps(changes []string) []diffFinding {
	var adds, dels []string
	for _, c := range changes {
		if strings.HasPrefix(c, "+") {
			adds = append(adds, c)
		} else if strings.HasPrefix(c, "-") {
			dels = append(dels, c)
		}
	}
	sort.Strings(adds)
	sort.Strings(dels)
	var findings []diffFinding
	n := len(adds)
	if len(dels) < n {
		n = len(dels)
	}
	for i := 0; i < n; i++ {
		a, d := adds[i], dels[i]
		aBody := strings.TrimSpace(a[1:])
		dBody := strings.TrimSpace(d[1:])
		if aBody != dBody && strings.ReplaceAll(aBody, `"`, "'") == strings.ReplaceAll(dBody, `"`, "'") {
			findings = append(findings, diffFinding{
				Category: "quote-style-swap",
				Line:     fmt.Sprintf("%s -> %s", truncate(d, 60), truncate(a, 60)),
			})
		}
	}
	return findings
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func analyzeDiffFile(fd *diffFileEntry) []diffFinding {
	var findings []diffFinding
	for _, line := range fd.changes {
		if cat := classifyLine(line); cat != "" {
			findings = append(findings, diffFinding{Category: cat, Line: truncate(line, 120)})
		}
	}
	findings = append(findings, detectQuoteSwaps(fd.changes)...)
	return findings
}

func buildDiffResult(files []*diffFileEntry) diffSurgeonResult {
	fileResults := []diffFileResult{}
	totalNoise, totalChanges := 0, 0
	for _, fd := range files {
		totalChanges += len(fd.changes)
		findings := analyzeDiffFile(fd)
		if len(findings) > 0 {
			totalNoise += len(findings)
			fileResults = append(fileResults, diffFileResult{File: fd.file, Findings: findings})
		}
	}
	ratio := 0.0
	if totalChanges > 0 {
		ratio = roundTo(float64(totalNoise)/float64(totalChanges), 2)
	}
	verdict := "CLEAN"
	if ratio >= 0.3 {
		verdict = "VERY_NOISY"
	} else if ratio >= 0.1 {
		verdict = "NOISY"
	}
	return diffSurgeonResult{
		Status:          "ok",
		FilesInDiff:     len(files),
		TotalChangeLine: totalChanges,
		NoiseLines:      totalNoise,
		NoiseRatio:      pyFloat(ratio),
		Verdict:         verdict,
		FileResults:     fileResults,
	}
}

func printDiffReport(w io.Writer, result diffSurgeonResult) {
	fmt.Fprintf(w, "Diff Surgeon — %d files, %d changed lines\n", result.FilesInDiff, result.TotalChangeLine)
	fmt.Fprintf(w, "Noise ratio: %s (%d noise lines)\n", pyPercent(float64(result.NoiseRatio)), result.NoiseLines)
	fmt.Fprintf(w, "Verdict: %s\n", result.Verdict)
	if len(result.FileResults) == 0 {
		fmt.Fprint(w, "\n  All changes look intentional. Clean diff.\n")
		return
	}
	fmt.Fprintln(w)
	for _, fr := range result.FileResults {
		fmt.Fprintf(w, "  %s:\n", fr.File)
		var cats []string
		byCat := map[string][]string{}
		for _, f := range fr.Findings {
			if _, ok := byCat[f.Category]; !ok {
				cats = append(cats, f.Category)
			}
			byCat[f.Category] = append(byCat[f.Category], f.Line)
		}
		for _, cat := range cats {
			lines := byCat[cat]
			fmt.Fprintf(w, "    [%s] %d instance(s)\n", cat, len(lines))
			for i, line := range lines {
				if i >= 3 {
					break
				}
				fmt.Fprintf(w, "      %s\n", line)
			}
			if len(lines) > 3 {
				fmt.Fprintf(w, "      ... and %d more\n", len(lines)-3)
			}
		}
	}
	fmt.Fprint(w, "\nRecommendation: review flagged lines. Remove changes that don't trace to your task.\n")
}

// RunDiff ports diff_surgeon.py's CLI. Findings are advisory (exit 0); a bad flag or a missing
// --file is a real usage error (exit 2 / 1 respectively).
func RunDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	diffRange := fs.String("diff", "", "Git diff range (e.g. HEAD~1..HEAD). Default: staged.")
	file := fs.String("file", "", "Read diff from a file instead of git")
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	diffText, exitCode := getDiff(*diffRange, *file, stderr)
	if exitCode != 0 {
		return exitCode
	}
	if strings.TrimSpace(diffText) == "" {
		result := noChangesResult{Status: "ok", Files: 0, Noise: 0, Verdict: "CLEAN", Message: "No diff to analyze"}
		if *jsonOut {
			fmt.Fprintln(stdout, mustIndentJSON(result))
		} else {
			fmt.Fprintln(stdout, "No diff to analyze. Stage changes (git add) or pass --diff.")
		}
		return 0
	}

	result := buildDiffResult(parseDiffFiles(diffText))
	if *jsonOut {
		fmt.Fprintln(stdout, mustIndentJSON(result))
		return 0
	}
	printDiffReport(stdout, result)
	return 0
}
