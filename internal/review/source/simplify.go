package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
)

type simplifyArgs struct {
	targets   []string
	base      string
	threshold string
	jsonOut   bool
}

type simplifyQuality struct {
	Score  int     `json:"score"`
	Grade  string  `json:"grade"`
	Smells []Smell `json:"smells"`
}

type simplifyFile struct {
	File       string          `json:"file"`
	Complexity []Finding       `json:"complexity"`
	Quality    simplifyQuality `json:"quality"`
}

type simplifySummary struct {
	Files      int `json:"files"`
	Complexity int `json:"complexity"`
	Quality    int `json:"quality"`
}

type simplifyReport struct {
	Files   []simplifyFile  `json:"files"`
	Summary simplifySummary `json:"summary"`
}

// parseSimplifyArgs: -h prints the usage page to stdout (exit 0); a bad flag or threshold exits 2.
func parseSimplifyArgs(args []string, stdout, stderr io.Writer) (simplifyArgs, int, bool) {
	fs := helptext.NewFlagSet("review simplify", helptext.Page{
		Usage: "claude-code review simplify [paths...] [flags]",
		About: "One advisory pass of complexity and quality checks. With no paths, " +
			"checks the files changed since the base.",
	})
	base := fs.String("base", "", "diff base ref (default: merge-base with upstream, else HEAD~1)")
	threshold := fs.String("threshold", "medium", "strict|medium|relaxed")
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	if helptext.WantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return simplifyArgs{}, 0, true
	}
	fs.SetOutput(stderr)
	targets, err := outfmt.Parse(fs, args)
	if err != nil {
		return simplifyArgs{}, helptext.ExitCode(err), true
	}
	if _, ok := complexityThresholds[*threshold]; !ok {
		fmt.Fprintf(stderr, "review simplify: argument --threshold: invalid choice: %q\n", *threshold)
		return simplifyArgs{}, 2, true
	}
	return simplifyArgs{targets: targets, base: *base, threshold: *threshold, jsonOut: *jsonOut}, 0, false
}

// gitLines runs git with list args in dir and returns its non-empty output lines.
func gitLines(dir string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// simplifyBase resolves the diff base: the flag, else merge-base with upstream, else HEAD~1.
func simplifyBase(flag string) string {
	if flag != "" {
		return flag
	}
	if l, err := gitLines(".", "merge-base", "@{upstream}", "HEAD"); err == nil && len(l) > 0 {
		return l[0]
	}
	return "HEAD~1"
}

// changedSimplifyFiles lists existing changed files (working tree vs base), relative to the cwd when possible.
func changedSimplifyFiles(base string) ([]string, error) {
	tops, err := gitLines(".", "rev-parse", "--show-toplevel")
	if err != nil || len(tops) == 0 {
		return nil, errors.New("not a git repository")
	}
	names, err := changedNames(tops[0], base)
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	if real, evalErr := filepath.EvalSymlinks(cwd); evalErr == nil {
		cwd = real // git's toplevel is symlink-resolved (macOS /var -> /private/var); Rel needs the same root
	}
	var files []string
	for _, n := range names {
		abs := filepath.Join(tops[0], n)
		if st, statErr := os.Stat(abs); statErr != nil || st.IsDir() {
			continue
		}
		if rel, relErr := filepath.Rel(cwd, abs); relErr == nil {
			abs = rel
		}
		files = append(files, abs)
	}
	return files, nil
}

// changedNames is git diff --name-only against base plus untracked files, which git diff never lists.
func changedNames(top, base string) ([]string, error) {
	names, err := gitLines(top, "diff", "--name-only", base)
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", base, err)
	}
	untracked, err := gitLines(top, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	return append(names, untracked...), nil
}

// simplifyTargets expands paths (files or dirs) to supported-language files, deduplicated and sorted.
func simplifyTargets(paths []string) []string {
	var exts []string
	for _, list := range Extensions {
		for _, e := range list {
			exts = append(exts, strings.TrimPrefix(e, "."))
		}
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range paths {
		for _, f := range collectComplexityFiles(p, exts) {
			if key := resolvePath(f); !seen[key] && detectLanguage(f) != "" {
				seen[key] = true
				files = append(files, f)
			}
		}
	}
	sort.Strings(files)
	return files
}

// analyzeSimplifyFile runs both checks on one file; ok is false when the file cannot be analyzed.
func analyzeSimplifyFile(path string, th ComplexityThresholds) (simplifyFile, bool) {
	q, err := Quality(path, "")
	if err != nil {
		return simplifyFile{}, false
	}
	var findings []Finding
	if r, cerr := analyzeFileComplexity(path, th); cerr == nil && r != nil {
		findings = r.Findings
	}
	return simplifyFile{File: path, Complexity: nonNilFindings(findings),
		Quality: simplifyQuality{Score: q.QualityScore, Grade: q.Grade, Smells: nonNilSmells(q.Smells)}}, true
}

func printSimplifyReport(w io.Writer, r simplifyReport, threshold string) {
	fmt.Fprintf(w, "Simplify Check — %d files, threshold %s\n", r.Summary.Files, threshold)
	for _, f := range r.Files {
		if len(f.Complexity) == 0 && len(f.Quality.Smells) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  %s  (quality %d/100 %s)\n", f.File, f.Quality.Score, f.Quality.Grade)
		for _, c := range f.Complexity {
			loc := ""
			if c.Line > 0 {
				loc = fmt.Sprintf(" line %d", c.Line)
			}
			fmt.Fprintf(w, "    [%s] %s%s: %s\n", strings.ToUpper(c.Severity), c.Rule, loc, c.Message)
		}
		for _, s := range f.Quality.Smells {
			fmt.Fprintf(w, "    [%s] %s (%s): %s\n", strings.ToUpper(s.Severity), s.Type, s.Location, s.Message)
		}
	}
	fmt.Fprintf(w, "\nFindings: %d (complexity %d, quality %d)\n",
		r.Summary.Complexity+r.Summary.Quality, r.Summary.Complexity, r.Summary.Quality)
}

// RunSimplify is the advisory first pass of the simplify skill; findings never change the exit code.
func RunSimplify(args []string, stdout, stderr io.Writer) int {
	parsed, code, done := parseSimplifyArgs(args, stdout, stderr)
	if done {
		return code
	}
	paths := parsed.targets
	if len(paths) == 0 {
		changed, err := changedSimplifyFiles(simplifyBase(parsed.base))
		if err != nil {
			fmt.Fprintf(stderr, "review simplify: %s\n", err)
			return 1
		}
		paths = changed
	}
	th := complexityThresholds[parsed.threshold]
	report := simplifyReport{Files: []simplifyFile{}}
	for _, f := range simplifyTargets(paths) {
		if sf, ok := analyzeSimplifyFile(f, th); ok {
			report.Files = append(report.Files, sf)
			report.Summary.Complexity += len(sf.Complexity)
			report.Summary.Quality += len(sf.Quality.Smells)
		}
	}
	report.Summary.Files = len(report.Files)
	if parsed.jsonOut {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
		fmt.Fprintln(stdout)
		return 0
	}
	if report.Summary.Files == 0 {
		fmt.Fprintln(stdout, "No changed source files.")
		return 0
	}
	printSimplifyReport(stdout, report, parsed.threshold)
	return 0
}

var lineLoc = regexp.MustCompile(`line \d+`)

// SimplicityFindings returns one-line complexity and quality findings present in after but not in before.
func SimplicityFindings(path, before, after string) []string {
	lang := detectLanguage(path)
	if lang == "" {
		return nil
	}
	old := map[string]int{}
	for _, f := range simplicityKeyed(path, before, lang) {
		old[f.key]++
	}
	var out []string
	for _, f := range simplicityKeyed(path, after, lang) {
		if old[f.key] > 0 {
			old[f.key]--
			continue
		}
		out = append(out, f.text)
	}
	return out
}

type keyedFinding struct{ key, text string }

// simplicityKeyed lists every finding of content with a line-free key and a located one-line text.
func simplicityKeyed(path, content, lang string) []keyedFinding {
	if content == "" {
		return nil
	}
	var out []keyedFinding
	if cl := detectComplexityLang(path); cl != "" {
		for _, f := range complexityContent(path, content, cl, complexityThresholds["medium"]).Findings {
			loc := ""
			if f.Line > 0 {
				loc = fmt.Sprintf("line %d: ", f.Line)
			}
			out = append(out, keyedFinding{"c|" + f.Rule + "|" + f.Message, loc + f.Message})
		}
	}
	if q, err := QualityContent(path, content, lang); err == nil {
		for _, s := range q.Smells {
			text := s.Type + ": " + s.Message
			if s.Location != "" {
				text = s.Location + ": " + text
			}
			key := "q|" + s.Type + "|" + s.Message + "|" + lineLoc.ReplaceAllString(s.Location, "")
			out = append(out, keyedFinding{key, text})
		}
	}
	return out
}
