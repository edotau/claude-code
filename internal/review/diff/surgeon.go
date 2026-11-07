// Package diff analyzes diffs and PRs: hunk surgery, PR analysis, the review report and the review gate, stdlib only.
package diff

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var (
	whitespaceOnlyRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+-]\s*$`) })
	hashCommentRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+-]\s*#`) })
	slashCommentRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+-]\s*(?://|/\*|\*/)`) })
	blockContRe      = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+-]\s*\*(?:\s|$)`) })
	markupCommentRe  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+-]\s*<!--`) })
	docstringAddRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+](?:"""|''')`) })
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

// Comment leaders by file extension; an unlisted or missing extension has none (conservative: not a comment).
var (
	hashExts   = setOf(".sh", ".bash", ".zsh", ".py", ".rb", ".yaml", ".yml", ".toml", ".pl", ".r", ".mk", ".conf", ".cfg", ".ini")
	slashExts  = setOf(".go", ".c", ".h", ".cc", ".cpp", ".hpp", ".java", ".js", ".jsx", ".ts", ".tsx", ".rs", ".swift", ".kt", ".cs", ".php", ".scala", ".css")
	markupExts = setOf(".html", ".htm", ".xml", ".md", ".svg", ".vue")
)

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

type diffFileEntry struct {
	file    string
	changes []string
}

func runGitDiff(repo, diffRange string, stderr io.Writer) (string, bool) {
	args := []string{"diff", "--cached"}
	if err := checkRefs(diffRange); err != nil {
		fmt.Fprintf(stderr, "[error] %v\n", err)
		return "", false
	}
	if diffRange != "" {
		args = []string{"diff", diffRange}
	}
	out, _, err := gitInherit(repo, args...)
	if err != nil {
		fmt.Fprintf(stderr, "[error] git diff failed: %v\n", err)
		return "", false
	}
	return out, true
}

// getDiff returns (text, exitCode); exitCode is 1 if --file is missing or git diff failed.
func getDiff(repo, diffRange, file string, stderr io.Writer) (string, int) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "[error] %s not found\n", file)
			return "", 1
		}
		return string(b), 0
	}
	text, ok := runGitDiff(repo, diffRange, stderr)
	if !ok {
		return "", 1
	}
	return text, 0
}

// parseDiffFiles splits a unified diff into per-file lists of +/- body lines.
func parseDiffFiles(diffText string) []*diffFileEntry {
	var files []*diffFileEntry
	var current *diffFileEntry
	inHunk := false
	for line := range strings.SplitSeq(diffText, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			current = &diffFileEntry{file: diffFileName(line)}
			files = append(files, current)
			inHunk = false
		case current == nil:
			continue
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case isBodyChange(line, inHunk):
			current.changes = append(current.changes, line)
		}
	}
	return files
}

// diffFileName returns the b/ path of a "diff --git" header, or the whole line when it has none.
func diffFileName(line string) string {
	if _, after, ok := strings.CutLast(line, " b/"); ok {
		return after
	}
	return line
}

// isBodyChange reports a +/- line, excluding the ---/+++ file headers that precede the first hunk.
func isBodyChange(line string, inHunk bool) bool {
	if !inHunk && (strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---")) {
		return false
	}
	return line != "" && (line[0] == '+' || line[0] == '-')
}

// isCommentLine reports a comment-only changed line using the leaders valid for the file extension.
func isCommentLine(line, ext string) bool {
	switch {
	case hashExts[ext]:
		return hashCommentRe().MatchString(line)
	case slashExts[ext]:
		return slashCommentRe().MatchString(line) || blockContRe().MatchString(line)
	case markupExts[ext]:
		return markupCommentRe().MatchString(line)
	}
	return false
}

// classifyLine returns a noise category for a changed line, or "" if it looks intentional.
func classifyLine(line, file string) string {
	if whitespaceOnlyRe().MatchString(line) {
		return "whitespace"
	}
	if isCommentLine(line, strings.ToLower(filepath.Ext(file))) {
		return "comment-only"
	}
	if docstringAddRe().MatchString(line) {
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
	n := min(len(dels), len(adds))
	for i := range n {
		a, d := adds[i], dels[i]
		aBody := strings.TrimSpace(a[1:])
		dBody := strings.TrimSpace(d[1:])
		if aBody != dBody && strings.ReplaceAll(aBody, `"`, "'") == strings.ReplaceAll(dBody, `"`, "'") {
			findings = append(findings, diffFinding{
				Category: "quote-style-swap",
				Line:     fmt.Sprintf("%s -> %s", textutil.TruncateRunes(d, 60), textutil.TruncateRunes(a, 60)),
			})
		}
	}
	return findings
}

func analyzeDiffFile(fd *diffFileEntry) []diffFinding {
	var findings []diffFinding
	var unclassified []string
	for _, line := range fd.changes {
		if cat := classifyLine(line, fd.file); cat != "" {
			findings = append(findings, diffFinding{Category: cat, Line: textutil.TruncateRunes(line, 120)})
		} else {
			unclassified = append(unclassified, line)
		}
	}
	// Lines already counted as noise stay out of the quote-swap scan so no line is counted twice.
	findings = append(findings, detectQuoteSwaps(unclassified)...)
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
	return runDiffIn("", args, stdout, stderr)
}

// runDiffIn is RunDiff with git run in repo ("" = the cwd).
func runDiffIn(repo string, args []string, stdout, stderr io.Writer) int {
	fs := helptext.NewFlagSet("diff", helptext.Page{
		Usage: "claude-code review diff [flags]",
		About: "Flag diff lines that don't trace to the task (noise, drive-by edits). Findings are advisory.",
	})
	fs.SetOutput(stderr)
	diffRange := fs.String("diff", "", "Git diff range (e.g. HEAD~1..HEAD). Default: staged.")
	file := fs.String("file", "", "Read diff from a file instead of git")
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}

	diffText, exitCode := getDiff(repo, *diffRange, *file, stderr)
	if exitCode != 0 {
		return exitCode
	}
	if strings.TrimSpace(diffText) == "" {
		result := noChangesResult{Status: "ok", Files: 0, Noise: 0, Verdict: "CLEAN", Message: "No diff to analyze"}
		if *jsonOut {
			return printJSON(stdout, stderr, result)
		}
		fmt.Fprintln(stdout, "No diff to analyze. Stage changes (git add) or pass --diff.")
		return 0
	}

	result := buildDiffResult(parseDiffFiles(diffText))
	if *jsonOut {
		return printJSON(stdout, stderr, result)
	}
	printDiffReport(stdout, result)
	return 0
}

// splitPositional separates positional args from flags so Go's flag.Parse (which stops at the
// first non-flag token) can still handle argparse-style interleaving like ". --base main".
func splitPositional(args []string, boolFlags map[string]bool) (positional, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' && a != "-" {
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") || boolFlags[name] {
				continue
			}
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return positional, rest
}

// printJSON writes v as indented JSON; an encode failure goes to stderr with exit 1.
func printJSON(stdout, stderr io.Writer, v any) int {
	js, err := indentJSON(v)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, js)
	return 0
}
