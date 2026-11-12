package providers

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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// Exit codes shared by every audit (scan/git/doctor/all): 0 clean, 1 finding(s), 2 usage error.
const (
	ExitClean    = 0
	ExitFindings = 1
)

// base64ish catches high-entropy tokens that don't match a known prefix.
var base64ish = regexp.MustCompile(`^[A-Za-z0-9_+/=-]{32,}$`)

// LooksLikeSecret is the credential-shape heuristic for one isolated value; ${ENV} refs and placeholders never match.
func LooksLikeSecret(val string) bool {
	return LooksLikeSecretGeneric(val, true)
}

// LooksLikeSecretGeneric is LooksLikeSecret with the bare base64-ish rule switchable via generic.
func LooksLikeSecretGeneric(val string, generic bool) bool {
	switch {
	case neverSecret(val):
		return false
	case knownSecretShape(val):
		return true
	default:
		return generic && base64ish.MatchString(val)
	}
}

// neverSecret reports values never flagged: empty, ${ENV} refs, file paths and obvious placeholders.
func neverSecret(val string) bool {
	switch {
	case val == "":
		return true
	case strings.HasPrefix(val, "$"): // ${ENV} or $ENV reference
		return true
	case strings.HasPrefix(val, "/") || strings.HasPrefix(val, "~/"): // file path
		return true
	}
	return strings.ContainsAny(val, "<>") || strings.Contains(val, "PLACEHOLDER") ||
		strings.Contains(val, "YOUR_") || strings.Contains(val, "xxx")
}

// knownSecretShape reports a known token prefix or a JWT shape.
func knownSecretShape(val string) bool {
	switch {
	case textutil.HasGitHubPrefix(val), textutil.IsSlackToken(val):
		return true
	case strings.HasPrefix(val, "AKIA"): // AWS
		return true
	case strings.HasPrefix(val, "sk-"): // OpenAI-style
		return true
	}
	return strings.HasPrefix(val, "eyJ") && strings.Count(val, ".") >= 2 // JWT
}

// secretPattern scans raw git diff lines; narrower than LooksLikeSecret (no bare base64 rule) to avoid noise.
var secretPattern = regexp.MustCompile(
	`(` + textutil.GitHubPattern(20) + `|` +
		`xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9_-]{20,}|` +
		`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)

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
	addition, hunk := false, false
	var pending []byte // fragments of one over-long line, joined before any match
	br := bufio.NewReaderSize(stdout, 1<<20)
	for {
		frag, isPrefix, err := br.ReadLine()
		if err == io.EOF {
			return findings, nil
		}
		if err != nil {
			return findings, err
		}
		pending = append(pending, frag...)
		if isPrefix {
			continue
		}
		line := string(pending)
		pending = pending[:0]
		commit, file, addition, hunk = parseGitLogLine(line, commit, file, hunk)
		if addition && !isTestFixture(file) && slices.ContainsFunc(secretPattern.FindAllString(line, -1), tokenShaped) {
			findings = append(findings, fmt.Sprintf("%s %s", commit, file))
		}
	}
}

// parseGitLogLine advances commit, file and hunk state from a line, and reports whether it is an added (+) line.
// A "+++ " file header only occurs before the first hunk; inside one, "++…" content is an addition.
func parseGitLogLine(line, commit, file string, hunk bool) (string, string, bool, bool) {
	switch {
	case strings.HasPrefix(line, "commit "):
		commit = strings.TrimPrefix(line, "commit ")
		if len(commit) > 12 {
			commit = commit[:12]
		}
		return commit, file, false, false
	case strings.HasPrefix(line, "diff --git "):
		if _, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/"); ok {
			file = b
		}
		return commit, file, false, false
	case strings.HasPrefix(line, "@@"):
		return commit, file, false, true
	case strings.HasPrefix(line, "+"):
		return commit, file, hunk || !strings.HasPrefix(line, "+++ "), hunk
	}
	return commit, file, false, hunk
}

// ScanGit reports secret-shaped additions in dir's history by commit and file, never the line; since caps commits.
func ScanGit(out io.Writer, dir, rev string, since int) int {
	fmt.Fprintln(out, "== git history credential scan ==")
	if strings.HasPrefix(rev, "-") { // a leading '-' would be parsed by git log as an option
		fmt.Fprintf(out, "  ⚠ invalid rev %q: must not start with '-'\n", rev)
		return ExitFindings
	}
	inRepo, err := probeGitRepo(dir)
	if err != nil {
		fmt.Fprintf(out, "  ⚠ could not probe git repo: %v\n", err)
		return ExitFindings
	}
	if !inRepo {
		fmt.Fprintln(out, "  ✓ not inside a git repo — skipped")
		return ExitClean
	}
	findings, ok := gitLogFindings(out, dir, rev, since)
	if !ok {
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

// gitLogFindings runs and scans `git log -p`; ok is false once a failure line has been written to out.
func gitLogFindings(out io.Writer, dir, rev string, since int) (findings []string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitLogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", gitLogArgs(dir, rev, since)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(out, "  ⚠ could not run git log: %v\n", err)
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(out, "  ⚠ could not run git log: %v\n", err)
		return nil, false
	}

	findings, readErr := scanGitLogOutput(stdout)
	if readErr != nil {
		cancel() // unread output would block git on a full pipe until the timeout
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		fmt.Fprintf(out, "  ⚠ git log output truncated: %v\n", readErr)
		return nil, false
	}
	if ctx.Err() != nil {
		fmt.Fprintln(out, "  ⚠ git log timed out — history not fully scanned")
		return nil, false
	}
	if waitErr != nil {
		fmt.Fprintf(out, "  ⚠ git log failed: %v: %s\n", waitErr, strings.TrimSpace(stderr.String()))
		return nil, false
	}
	return findings, true
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
