package autofix

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

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
	Sha, Lane, Live, Gate, GateOutput string
	Findings                          []Finding
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
	return []string{"agents", "ask", "--background", "--format", "json", "--workdir", req.Lane,
		"--provider", "databricks", "--tools", "--deny", "Edit(" + req.Live + "/**)",
		"--deny", "Write(" + req.Live + "/**)", "-"}
}

// FixLeaf launches one fix leaf for the judgment findings of req.Sha, at most once per sha and finding set.
func FixLeaf(ctx context.Context, req LeafRequest, budget func() (ok bool, why string)) (Attempt, error) {
	findings := slices.DeleteFunc(slices.Clone(req.Findings), func(f Finding) bool { return f.Class.Deterministic() })
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
	if i := slices.IndexFunc(prior, func(a Attempt) bool { return a.Key == row.Key }); i >= 0 {
		return prior[i], ErrAttempted
	}
	if ok, why := budget(); !ok {
		return Attempt{}, fmt.Errorf("%w: %s", ErrBudget, why)
	}
	brief, err := renderLeaf(req, findings, row.Paths)
	if err != nil {
		return Attempt{}, err
	}
	row.Verdict = "dispatched"
	row.DispatchID, err = launchLeaf(ctx, req.Lane, brief, LeafArgs(req))
	if err != nil {
		row.Verdict, row.DispatchID = "launch-failed", "" // recorded too: a failed launch is not retried either
	}
	data, marshalErr := json.Marshal(row)
	if marshalErr == nil {
		marshalErr = paths.AppendLine(file, data)
	}
	if marshalErr != nil {
		return row, errors.Join(err, fmt.Errorf("record autofix attempt: %w", marshalErr))
	}
	return row, err
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

func renderLeaf(req LeafRequest, findings []Finding, owned []string) (string, error) {
	encoded, err := json.Marshal(owned)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("fix").Funcs(template.FuncMap{"line": func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}}).Option("missingkey=error").Parse(leafBrief)
	if err != nil {
		return "", fmt.Errorf("parse autofix brief: %w", err)
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, struct {
		LeafRequest
		Owned    string
		Judgment []Finding
	}{req, string(encoded), findings})
	if err != nil {
		return "", fmt.Errorf("render autofix brief: %w", err)
	}
	return out.String(), nil
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
