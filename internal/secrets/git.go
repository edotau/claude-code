package secrets

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// gitLogTimeout bounds one history scan; a huge repo with no -n limit must still terminate.
var gitLogTimeout = 60 * time.Second

// ScanGit audits dir's git history for secret-shaped ADDITIONS (lines a commit introduced), never
// printing the matched line itself — only which commit and file introduced it. since caps the scan
// to the last N commits (0 = full history). Read-only: runs `git log -p`, nothing else.
func ScanGit(out io.Writer, dir string, since int) int {
	fmt.Fprintln(out, "== git history credential scan ==")
	if exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run() != nil {
		fmt.Fprintln(out, "  ✓ not inside a git repo — skipped")
		return ExitClean
	}
	args := []string{"-C", dir, "log", "-p", "--no-color", "--"}
	if since > 0 {
		args = append(args[:len(args)-1], "-n", fmt.Sprintf("%d", since), "--")
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitLogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(out, "  ⚠ could not run git log: %v\n", err)
		return ExitFindings
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(out, "  ⚠ could not run git log: %v\n", err)
		return ExitFindings
	}

	var findings []string
	commit, file := "", ""
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "commit "):
			commit = strings.TrimPrefix(line, "commit ")
			if len(commit) > 12 {
				commit = commit[:12]
			}
		case strings.HasPrefix(line, "diff --git "):
			if _, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/"); ok {
				file = b
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if secretPattern.MatchString(line) {
				findings = append(findings, fmt.Sprintf("%s %s", commit, file))
			}
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		fmt.Fprintf(out, "  ⚠ git log output truncated: %v\n", scanErr)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(out, "  ⚠ git log timed out — history not fully scanned")
		return ExitFindings
	}
	if waitErr != nil {
		fmt.Fprintf(out, "  ⚠ git log failed: %v: %s\n", waitErr, strings.TrimSpace(stderr.String()))
		return ExitFindings
	}
	if len(findings) == 0 {
		fmt.Fprintln(out, "  ✓ no secret-shaped additions in scanned history")
		fmt.Fprintln(out, "secrets git: clean.")
		return ExitClean
	}
	for _, f := range findings {
		fmt.Fprintf(out, "  ⚠ %s — secret-shaped addition (rotate + scrub history)\n", f)
	}
	fmt.Fprintf(out, "secrets git: %d finding(s).\n", len(findings))
	return ExitFindings
}
