package secrets

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// gitLogTimeout bounds one history scan; a huge repo with no -n limit must still terminate.
var gitLogTimeout = 60 * time.Second

// gitLogArgs builds the `git log -p` argv for dir, limited to since commits and/or rev when set.
func gitLogArgs(dir, rev string, since int) []string {
	args := []string{"-C", dir, "log", "-p", "--no-color", "--no-textconv", "--"}
	if since > 0 {
		args = append(args[:len(args)-1], "-n", strconv.Itoa(since), "--")
	}
	if rev != "" {
		args = append(args[:len(args)-1], rev, "--")
	}
	return args
}

// scanGitLogOutput walks `git log -p` output and returns one finding per secret-shaped added line.
// ReadLine fragments an over-long line (minified bundle, lockfile) instead of aborting the scan mid-pipe.
func scanGitLogOutput(stdout io.Reader) ([]string, error) {
	var findings []string
	commit, file := "", ""
	addition, cont := false, false
	br := bufio.NewReaderSize(stdout, 1<<20)
	for {
		frag, isPrefix, err := br.ReadLine()
		if err != nil {
			if err != io.EOF {
				return findings, err
			}
			return findings, nil
		}
		line := string(frag)
		if !cont {
			addition = false
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
				addition = true
			}
		}
		if addition && !isTestFixture(file) && slices.ContainsFunc(secretPattern.FindAllString(line, -1), tokenShaped) {
			findings = append(findings, fmt.Sprintf("%s %s", commit, file))
			addition = false // one finding per added line, however many fragments it spans
		}
		cont = isPrefix
	}
}

// ScanGit audits dir's git history for secret-shaped ADDITIONS (lines a commit introduced), never
// printing the matched line itself — only which commit and file introduced it. since caps the scan
// to the last N commits (0 = full history); rev limits it to a revision range (e.g. @{u}..HEAD). Read-only: runs `git log -p`, nothing else.
func ScanGit(out io.Writer, dir, rev string, since int) int {
	fmt.Fprintln(out, "== git history credential scan ==")
	inRepo, err := probeGitRepo(dir)
	if err != nil {
		fmt.Fprintf(out, "  ⚠ could not probe git repo: %v\n", err)
		return ExitFindings
	}
	if !inRepo {
		fmt.Fprintln(out, "  ✓ not inside a git repo — skipped")
		return ExitClean
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitLogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", gitLogArgs(dir, rev, since)...)
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

	findings, readErr := scanGitLogOutput(stdout)
	if readErr != nil {
		cancel() // unread output would block git on a full pipe until the timeout
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		fmt.Fprintf(out, "  ⚠ git log output truncated: %v\n", readErr)
		return ExitFindings
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
		fmt.Fprintln(out, "doctor --git: clean.")
		return ExitClean
	}
	for _, f := range findings {
		fmt.Fprintf(out, "  ⚠ %s — secret-shaped addition (rotate + scrub history)\n", f)
	}
	fmt.Fprintf(out, "doctor --git: %d finding(s).\n", len(findings))
	return ExitFindings
}

func probeGitRepo(dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitLogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	_, err := cmd.Output()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), "not a git repository") {
		return false, nil
	}
	return err == nil, err
}

// isTestFixture reports test/testdata paths, whose synthetic keys are noise (GitHub push protection skips them too).
func isTestFixture(p string) bool {
	if slices.Contains(strings.Split(p, "/"), "testdata") {
		return true
	}
	base := path.Base(p)
	for _, suf := range []string{"_test.go", "_test.py", ".test.ts", ".test.js", ".spec.ts", ".spec.js"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// tokenShaped rejects prose that merely starts like a key (`sk-before-changing-appearance`): a minted prefixed
// key always mixes digits in. A JWT (eyJ…) is base64url and may legitimately carry none, so it always counts.
func tokenShaped(match string) bool {
	return strings.HasPrefix(match, "eyJ") || strings.ContainsAny(match, "0123456789")
}
