package source

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// RunQuality is the code_quality_checker.py CLI: same flags, exit 1 on a missing path or output write error.
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

	var report any
	if info.IsDir() {
		if dr, derr := analyzeDirectory(target, parsed.language); derr != nil {
			report = errorResult{Error: derr.Error()}
		} else {
			report = dr
		}
	} else {
		if qr, qerr := Quality(target, ""); qerr != nil {
			report = errorResult{Error: qerr.Error()}
		} else {
			report = qr
		}
	}

	if parsed.jsonOut {
		if err := emitQualityJSON(report, parsed.output, stdout); err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
	} else {
		printQualityReport(report, stdout)
	}
	return 0
}

func emitQualityJSON(report any, output string, stdout io.Writer) error {
	var buf strings.Builder
	if err := writeJSON(&buf, report); err != nil {
		return err
	}
	if output != "" {
		if err := os.WriteFile(output, []byte(buf.String()), 0o644); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "Results written to %s\n", output)
		return err
	} else {
		_, err := fmt.Fprintln(stdout, buf.String())
		return err
	}
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
	fmt.Fprintf(w, "Avg Complexity: %s\n", formatPyFloat(float64(r.Metrics.AvgComplexity)))
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
	fmt.Fprintf(w, "Average Score: %s/100 (%s)\n", formatPyFloat(float64(d.AverageScore)), d.OverallGrade)
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

// formatPyFloat mimics Python's str(float): "3.2" not "3.200000", "0.0" not "0".
func formatPyFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	if !strings.Contains(s, ".") && !strings.Contains(s, "e") {
		s += ".0"
	}
	return s
}
