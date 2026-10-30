package hooks

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/review/source"
)

const (
	simplicityMaxFile  = 1 << 20
	simplicityMaxLines = 8
	simplicityMaxChars = 1000
	simplicityFixHint  = "Fix per rules/standards/quality.md → Comments; patterns: " +
		"skills/simplify/references/simplification-patterns.md"
)

// ReviewSimplicity is the PostToolUse(Write|Edit) advisory: findings the edit introduced, never blocking.
func ReviewSimplicity(r io.Reader, stdout, _ io.Writer) int {
	in := ParseInput(r)
	path, before, after, ok := editContents(in)
	if !ok {
		return ExitProceed
	}
	found := source.SimplicityFindings(path, before, after)
	if len(found) == 0 {
		return ExitProceed
	}
	rel := path
	if in.CWD != "" {
		if p, err := filepath.Rel(in.CWD, path); err == nil && !strings.HasPrefix(p, "..") {
			rel = p
		}
	}
	fmt.Fprint(stdout, MergeContext("PostToolUse", []string{simplicityText(rel, found)}, in.Vendor()))
	return ExitProceed
}

// editContents is the edited file's path and its content before and after this Write or Edit.
func editContents(in *Input) (path, before, after string, ok bool) {
	path = in.nested("tool_input", "file_path")
	if path == "" || (in.ToolName != "Write" && in.ToolName != "Edit") {
		return "", "", "", false
	}
	if info, err := os.Stat(path); err != nil || info.Size() > simplicityMaxFile {
		return "", "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", false
	}
	after = string(data)
	if in.ToolName == "Write" {
		return path, gitHeadContent(path), after, true
	}
	before = undoEdit(after, in.nested("tool_input", "old_string"), in.nested("tool_input", "new_string"),
		in.ToolInput()["replace_all"] == true)
	return path, before, after, true
}

// undoEdit reverses an Edit on the post-edit content: new_string back to old_string (first or all occurrences).
func undoEdit(after, oldStr, newStr string, all bool) string {
	if newStr == "" {
		return after
	}
	n := 1
	if all {
		n = -1
	}
	return strings.Replace(after, newStr, oldStr, n)
}

// gitHeadContent is the file at HEAD, or "" when untracked or outside a repo.
func gitHeadContent(path string) string {
	cmd := exec.Command("git", "show", "HEAD:./"+filepath.Base(path))
	cmd.Dir = filepath.Dir(path)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// simplicityText renders the header plus up to simplicityMaxLines findings within simplicityMaxChars.
func simplicityText(rel string, found []string) string {
	n := len(found)
	var b strings.Builder
	fmt.Fprintf(&b, "review-simplicity: %d new finding(s) in %s:", n, rel)
	for i, f := range found {
		line := "\n- " + f
		if i >= simplicityMaxLines || b.Len()+len(line) > simplicityMaxChars {
			fmt.Fprintf(&b, "\n(+%d more)", n-i)
			break
		}
		b.WriteString(line)
	}
	b.WriteString("\n" + simplicityFixHint)
	return b.String()
}
