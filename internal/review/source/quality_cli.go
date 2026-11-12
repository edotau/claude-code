package source

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

type qualityArgs struct {
	path     string
	language string
	jsonOut  bool
	output   string
}

// parseQualityArgs is a real FlagSet, so -h anywhere in args prints the usage page; -l/-o/-r mirror the
// former argparse short aliases. Explicit -h prints to stdout and exits 0; a bad flag stays on stderr, exit 2.
func parseQualityArgs(args []string, stdout, stderr io.Writer) (qualityArgs, int, bool) {
	fs := helptext.NewFlagSet("review quality", helptext.Page{
		Usage: "claude-code review quality <path> [flags]",
		About: "Code smells and SOLID violations in one file, or every file under a directory.",
	})
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	language := fs.String("language", "", "only this language when <path> is a directory (-l)")
	fs.StringVar(language, "l", "", "shorthand for --language")
	output := fs.String("output", "", "with --json: write the report to this file (-o)")
	fs.StringVar(output, "o", "", "shorthand for --output")
	if helptext.WantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return qualityArgs{}, 0, true
	}
	fs.SetOutput(stderr)
	positional, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return qualityArgs{}, helptext.ExitCode(err), true
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "review quality: expected exactly one path argument, got %d\n", len(positional))
		fs.Usage()
		return qualityArgs{}, 2, true
	}
	return qualityArgs{path: positional[0], language: *language, jsonOut: *jsonOut, output: *output}, 0, false
}

// resolvePath mirrors Path(...).resolve(): absolute, with symlinks collapsed where possible.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// RunQuality is the code_quality_checker.py CLI: same flags, exit 1 on a bad path, analysis or write error.
func RunQuality(args []string, stdout, stderr io.Writer) int {
	parsed, code, done := parseQualityArgs(args, stdout, stderr)
	if done {
		return code
	}
	target := resolvePath(parsed.path)
	info, statErr := os.Stat(target)
	if statErr != nil {
		fmt.Fprintf(stderr, "Error: Path does not exist: %s\n", target)
		return 1
	}

	report := qualityReportFor(target, info.IsDir(), parsed.language, stderr)

	if parsed.jsonOut {
		if err := emitQualityJSON(report, parsed.output, stdout); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
	} else {
		printQualityReport(report, stdout)
	}
	if _, failed := report.(errorResult); failed {
		return 1
	}
	if dr, ok := report.(directoryReport); ok && len(dr.FilesFailed) > 0 {
		return 1
	}
	return 0
}

// qualityReportFor analyzes a directory or a single file; an analysis error becomes an errorResult report.
func qualityReportFor(target string, isDir bool, language string, warn io.Writer) any {
	if isDir {
		dr, err := analyzeDirectory(target, language, warn)
		if err != nil {
			return errorResult{Error: err.Error()}
		}
		return dr
	}
	qr, err := Quality(target)
	if err != nil {
		return errorResult{Error: err.Error()}
	}
	return qr
}

func emitQualityJSON(report any, output string, stdout io.Writer) error {
	var buf strings.Builder
	if err := writeJSON(&buf, report); err != nil {
		return err
	}
	if output == "" {
		_, err := fmt.Fprintln(stdout, buf.String())
		return err
	}
	// AtomicWrite would create a missing parent directory; fail as the plain write did.
	if _, err := os.Stat(filepath.Dir(output)); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	if err := paths.AtomicWrite(output, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	_, err := fmt.Fprintf(stdout, "Results written to %s\n", output)
	return err
}

func printQualityReport(report any, w io.Writer) {
	if e, ok := report.(errorResult); ok {
		fmt.Fprintf(w, "Error: %s\n", e.Error)
		return
	}
	fmt.Fprintln(w, strings.Repeat("=", 60))
	fmt.Fprintln(w, "CODE QUALITY REPORT")
	fmt.Fprintln(w, strings.Repeat("=", 60))
	switch v := report.(type) {
	case QualityReport:
		printFileQuality(v, w)
	case directoryReport:
		printDirQuality(v, w)
	}
	fmt.Fprintln(w, "\n"+strings.Repeat("=", 60))
}

func printFileQuality(r QualityReport, w io.Writer) {
	fmt.Fprintf(w, "\nFile: %s\n", r.File)
	fmt.Fprintf(w, "Language: %s\n", r.Language)
	fmt.Fprintf(w, "Quality Score: %d/100 (%s)\n", r.QualityScore, r.Grade)
	fmt.Fprintf(w, "\nLines: %d (%d code, %d comments)\n", r.Metrics.Lines.Total, r.Metrics.Lines.Code, r.Metrics.Lines.Comment)
	fmt.Fprintf(w, "Functions: %d   Classes: %d\n", r.Metrics.Functions, r.Metrics.Classes)
	fmt.Fprintf(w, "Avg Complexity: %s\n", FormatPyFloat(float64(r.Metrics.AvgComplexity)))
	if len(r.Smells) > 0 {
		fmt.Fprintln(w, "\n--- CODE SMELLS ---")
		for i, s := range r.Smells {
			if i >= 10 {
				break
			}
			fmt.Fprintf(w, "  [%s] %s (%s)\n", strings.ToUpper(s.Severity), s.Message, s.Location)
		}
	}
	if len(r.SolidViolations) > 0 {
		fmt.Fprintln(w, "\n--- SOLID VIOLATIONS ---")
		for _, v := range r.SolidViolations {
			fmt.Fprintf(w, "  [%s] %s\n", v.Principle, v.Message)
		}
	}
}

func printDirQuality(d directoryReport, w io.Writer) {
	fmt.Fprintf(w, "\nDirectory: %s\n", d.Directory)
	fmt.Fprintf(w, "Files Analyzed: %d\n", d.FilesAnalyzed)
	fmt.Fprintf(w, "Average Score: %s/100 (%s)\n", FormatPyFloat(float64(d.AverageScore)), d.OverallGrade)
	fmt.Fprintf(w, "Total Code Smells: %d\n", d.TotalCodeSmells)
	fmt.Fprintf(w, "Total SOLID Violations: %d\n", d.TotalSolidViolations)
	fmt.Fprintln(w, "\n--- FILES BY QUALITY ---")
	for i, f := range d.Files {
		if i >= 10 {
			break
		}
		fmt.Fprintf(w, "  %3d/100 [%s] %s\n", f.QualityScore, f.Grade, f.File)
	}
}

// FormatPyFloat mimics Python's str(float): "3.2" not "3.200000", "0.0" not "0".
func FormatPyFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	if !strings.Contains(s, ".") && !strings.Contains(s, "e") {
		s += ".0"
	}
	return s
}

// skipParts are directory names never walked; venv, build and dist are decided by isOutputDir.
var skipParts = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".venv": true}

// pathHasSkippedPart mirrors `SKIP_PARTS & set(filepath.parts)`.
func pathHasSkippedPart(path string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if skipParts[part] {
			return true
		}
	}
	return false
}

// isOutputDir reports a venv or build/dist directory holding output; a build/dist with .go files is a package.
func isOutputDir(dir string) bool {
	switch filepath.Base(dir) {
	case "venv":
		_, err := os.Stat(filepath.Join(dir, "pyvenv.cfg"))
		return err == nil
	case "build", "dist":
		entries, err := os.ReadDir(dir)
		return err == nil && !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), ".go") })
	}
	return false
}

// walkDir is the traversal seam; tests wrap it to count the paths a walk visits.
var walkDir = filepath.WalkDir

// skipEntry reports a walk entry under a skipped part, or a directory below root that holds output.
func skipEntry(root, p string, d os.DirEntry) bool {
	rel, err := filepath.Rel(root, p)
	return err != nil || pathHasSkippedPart(rel) || (d != nil && d.IsDir() && p != root && isOutputDir(p))
}

// walkSourceFiles calls fn for each non-skipped file under root and returns the paths the walk could not read.
func walkSourceFiles(root string, warn io.Writer, fn func(p string)) []string {
	var unreadable []string
	_ = walkDir(root, func(p string, d os.DirEntry, err error) error {
		if skipEntry(root, p, d) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			fmt.Fprintf(warn, "warning: skipping unreadable path %s: %v\n", p, err)
			unreadable = append(unreadable, p)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fn(p)
		return nil
	})
	return unreadable
}

// analyzeDirectory ports code_quality_checker.analyze_directory.
func analyzeDirectory(dir, language string, warn io.Writer) (directoryReport, error) {
	var extensions []string
	if language != "" {
		extensions = Extensions[language]
	} else {
		for _, lang := range languageOrder {
			extensions = append(extensions, Extensions[lang]...)
		}
	}

	results, failed := walkExtensions(dir, extensions, warn)
	if len(results) == 0 && len(failed) > 0 {
		return directoryReport{}, fmt.Errorf("analysis failed for %d files, e.g. %s", len(failed), failed[0])
	}
	if len(results) == 0 {
		return directoryReport{}, errors.New("no supported files found")
	}

	total := 0.0
	for _, r := range results {
		total += float64(r.QualityScore)
	}
	avg := PyFloat(Round1(total / float64(len(results))))
	smells, violations := 0, 0
	for _, r := range results {
		smells += len(r.Smells)
		violations += len(r.SolidViolations)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].QualityScore < results[j].QualityScore })

	return directoryReport{
		Directory: dir, FilesAnalyzed: len(results), AverageScore: avg, OverallGrade: getGrade(int(avg)),
		TotalCodeSmells: smells, TotalSolidViolations: violations, Files: results, FilesFailed: failed,
	}, nil
}

// walkExtensions collects supported files in one traversal; failed lists files whose analysis errored.
func walkExtensions(dir string, extensions []string, warn io.Writer) (out []QualityReport, failed []string) {
	byExtension := make(map[string][]string, len(extensions))
	for _, ext := range extensions {
		byExtension[ext] = nil
	}
	failed = walkSourceFiles(dir, warn, func(p string) {
		for ext := range byExtension {
			if strings.HasSuffix(p, ext) {
				byExtension[ext] = append(byExtension[ext], p)
			}
		}
	})
	for _, ext := range extensions {
		matches := byExtension[ext]
		sort.Strings(matches)
		for _, m := range matches {
			r, err := Quality(m)
			if err != nil {
				fmt.Fprintf(warn, "warning: analysis failed: %v\n", err)
				failed = append(failed, m)
				continue
			}
			out = append(out, r)
		}
	}
	return out, failed
}
