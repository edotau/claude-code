package source

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type qualityArgs struct {
	path     string
	language string
	jsonOut  bool
	output   string
}

// parseQualityArgs accepts argparse's interspersed positional/flag order and its -l/-o short aliases.
func parseQualityArgs(args []string) (qualityArgs, error) {
	var a qualityArgs
	var positionals []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--recursive" || tok == "-r":
			// store_true with default=True: the flag is inert either way (see report deviation note).
		case tok == "--json":
			a.jsonOut = true
		case tok == "--language" || tok == "-l":
			i++
			if i >= len(args) {
				return a, fmt.Errorf("argument --language/-l: expected one argument")
			}
			a.language = args[i]
		case strings.HasPrefix(tok, "--language="):
			a.language = strings.TrimPrefix(tok, "--language=")
		case tok == "--output" || tok == "-o":
			i++
			if i >= len(args) {
				return a, fmt.Errorf("argument --output/-o: expected one argument")
			}
			a.output = args[i]
		case strings.HasPrefix(tok, "--output="):
			a.output = strings.TrimPrefix(tok, "--output=")
		case tok != "-" && strings.HasPrefix(tok, "-"):
			return a, fmt.Errorf("unrecognized arguments: %s", tok)
		default:
			positionals = append(positionals, tok)
		}
	}
	if len(positionals) != 1 {
		return a, fmt.Errorf("expected exactly one path argument, got %d", len(positionals))
	}
	a.path = positionals[0]
	return a, nil
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

// RunQuality is the code_quality_checker.py CLI: same flags, same exit codes (1 only on a missing path).
func RunQuality(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseQualityArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	target := resolvePath(parsed.path)
	info, statErr := os.Stat(target)
	if statErr != nil {
		fmt.Fprintf(stderr, "Error: Path does not exist: %s\n", target)
		return 1
	}

	var report interface{}
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
		emitQualityJSON(report, parsed.output, stdout)
	} else {
		printQualityReport(report, stdout)
	}
	return 0
}

func emitQualityJSON(report interface{}, output string, stdout io.Writer) {
	var buf strings.Builder
	writeJSON(&buf, report)
	if output != "" {
		os.WriteFile(output, []byte(buf.String()), 0o644)
		fmt.Fprintf(stdout, "Results written to %s\n", output)
	} else {
		fmt.Fprintln(stdout, buf.String())
	}
}

func printQualityReport(report interface{}, w io.Writer) {
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
