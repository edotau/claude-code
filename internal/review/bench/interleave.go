package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/source"
)

// ABOptions is an interleaved run: A is the parent worktree, B the fix; rounds alternate A, B, A, B, ...
type ABOptions struct {
	A, B   string // worktree dirs, both clean and at different commits
	Pkg    string // package dir relative to a worktree, e.g. internal/hooks/safety
	Bench  string // -bench regexp; "" is "."
	Rounds int    // A/B pairs; at least 1
	Count  int    // samples per benchmark per round; at least 1
	Work   string // dir for the round files and the merged before.txt and after.txt; "" makes a temp dir
}

// ABResult names both commits, the merged run files, and the verdict decided on them.
type ABResult struct {
	CommitA, CommitB string
	Before, After    string
	Decision         Decision
}

// abSeam is what an interleaved run touches outside the process; tests stub it.
type abSeam struct {
	commit func(dir string) (string, error)
	dirty  func(dir string) (bool, error)
	bench  func(ctx context.Context, dir string, o ABOptions, out string) error
	decide func(before, after string) (Decision, error)
}

var abBusy sync.Mutex // one benchmark process at a time in this process

// Interleave runs the A/B benchmarks one process at a time, merges each side's rounds, and decides.
func Interleave(ctx context.Context, o ABOptions, log io.Writer) (ABResult, error) {
	return interleave(ctx, o, abSeam{gitCommit, gitDirty, execBench, decideFiles}, log)
}

func interleave(ctx context.Context, o ABOptions, s abSeam, log io.Writer) (ABResult, error) {
	if !abBusy.TryLock() {
		return ABResult{}, errors.New("interleave: another interleaved run is active")
	}
	defer abBusy.Unlock()
	o, err := o.normalized()
	if err != nil {
		return ABResult{}, err
	}
	res, err := preflight(o, s)
	if err != nil {
		return res, err
	}
	fmt.Fprintf(log, "interleave: A %s (%s) vs B %s (%s), %d rounds x count %d\n",
		res.CommitA, o.A, res.CommitB, o.B, o.Rounds, o.Count)
	as, bs, err := runRounds(ctx, o, s, res, log)
	if err != nil {
		return res, err
	}
	res.Before, res.After = filepath.Join(o.Work, "before.txt"), filepath.Join(o.Work, "after.txt")
	if err := errors.Join(mergeRuns(res.Before, as), mergeRuns(res.After, bs)); err != nil {
		return res, err
	}
	res.Decision, err = s.decide(res.Before, res.After)
	return res, err
}

// normalized resolves the worktree dirs (the repo top is symlink-resolved) and defaults the rest.
func (o ABOptions) normalized() (ABOptions, error) {
	if o.Pkg == "" || filepath.IsAbs(o.Pkg) || o.Rounds < 1 || o.Count < 1 {
		return o, errors.New("interleave: need a relative Pkg, Rounds >= 1 and Count >= 1")
	}
	for _, dir := range []*string{&o.A, &o.B} {
		abs, err := canonicalWorktree(*dir)
		if err != nil {
			return o, err
		}
		*dir = abs
	}
	if o.A == o.B {
		return o, errors.New("interleave: A and B are the same worktree")
	}
	if o.Bench == "" {
		o.Bench = "."
	}
	var err error
	o.Work, err = ensureWorkDir(o.Work)
	return o, err
}

func ensureWorkDir(work string) (string, error) {
	if work == "" {
		w, err := os.MkdirTemp("", "bench-ab-")
		if err != nil {
			return "", err
		}
		work = w
	}
	return work, os.MkdirAll(work, 0o755)
}

func canonicalWorktree(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", fmt.Errorf("interleave: worktree: %w", err)
	}
	return abs, nil
}

// preflight refuses a dirty worktree, or two worktrees at one commit, before any benchmark starts.
func preflight(o ABOptions, s abSeam) (ABResult, error) {
	var res ABResult
	for i, d := range []string{o.A, o.B} {
		dirty, err := s.dirty(d)
		if err != nil {
			return res, fmt.Errorf("interleave: %s: %w", d, err)
		}
		if dirty {
			return res, fmt.Errorf("interleave: worktree %s is dirty; commit the fix first", d)
		}
		sha, err := s.commit(d)
		if err != nil {
			return res, fmt.Errorf("interleave: %s: %w", d, err)
		}
		if i == 0 {
			res.CommitA = sha
		} else {
			res.CommitB = sha
		}
	}
	if res.CommitA == res.CommitB {
		return res, fmt.Errorf("interleave: A and B are both at %s; nothing to compare", res.CommitA)
	}
	return res, nil
}

// runRounds runs A then B per round, never two at once, and returns each side's round files.
func runRounds(ctx context.Context, o ABOptions, s abSeam, res ABResult, log io.Writer) (as, bs []string, err error) {
	for r := 1; r <= o.Rounds; r++ {
		for _, side := range []struct {
			name, dir, sha string
			files          *[]string
		}{{"a", o.A, res.CommitA, &as}, {"b", o.B, res.CommitB, &bs}} {
			out := filepath.Join(o.Work, fmt.Sprintf("%s-%d.txt", side.name, r))
			fmt.Fprintf(log, "interleave: round %d/%d %s %s\n", r, o.Rounds, strings.ToUpper(side.name), side.sha)
			if err := s.bench(ctx, side.dir, o, out); err != nil {
				return nil, nil, err
			}
			if err := checkRound(out, side.sha); err != nil {
				return nil, nil, err
			}
			*side.files = append(*side.files, out)
		}
	}
	return as, bs, nil
}

// checkRound confirms a round's run text came from the expected clean commit.
func checkRound(file, sha string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	text := string(b)
	if !strings.Contains(text, "\n# commit: "+sha+"\n") && !strings.HasPrefix(text, "# commit: "+sha+"\n") {
		return fmt.Errorf("interleave: %s is not a run of commit %s", file, sha)
	}
	if strings.Contains(text, "\n# dirty: true\n") {
		return fmt.Errorf("interleave: %s ran on a dirty tree", file)
	}
	return nil
}

// mergeRuns writes dst: the first round whole, then every later round's samples and load lines.
func mergeRuns(dst string, files []string) error {
	var sb strings.Builder
	var first string
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if i == 0 {
			first = string(b)
			sb.WriteString(strings.TrimSuffix(first, "\n") + "\n")
			continue
		}
		if !sameEpochs(first, string(b)) {
			return fmt.Errorf("interleave: benchmark epochs changed between rounds (%s)", f)
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			if strings.HasPrefix(line, "Benchmark") || headerNum.MatchString(line) {
				sb.WriteString(line + "\n")
			}
		}
	}
	return paths.AtomicWriteNoSync(dst, []byte(sb.String()), 0o644)
}

func gitCommit(dir string) (string, error) {
	return source.GitInherit(dir, "rev-parse", "--short", "HEAD")
}

func gitDirty(dir string) (bool, error) {
	out, err := source.GitInherit(dir, "status", "--porcelain")
	return out != "", err
}

// execBench runs `review bench --out` of this binary in dir, so one side's run is its own process.
func execBench(ctx context.Context, dir string, o ABOptions, out string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "review", "bench", "--path", o.Pkg, "--count", strconv.Itoa(o.Count),
		"--bench", o.Bench, "--out", out)
	cmd.Dir = dir
	ownGroup(cmd)
	cmd.WaitDelay = waitDelay
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("interleave: review bench in %s: %w\n%s", dir, err, errb.String())
	}
	return nil
}
