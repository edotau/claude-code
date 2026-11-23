package autofix

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
)

//go:embed briefs/fix.tmpl
var leafBrief string

var (
	// ErrAttempted means the same sha and finding set already had a leaf; the budget is not spent twice.
	ErrAttempted = errors.New("autofix findings already attempted")
	ErrBudget    = errors.New("autofix budget refused")
	attemptFile  = func() string { return filepath.Join(paths.StateDir(), "autofix", "attempts.jsonl") }
	launchLeaf   = execLaunch // a test swaps it; nothing else may launch
)

// LeafRequest is one commit's findings: Lane is the autofix-<sha7> worktree, Live the checkout the leaf must not write.
type LeafRequest struct {
	Sha, Lane, Live, Gate string
	Findings              []Finding
}

// Attempt is one attempts.jsonl row; it never stores gate output or brief text.
type Attempt struct {
	At         time.Time `json:"at"`
	Sha        string    `json:"sha"`
	Key        string    `json:"key"`
	Classes    []Class   `json:"classes"`
	Paths      []string  `json:"paths"`
	Verdict    string    `json:"verdict"`
	DispatchID string    `json:"dispatch_id,omitempty"`
}

// LeafArgs is the one `agents ask` line, the brief on stdin; the deny keeps the leaf's writes in the lane.
func LeafArgs(req LeafRequest) []string {
	deny, _ := json.Marshal(map[string][]string{"deny": {"Edit(" + req.Live + "/**)", "Write(" + req.Live + "/**)"}})
	return []string{"agents", "ask", "--background", "--format", "json", "--workdir", req.Lane,
		"--provider", "databricks", "--tools", "--permissions", string(deny), "-"}
}

// LocalFindings drops findings whose path is not a plain repo-relative file name; dropped counts them.
func LocalFindings(in []Finding) (kept []Finding, dropped int) {
	kept = slices.DeleteFunc(slices.Clone(in), func(f Finding) bool { return f.Path != "" && !localPath(f.Path) })
	return kept, len(in) - len(kept)
}

// safePath is the charset a path may use where it is named in Owned: and the Verify command line.
var safePath = regexp.MustCompile(`^[\w.+/-]+$`)

func localPath(p string) bool {
	return safePath.MatchString(p) && filepath.IsLocal(p) && !strings.HasPrefix(p, "-") && path.Clean(p) == p
}

// Attempts reads the attempts ledger; a missing ledger is empty, a corrupt row an error.
func Attempts() ([]Attempt, error) { return readAttempts(attemptFile()) }

// FixLeaf launches one fix leaf for the judgment findings of req.Sha, at most once per sha and finding set.
func FixLeaf(ctx context.Context, req LeafRequest, budget func() (ok bool, why string)) (Attempt, error) {
	findings, _ := LocalFindings(req.Findings)
	findings = slices.DeleteFunc(findings, func(f Finding) bool { return f.Class.Deterministic() })
	if len(findings) == 0 {
		return Attempt{}, nil
	}
	if req.Sha == "" || req.Lane == "" || req.Live == "" || req.Gate == "" {
		return Attempt{}, errors.New("autofix leaf needs a sha, lane, live checkout and gate")
	}
	file := attemptFile()
	var row Attempt
	// The outer lock spans read, launch and append, so two callers never launch for the same key.
	err := paths.WithFileLock(strings.TrimSuffix(file, ".jsonl"), func() error {
		var err error
		row, err = attemptLocked(ctx, file, req, findings, budget)
		return err
	})
	return row, err
}

func attemptLocked(ctx context.Context, file string, req LeafRequest, findings []Finding,
	budget func() (bool, string)) (Attempt, error) {
	row := newAttempt(req.Sha, findings)
	prior, err := readAttempts(file)
	if err != nil {
		return Attempt{}, err
	}
	// the last row of a key is its outcome: dispatching is superseded by dispatched or launch-failed
	if i := lastIndex(prior, row.Key); i >= 0 {
		return prior[i], ErrAttempted
	}
	if ok, why := budget(); !ok {
		return Attempt{}, fmt.Errorf("%w: %s", ErrBudget, why)
	}
	brief, err := renderLeaf(req, findings, row.Paths)
	if err != nil {
		return Attempt{}, err
	}
	row.Verdict = "dispatching" // written before the launch: a crash after it blocks a retry, never launches unrecorded
	if err := appendAttempt(file, row); err != nil {
		return Attempt{}, err
	}
	row.Verdict = "dispatched"
	row.DispatchID, err = launchLeaf(ctx, req.Lane, brief, LeafArgs(req))
	if err != nil {
		row.Verdict, row.DispatchID = "launch-failed", "" // recorded too: a failed launch is not retried either
	}
	return row, errors.Join(err, appendAttempt(file, row))
}

func lastIndex(rows []Attempt, key string) int {
	for i, row := range slices.Backward(rows) {
		if row.Key == key {
			return i
		}
	}
	return -1
}

func appendAttempt(file string, row Attempt) error {
	data, err := json.Marshal(row)
	if err == nil {
		err = paths.AppendLine(file, data)
	}
	if err != nil {
		return fmt.Errorf("record autofix attempt: %w", err)
	}
	return nil
}

// newAttempt keys on the sha and the sorted class/path pairs, so finding order and line moves do not matter.
func newAttempt(sha string, findings []Finding) Attempt {
	var classes []Class
	var files, pairs []string
	for _, f := range findings {
		classes = append(classes, f.Class)
		pairs = append(pairs, string(f.Class)+"\t"+f.Path)
		if f.Path != "" {
			files = append(files, f.Path)
		}
	}
	slices.Sort(classes)
	slices.Sort(files)
	slices.Sort(pairs)
	sum := sha256.Sum256([]byte(sha + "\n" + strings.Join(slices.Compact(pairs), "\n")))
	return Attempt{At: time.Now().UTC(), Sha: sha, Key: hex.EncodeToString(sum[:]),
		Classes: slices.Compact(classes), Paths: slices.Compact(files)}
}

func readAttempts(file string) ([]Attempt, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read autofix attempts: %w", err)
	}
	defer f.Close()
	var rows []Attempt
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for line := 1; sc.Scan(); line++ {
		var row Attempt
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, line, err)
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

// maxDataRunes caps one finding message in the brief's data block.
const maxDataRunes = 200

func renderLeaf(req LeafRequest, findings []Finding, owned []string) (string, error) {
	encoded, err := json.Marshal(owned)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("autofix brief nonce: %w", err)
	}
	fence := "AUTOFIX-DATA-" + hex.EncodeToString(nonce)
	data := make([]Finding, len(findings))
	for i, f := range findings {
		data[i] = Finding{Class: f.Class, Path: f.Path, Line: f.Line,
			Rule: dataLine(f.Rule, fence), Message: dataLine(f.Message, fence)}
	}
	tmpl, err := template.New("fix").Option("missingkey=error").Parse(leafBrief)
	if err != nil {
		return "", fmt.Errorf("parse autofix brief: %w", err)
	}
	policy := slices.ContainsFunc(owned, func(p string) bool { // hook/permission fixes change cost only (R27)
		p = filepath.ToSlash(p)
		return strings.HasPrefix(p, "internal/hooks/") || strings.HasPrefix(p, "internal/permission/")
	})
	var out bytes.Buffer
	err = tmpl.Execute(&out, struct {
		LeafRequest
		Owned, Fence string
		Judgment     []Finding
		Policy       bool
	}{req, string(encoded), fence, data, policy})
	if err != nil {
		return "", fmt.Errorf("render autofix brief: %w", err)
	}
	return out.String(), nil
}

// dataLine defangs tool text for the data block: one line, no control runes, backticks or fence, capped.
func dataLine(s, fence string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '`' {
			return ' '
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, fence, "")
	s = strings.ReplaceAll(s, "AUTOFIX-DATA-", "")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxDataRunes {
		s = string(r[:maxDataRunes]) + "…"
	}
	return s
}

// execLaunch starts the background job and returns its id without waiting; the gate rerun is the caller's.
func execLaunch(ctx context.Context, dir, brief string, args []string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate claude-code: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(brief)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("launch autofix leaf: %w\n%s", err, stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(out), []byte{'\n'})
	var job struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &job); err != nil || job.ID == "" {
		return "", fmt.Errorf("autofix launch: no job id in %q", out)
	}
	return job.ID, nil
}
