package providers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// Exit codes shared by every audit (scan/git/doctor/all): 0 clean, 1 finding(s), 2 usage error.
const (
	ExitClean    = 0
	ExitFindings = 1
)

// base64ish catches high-entropy tokens that don't match a known prefix.
var base64ish = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_+/=-]{32,}$`) })

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
		return generic && base64ish().MatchString(val)
	}
}

// neverSecret reports values never flagged: empty, ${ENV} refs, file paths and obvious placeholders.
func neverSecret(val string) bool {
	switch {
	case val == "":
		return true
	case strings.HasPrefix(val, "$"):
		return true
	case strings.HasPrefix(val, "/") || strings.HasPrefix(val, "~/"):
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
	case strings.HasPrefix(val, "AKIA"):
		return true
	case strings.HasPrefix(val, "sk-"):
		return true
	}
	return strings.HasPrefix(val, "eyJ") && strings.Count(val, ".") >= 2
}

// secretPattern scans raw git diff lines; narrower than LooksLikeSecret (no bare base64 rule) to avoid noise.
var secretPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(
		`(` + textutil.GitHubPattern(20) + `|` +
			`xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9_-]{20,}|` +
			`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)
})

// gitLogTimeout bounds one history scan; a huge repo with no -n limit must still terminate.
var gitLogTimeout = 60 * time.Second

// gitLogArgs builds the `git log -p` argv for dir, limited to since commits and/or rev when set.
func gitLogArgs(dir, rev string, since int) []string {
	args := []string{"-C", dir, "-c", "core.quotepath=false", "log", "-p", "--no-color", "--no-textconv", "--"}
	if since > 0 {
		args = append(args[:len(args)-1], "-n", strconv.Itoa(since), "--")
	}
	if rev != "" {
		args = append(args[:len(args)-1], rev, "--")
	}
	return args
}

// scanGitLogOutput returns one finding per secret-shaped or workspace-identifier added line; deny is the host list.
func scanGitLogOutput(stdout io.Reader, deny []string) ([]string, error) {
	var findings []string
	commit, file := "", ""
	addition, hunk := false, false
	next := 0
	// fragments of one over-long line, joined before any match
	var pending []byte
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
		var lineNo int
		lineNo, next = newLineNo(line, hunk, addition, next)
		if !addition {
			continue
		}
		fixture := isTestFixture(file)
		if !fixture && slices.ContainsFunc(secretPattern().FindAllString(line, -1), tokenShaped) {
			findings = append(findings, fmt.Sprintf("%s %s", commit, file))
		}
		for _, kind := range identifierKinds(line, deny, fixture) {
			findings = append(findings, fmt.Sprintf("%s %s:%d — %s", commit, file, lineNo, kind))
		}
	}
}

var hunkNewStart = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)

// newLineNo tracks the new-file line number: it is the number of an added line, and next is the one after it.
func newLineNo(line string, hunk, addition bool, next int) (int, int) {
	if m := hunkNewStart.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[1])
		return 0, n
	}
	if addition {
		return next, next + 1
	}
	if hunk && strings.HasPrefix(line, " ") {
		return 0, next + 1
	}
	return 0, next
}

// parseGitLogLine advances commit, file and hunk state and reports an added line; "+++ " occurs only before a hunk.
func parseGitLogLine(line, commit, file string, hunk bool) (string, string, bool, bool) {
	switch {
	case strings.HasPrefix(line, "commit "):
		commit = strings.TrimPrefix(line, "commit ")
		if len(commit) > 12 {
			commit = commit[:12]
		}
		return commit, file, false, false
	case strings.HasPrefix(line, "diff --git "):
		_, file, _ = strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/")
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
	// a leading '-' would be parsed by git log as an option
	if strings.HasPrefix(rev, "-") {
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
		fmt.Fprintln(out, "  ✓ no secret-shaped or workspace-identifier additions in scanned history")
		fmt.Fprintln(out, "doctor --git: clean.")
		return ExitClean
	}
	for _, f := range findings {
		if strings.Contains(f, " — ") {
			fmt.Fprintf(out, "  ⚠ %s (value withheld; rotate or scrub before pushing)\n", f)
			continue
		}
		fmt.Fprintf(out, "  ⚠ %s — secret-shaped addition (rotate + scrub history)\n", f)
	}
	fmt.Fprintf(out, "doctor --git: %d finding(s).\n", len(findings))
	return ExitFindings
}

// gitLogFindings runs and scans `git log -p`; ok is false once a failure line has been written to out.
func gitLogFindings(out io.Writer, dir, rev string, since int) (findings []string, ok bool) {
	deny, denyErr := denyHosts()
	if denyErr != nil {
		msg := workspaceHost.ReplaceAllString(denyErr.Error(), "<host>")
		fmt.Fprintf(out, "  ⚠ configured-host check disabled, only shape rules ran: %s\n", msg)
	}
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

	findings, readErr := scanGitLogOutput(stdout, deny)
	if readErr != nil {
		// unread output would block git on a full pipe until the timeout
		cancel()
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

// tokenShaped rejects prose that merely starts like a key, since a minted key mixes digits; a JWT always counts.
func tokenShaped(match string) bool {
	return strings.HasPrefix(match, "eyJ") || strings.ContainsAny(match, "0123456789")
}

const hostDomains = `(?:azuredatabricks\.net|cloud\.databricks\.com|gcp\.databricks\.com)`

var (
	workspaceHost = regexp.MustCompile(
		`(?i)\b(?:[a-z0-9][a-z0-9-]*\.)*[a-z0-9][a-z0-9-]*\.` + hostDomains + `\b`)
	workspaceOrgID = regexp.MustCompile(`(?i)databricks[^\s"'<>]*[?&]o=\d{6,}`)
	// hostShape matches the live-workspace names anywhere on a line, so an escape or extra label cannot hide one.
	hostShape = regexp.MustCompile(`(?i)(?:adb-\d{10,}|dbc-[0-9a-f]{8}-[0-9a-f]{4})[a-z0-9.-]*\.` + hostDomains)
)

// placeholderNames are the profile-name labels the docs and fixtures use for a fake workspace host.
var placeholderNames = []string{"example", "dev", "qa", "prod", "x", "accounts", "community"}

// realWorkspaceHost reports a named host that is no placeholder; adb-/dbc- shapes are hostShape's.
func realWorkspaceHost(host string) bool {
	host = strings.ToLower(host)
	if strings.HasSuffix(host, ".azuredatabricks.net") || strings.HasPrefix(host, "dbc-") {
		return false
	}
	return !slices.Contains(placeholderNames, strings.Split(host, ".")[0]) && !strings.Contains(host, "example")
}

// denyHosts lists every configured Databricks profile host (never a public commit); the error is the registry load's.
var denyHosts = func() ([]string, error) {
	reg, err := LoadShared()
	if err != nil || reg == nil {
		return nil, err
	}
	var out []string
	for _, p := range reg.Providers {
		if p.Preset != "databricks" && p.Provider != "databricks" {
			continue
		}
		if host := denyHost(AuthHost(p)); host != "" {
			out = append(out, host)
		}
	}
	return out, nil
}

// denyHost is rawURL's lower-cased host, empty for a bare name or an example host; nothing else is exempt.
func denyHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	host := strings.ToLower(u.Hostname())
	if err != nil || !strings.Contains(host, ".") || strings.Contains(host, "example") {
		return ""
	}
	return host
}

// identifierKinds names the kinds of real workspace identifiers on an added line, never their values.
func identifierKinds(line string, deny []string, fixture bool) []string {
	var kinds []string
	named := slices.ContainsFunc(workspaceHost.FindAllString(line, -1), realWorkspaceHost)
	if !fixture && (named || hostShape.MatchString(line)) {
		kinds = append(kinds, "real-shaped Databricks workspace host")
	}
	if !fixture && workspaceOrgID.MatchString(line) {
		kinds = append(kinds, "Databricks workspace id in a URL")
	}
	lower := strings.ToLower(line)
	if slices.ContainsFunc(deny, func(h string) bool { return strings.Contains(lower, h) }) {
		kinds = append(kinds, "host of a configured Databricks profile")
	}
	return kinds
}
