package quality

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
)

const (
	simplicityMaxFile  = 1 << 20
	simplicityMaxLines = 8
	simplicityMaxChars = 1000
	simplicityFixHint  = "Fix per rules/standards/quality.md → Comments, Simplify (built-in /simplify)"
)

// ReviewSimplicity is the PostToolUse(Write|Edit) advisory: findings the edit introduced, never blocking.
func ReviewSimplicity(in *core.Input) core.Result {
	path, before, after, ok := editContents(in)
	if !ok {
		return core.Result{}
	}
	found := source.SimplicityFindings(path, before, after)
	if len(found) == 0 {
		return core.Result{}
	}
	rel := path
	if in.CWD != "" {
		if p, err := filepath.Rel(in.CWD, path); err == nil && !strings.HasPrefix(p, "..") {
			rel = p
		}
	}
	return core.Result{Context: simplicityText(rel, found)}
}

// editContents is the edited file's path and its content before and after this Write or Edit.
func editContents(in *core.Input) (path, before, after string, ok bool) {
	path = in.Nested("tool_input", "file_path")
	if path != "" && !filepath.IsAbs(path) && in.CWD != "" {
		path = filepath.Join(in.CWD, path)
	}
	if path == "" || (in.ToolName != "Write" && in.ToolName != "Edit") {
		return "", "", "", false
	}
	after, ok = readSmallFile(path)
	if !ok {
		return "", "", "", false
	}
	if in.ToolName == "Write" {
		return path, gitHeadContent(path), after, true
	}
	before, ok = editBefore(in, after)
	return path, before, after, ok
}

// editBefore reverses an Edit on its post-edit content; false when the edit's occurrence is ambiguous.
func editBefore(in *core.Input, after string) (string, bool) {
	oldStr, newStr, all := in.Nested("tool_input", "old_string"), in.Nested("tool_input", "new_string"),
		in.ToolInput()["replace_all"] == true
	if !all && newStr != "" && strings.Count(after, newStr) > 1 {
		return "", false // ambiguous: cannot tell which occurrence the edit wrote
	}
	return undoEdit(after, oldStr, newStr, all), true
}

// readSmallFile returns the file's content unless it is unreadable or over simplicityMaxFile.
func readSmallFile(path string) (string, bool) {
	if info, err := os.Stat(path); err != nil || info.Size() > simplicityMaxFile {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := proc.RunGitRaw(ctx, filepath.Dir(path), "show", "HEAD:./"+filepath.Base(path))
	if err != nil {
		return ""
	}
	return out
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
