package quality

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	if p, ok := core.RelWithin(in.CWD, path); ok {
		rel = p
	}
	return core.Result{Context: simplicityText(rel, found)}
}

// editContents is the edited file's path and its content before and after this Write or Edit.
func editContents(in *core.Input) (path, before, after string, ok bool) {
	path = core.AnchorPath(in.Nested("tool_input", "file_path"), in.CWD)
	if path == "" || (in.ToolName != "Write" && in.ToolName != "Edit") || source.DetectLanguage(path) == "" {
		return "", "", "", false
	}
	after, ok = readSmallFile(path)
	if !ok {
		return "", "", "", false
	}
	if in.ToolName == "Write" {
		before, ok = gitHeadContent(path)
		return path, before, after, ok
	}
	before, ok = editBefore(in, after)
	return path, before, after, ok
}

// editBefore reverses an Edit on its post-edit content; false when the edit is a deletion or its occurrence is ambiguous.
func editBefore(in *core.Input, after string) (string, bool) {
	oldStr, newStr, all := in.Nested("tool_input", "old_string"), in.Nested("tool_input", "new_string"),
		in.ToolInput()["replace_all"] == true
	if newStr == "" {
		return "", false // a deletion leaves no marker to locate where old_string was
	}
	if !all && strings.Count(after, newStr) > 1 {
		return "", false // ambiguous: cannot tell which occurrence the edit wrote
	}
	n := 1
	if all {
		n = -1
	}
	return strings.Replace(after, newStr, oldStr, n), true
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

// gitShowHead runs git for gitHeadContent; a test swaps it to count calls.
var gitShowHead = gitHeadRaw

// gitHeadRaw fixes git's diagnostic locale without changing the shared process environment.
func gitHeadRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(proc.GitEnv(), "LC_ALL=C")
	cmd.WaitDelay = proc.ExecWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// gitHeadContent is the file at HEAD ("" when untracked or outside a repo); false when git failed for another reason.
func gitHeadContent(path string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := gitShowHead(ctx, filepath.Dir(path), "show", "HEAD:./"+filepath.Base(path))
	if err == nil {
		return out, true
	}
	for _, m := range []string{"not in 'HEAD'", "does not exist in 'HEAD'", "not a git repository", "invalid object name 'HEAD'"} {
		if ctx.Err() == nil && strings.Contains(err.Error(), m) {
			return "", true
		}
	}
	return "", false
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
