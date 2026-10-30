package diff

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var gateFileRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\.(py|ts|tsx|js|jsx)$`) })
var gateVerdictLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\s+\[WARN\]|^Verdict:`) })
var gateNoiseLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`Noise ratio:|Verdict:`) })

func gateStagedFiles(repo string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := proc.RunGitInherit(ctx, repo, "diff", "--cached", "--name-only")
	if err != nil {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) != "" {
			files = append(files, line)
		}
	}
	return files
}

func gateComplexityFiles(repo string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := proc.RunGitInherit(ctx, repo, "diff", "--cached", "--name-only", "--diff-filter=ACMR")
	if err != nil {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && gateFileRe().MatchString(line) {
			files = append(files, line)
		}
	}
	return files
}

func filterLines(text string, re *regexp.Regexp) (string, bool) {
	var matched []string
	for line := range strings.SplitSeq(text, "\n") {
		if re.MatchString(line) {
			matched = append(matched, line)
		}
	}
	if len(matched) == 0 {
		return "", false
	}
	return strings.Join(matched, "\n"), true
}

// RunGate ports code-review-gate.sh: advisory pre-commit summary — ALWAYS returns 0.
func RunGate(args []string, stdout, stderr io.Writer) int {
	repo, help := gateRepo(args, stderr)
	if help {
		return 0
	}

	staged := gateStagedFiles(repo)
	if len(staged) == 0 {
		return 0
	}

	fmt.Fprintln(stdout, "--- code-review gate (advisory) ---")

	changed := gateComplexityFiles(repo)
	if len(changed) > 0 {
		fmt.Fprintln(stdout, "[simplicity] complexity_checker (medium):")
		var buf bytes.Buffer
		rc := source.RunComplexity(append(changed, "--threshold", "medium"), &buf, &buf)
		if rc != 0 {
			fmt.Fprintf(stdout, "  detector failed (exit %d):\n", rc)
			indentLines(stdout, buf.String())
		} else if matched, ok := filterLines(buf.String(), gateVerdictLineRe()); ok {
			fmt.Fprintln(stdout, matched)
		} else {
			fmt.Fprintln(stdout, "  clean")
		}
	}

	fmt.Fprintln(stdout, "[surgical] diff_surgeon (staged):")
	var out2 bytes.Buffer
	rc2 := RunDiff(nil, &out2, &out2)
	if rc2 != 0 {
		fmt.Fprintf(stdout, "  detector failed (exit %d):\n", rc2)
		indentLines(stdout, out2.String())
	} else if matched, ok := filterLines(out2.String(), gateNoiseLineRe()); ok {
		fmt.Fprintln(stdout, matched)
	} else {
		fmt.Fprintln(stdout, "  clean")
	}

	fmt.Fprintln(stdout, "--- /code-review gate ---")
	return 0
}

// gateRepo parses RunGate's flags; a bad flag must not block a commit, so it falls back to scanning for --repo.
func gateRepo(args []string, stderr io.Writer) (repo string, help bool) {
	fs := helptext.NewFlagSet("gate", helptext.Page{
		Usage: "claude-code review gate [flags]",
		About: "Advisory pre-commit summary of the staged change: complexity and diff-noise\n" +
			"findings. Always exits 0, so it never blocks a commit.",
	})
	fs.SetOutput(stderr)
	repoFlag := fs.String("repo", ".", "the git repo whose staged change to check")
	if _, done := helptext.Parse(fs, args); !done {
		return *repoFlag, false
	}
	if wantsHelp(args) {
		return "", true
	}
	repo = "."
	for i, a := range args {
		if a == "--repo" && i+1 < len(args) {
			repo = args[i+1]
		}
	}
	return repo, false
}

// wantsHelp reports a -h/--help flag in args.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "-help" || a == "--help" {
			return true
		}
	}
	return false
}

func indentLines(w io.Writer, text string) {
	for line := range strings.SplitSeq(text, "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
