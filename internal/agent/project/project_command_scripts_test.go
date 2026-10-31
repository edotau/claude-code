package project

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// scriptRefPattern finds a scripts/<name>.sh reference anywhere in a command file's text.
var scriptRefPattern = regexp.MustCompile(`scripts/[A-Za-z0-9_.-]+\.sh`)

// argPlaceholderPattern matches $ARGUMENTS or a positional $1-$9, neither of which Claude Code
// substitutes inside a !`...` directive.
var argPlaceholderPattern = regexp.MustCompile(`\$(ARGUMENTS|[1-9])\b`)

// isInlineBashDirective reports whether a "!"-led line is a fully-closed single-line !`...` directive —
// the only shape Claude Code actually executes (code.claude.com/docs/en/skills.md); a bare "!cmd" never runs.
func isInlineBashDirective(line string) bool {
	return strings.HasPrefix(line, "!`") && strings.HasSuffix(line, "`") && len(line) > 2
}

// checkCommandScripts walks commands/**/*.md under root, applying the three guard rules below.
// Returns a slice of "<rel>:<line>: <message>" failures, or nil if the file is clean.
func checkCommandScripts(root, path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, path)
	text := string(body)
	var failures []string

	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "!") {
			continue
		}
		lineNo := i + 1
		if !isInlineBashDirective(trimmed) {
			failures = append(failures, rel+":"+strconv.Itoa(lineNo)+": "+trimmed+" is not a single-line !`...` directive")
			continue
		}
		if argPlaceholderPattern.MatchString(trimmed) {
			failures = append(failures, rel+":"+strconv.Itoa(lineNo)+": "+trimmed+" uses $ARGUMENTS/$1-$9, which Claude Code never substitutes inside !`...`")
		}
	}

	// scripts/<x>.sh may be referenced anywhere in the file (a ! line or prose telling the model to run it).
	for _, ref := range scriptRefPattern.FindAllString(text, -1) {
		scriptPath := filepath.Join(root, ref)
		info, serr := os.Stat(scriptPath)
		if serr != nil {
			failures = append(failures, rel+": references "+ref+" which does not exist")
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			failures = append(failures, rel+": references "+ref+" which is not executable")
		}
	}
	return failures, nil
}

// TestCommandBashDirectivesDelegateToScripts guards the commands/*.md "!" directive contract: every
// "!"-led line must be a single-line !`...` directive with no $ARGUMENTS/$1-$9 placeholder (neither
// substitutes), and every scripts/<name>.sh referenced anywhere in the file must exist and be executable.
func TestCommandBashDirectivesDelegateToScripts(t *testing.T) {
	root := repoRoot(t)
	commandsDir := filepath.Join(root, "commands")
	if _, err := os.Stat(commandsDir); err != nil {
		t.Skip("no commands/ dir — not the harness checkout")
	}

	checked := 0
	err := filepath.Walk(commandsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		checked++
		failures, cerr := checkCommandScripts(root, path)
		if cerr != nil {
			return cerr
		}
		for _, f := range failures {
			t.Error(f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Skip("no commands/*.md files found — not the harness checkout")
	}
}

// TestCommandBashDirectiveGuardHasTeeth proves the guard rejects the exact regression it exists to
// catch: a !`...` directive that passes $ARGUMENTS, which Claude Code never substitutes.
func TestCommandBashDirectiveGuardHasTeeth(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte("# bad\n\n!`bash x.sh \"$ARGUMENTS\"`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failures, err := checkCommandScripts(root, bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) == 0 {
		t.Fatal("expected a failure for a !`...` directive using $ARGUMENTS, got none")
	}
}
