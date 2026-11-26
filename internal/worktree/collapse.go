package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrCollapseStopped marks a collapse that refused up front or dropped a lane; the collector keeps its last good head.
var ErrCollapseStopped = errors.New("collapse stopped")

// CollapseRequest names the collector lane and the finished lanes to cherry-pick into it, in order.
type CollapseRequest struct {
	Collector string
	Lanes     []string
	DryRun    bool
	Keep      bool // leave picked lanes in place: the land drainer removes them itself once the batch has landed
	Out       io.Writer
}

type collapseLane struct{ name, dir, branch string }

// Collapse picks each lane into the collector, building and vetting per lane; a lane that fails is dropped, not fatal.
func Collapse(ctx context.Context, repo *Repo, req CollapseRequest) error {
	lanes, err := collapseLanes(ctx, repo, req)
	if err != nil {
		return err
	}
	dir, err := collapseCollector(repo, req)
	if err != nil {
		return err
	}
	var picked, dropped []string
	pick := func() (err error) {
		picked, dropped, err = collapsePicks(ctx, repo, dir, lanes, req)
		return err
	}
	if req.DryRun {
		err = pick()
	} else {
		// the lease keeps a land, resolve or clean of the collector out while picks and rollbacks run
		err = withWorktreeLease(repo, req.Collector, func() error {
			if err := collapseReady(ctx, dir); err != nil {
				return fmt.Errorf("%w: collector %s: %v", ErrCollapseStopped, req.Collector, err)
			}
			return pick()
		})
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(req.Out, "collector %s: %d lanes picked (%s), %d dropped; next: claude-code worktree land --verify %s %s\n",
		req.Collector, len(picked), strings.Join(picked, " "), len(dropped), "'go build ./... && go vet ./... && go test ./...'", req.Collector)
	if len(dropped) > 0 {
		return fmt.Errorf("%w: dropped %d of %d lanes: %s", ErrCollapseStopped, len(dropped), len(lanes), strings.Join(dropped, "; "))
	}
	return nil
}

// collapsePicks picks (or previews) each lane in order, dropping one whose pick fails; a failed rollback stops all.
func collapsePicks(ctx context.Context, repo *Repo, dir string, lanes []collapseLane, req CollapseRequest) (picked, dropped []string, err error) {
	for _, l := range lanes {
		before := pickPoint{head: headOf(l.dir)}
		base, _ := gitCmd(ctx, "-C", dir, "merge-base", "HEAD", l.branch).Output() // collector before the pick
		before.base = strings.TrimSpace(string(base))
		shas, files, err := collapsePlan(ctx, dir, l.branch)
		if err == nil && !req.DryRun {
			err = collapsePick(ctx, dir, shas, files)
		}
		if errors.Is(err, errCollapseRestore) {
			return picked, dropped, fmt.Errorf("%w at lane %s: %v", ErrCollapseStopped, l.name, err)
		}
		if err != nil {
			dropped = append(dropped, fmt.Sprintf("%s (%v)", l.name, err))
			fmt.Fprintf(req.Out, "dropped %s: %v\n", l.name, err)
			continue
		}
		picked = append(picked, l.name)
		if req.DryRun {
			fmt.Fprintf(req.Out, "would pick %s: %d commits, %d files\n  files: %s\nwould remove %s\n", l.name, len(shas), len(files), strings.Join(files, " "), l.name)
			continue
		}
		afterCollapsePick(l)
		before.n, before.m = len(shas), laneCommits(ctx, dir, before)
		count := fmt.Sprintf("%d commits", before.n)
		if before.m > before.n {
			count = fmt.Sprintf("%d of %d commits", before.n, before.m)
		}
		outcome := "kept: its batch has not landed yet"
		if !req.Keep {
			outcome = collapseRemove(ctx, repo, dir, l, before)
		}
		fmt.Fprintf(req.Out, "collapsed %s (%s); %s\n", l.name, count, outcome)
	}
	return picked, dropped, nil
}

// pickPoint: lane head as picked, collector merge-base before it, commits picked (n), all past the base (m).
type pickPoint struct {
	head, base string
	n, m       int
}

// notConverged is why the collector does not hold all of the lane: differing content, or commits not picked this run.
func notConverged(ctx context.Context, dir string, p pickPoint) string {
	switch path := contentDiffers(ctx, dir, p); {
	case path != "":
		return "content differs from the collector (" + path + ")"
	case p.m < 0:
		return "the lane's commits could not be counted"
	case p.m != p.n:
		return fmt.Sprintf("%d commits were already in the collector or are merges", p.m-p.n)
	}
	return ""
}

// laneCommits counts the lane's commits past the base, merges included.
func laneCommits(ctx context.Context, dir string, p pickPoint) int {
	out, err := gitCmd(ctx, "-C", dir, "rev-list", "--count", p.base+".."+p.head).Output()
	m, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || convErr != nil {
		return -1
	}
	return m
}

// contentDiffers returns the first path the lane changed whose tree entry in the collector's HEAD is not the lane's.
func contentDiffers(ctx context.Context, dir string, p pickPoint) string {
	if p.base == "" {
		return "(no merge base)"
	}
	changed, err := gitNames(ctx, dir, "diff", "--no-renames", "--name-only", "-z", p.base, p.head)
	if err != nil {
		return "(diff unreadable)"
	}
	for _, path := range changed {
		inCol, err1 := treeEntry(ctx, dir, "HEAD", path)
		inLane, err2 := treeEntry(ctx, dir, p.head, path)
		if err1 != nil || err2 != nil || inCol != inLane {
			return path
		}
	}
	return ""
}

// treeEntry is path's "mode type id" at rev, "" when absent; a git failure is an error, never "absent".
func treeEntry(ctx context.Context, dir, rev, path string) (string, error) {
	out, err := gitCmd(ctx, "-C", dir, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", rev, "--", path).Output()
	if err != nil {
		return "", err
	}
	entry, _, _ := strings.Cut(string(out), "\t")
	return entry, nil
}

// afterCollapsePick is a test seam between a lane's pick and its removal.
var afterCollapsePick = func(collapseLane) {}

// collapseRemove removes a converged lane and returns "removed", or "kept: <why>" for what it left in place.
func collapseRemove(ctx context.Context, repo *Repo, dir string, l collapseLane, p pickPoint) string {
	picked := p.head
	if headOf(l.dir) != picked {
		return "kept: head moved after the pick"
	}
	if why := notConverged(ctx, dir, p); why != "" {
		return "kept: " + why
	}
	out, err := gitCmd(ctx, "-C", l.dir, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil || len(strings.TrimSpace(string(out))) > 0 {
		return "kept: uncommitted or untracked files"
	}
	if _, reason := cleanBusyReason(l.dir, "", nil); reason != "" {
		return "kept: " + reason
	}
	if out, err := gitCmd(ctx, "-C", repo.Main, "worktree", "remove", l.dir).CombinedOutput(); err != nil {
		return "kept: git worktree remove: " + strings.TrimSpace(string(out))
	}
	if out, err := gitCmd(ctx, "-C", repo.Main, "update-ref", "-d", "refs/heads/"+l.branch, picked).CombinedOutput(); err != nil {
		return "worktree removed; kept: branch " + l.branch + ": " + strings.TrimSpace(string(out))
	}
	if err := CloseLedger(repo.Main, l.name, LedgerClose{Via: "collapse", Head: picked, BranchDeleted: true}); err != nil {
		fmt.Fprintf(os.Stderr, "worktree collapse: warning: ledger: %v\n", err)
	}
	return "removed"
}

// collapseReady refuses a collector with tracked edits or a cherry-pick, merge, revert or rebase in progress.
func collapseReady(ctx context.Context, dir string) error {
	out, err := gitCmd(ctx, "-C", dir, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if len(out) > 0 {
		return fmt.Errorf("has uncommitted changes: %s", strings.Join(strings.Fields(string(out)), " "))
	}
	for _, p := range []string{"CHERRY_PICK_HEAD", "MERGE_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		gp, err := gitCmd(ctx, "-C", dir, "rev-parse", "--path-format=absolute", "--git-path", p).Output()
		if err != nil {
			return fmt.Errorf("git rev-parse --git-path %s: %w", p, err)
		}
		if _, err := os.Stat(strings.TrimSpace(string(gp))); err == nil {
			return fmt.Errorf("has an operation in progress (%s)", p)
		}
	}
	return nil
}

// collapseLanes resolves every lane and refuses, by name and before any pick, one that is missing or has no branch.
func collapseLanes(ctx context.Context, repo *Repo, req CollapseRequest) ([]collapseLane, error) {
	var lanes []collapseLane
	var bad []string
	for _, name := range req.Lanes {
		dest, err := resolveWorktree(ctx, repo, name)
		if err != nil || name == req.Collector {
			bad = append(bad, name+" (not a usable source lane)")
			continue
		}
		branch, err := currentWorktreeBranch(dest)
		if err != nil || branch == "" {
			bad = append(bad, name+" (no branch)")
			continue
		}
		lanes = append(lanes, collapseLane{name, dest, branch})
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%w: refused before picking anything: %s", ErrCollapseStopped, strings.Join(bad, ", "))
	}
	return lanes, nil
}

// collapseCollector returns the collector's directory, creating the lane from main unless this is a dry run.
func collapseCollector(repo *Repo, req CollapseRequest) (string, error) {
	dest, err := resolveWorktreeBounded(repo, req.Collector)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if req.DryRun {
		return repo.Main, nil
	}
	return Create(repo, req.Collector, CreateOptions{Session: "cli", Log: io.Discard})
}

// collapsePlan lists the lane's commits not yet in the collector (oldest first) and the files they touch.
func collapsePlan(ctx context.Context, dir, branch string) (shas, files []string, err error) {
	out, err := gitCmd(ctx, "-C", dir, "cherry", "HEAD", branch).Output()
	if err != nil {
		return nil, nil, fmt.Errorf("git cherry %s: %w", branch, err)
	}
	for row := range strings.SplitSeq(string(out), "\n") {
		if sha, ok := strings.CutPrefix(row, "+ "); ok {
			shas = append(shas, sha)
		}
	}
	for _, sha := range shas {
		names, err := gitNames(ctx, dir, "diff-tree", "-z", "--no-commit-id", "--name-only", "-r", "--root", sha)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range names {
			if !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	return shas, files, nil
}

// collapsePick cherry-picks shas into dir, then builds and vets; any failure leaves dir at its previous HEAD.
func collapsePick(ctx context.Context, dir string, shas, files []string) error {
	if len(shas) == 0 {
		return nil
	}
	before := headOf(dir)
	if out, err := gitCmd(ctx, append([]string{"-C", dir, "cherry-pick"}, shas...)...).CombinedOutput(); err != nil {
		conflicts, _ := gitNames(ctx, dir, "diff", "-z", "--name-only", "--diff-filter=U")
		err = fmt.Errorf("cherry-pick: %s", strings.TrimSpace(string(out)))
		if len(conflicts) > 0 {
			err = fmt.Errorf("conflict in %s", strings.Join(conflicts, ", "))
		}
		return collapseRestore(dir, before, err)
	}
	if err := collapseVerify(ctx, dir, files); err != nil {
		return collapseRestore(dir, before, err)
	}
	return nil
}

// errCollapseRestore marks a dropped pick whose rollback failed: the collector is not at its last good head.
var errCollapseRestore = errors.New("collector not restored")

// collapseRestore ends any pick in progress and resets dir to before (even on a canceled ctx), then returns cause.
func collapseRestore(dir, before string, cause error) error {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	_ = gitCmd(ctx, "-C", dir, "cherry-pick", "--abort").Run() // fails when no pick is in progress
	out, err := gitCmd(ctx, "-C", dir, "reset", "-q", "--hard", before).CombinedOutput()
	if err == nil {
		out, err = gitCmd(ctx, "-C", dir, "status", "--porcelain", "--untracked-files=no").Output()
	}
	if err != nil || len(out) > 0 || headOf(dir) != before {
		return fmt.Errorf("%w to %s after %v: %v %s", errCollapseRestore, ShortOID(before), cause, err, strings.TrimSpace(string(out)))
	}
	return cause
}

// collapseVerify runs go build on the module and go vet on the packages files touch; no go.mod skips both.
func collapseVerify(ctx context.Context, dir string, files []string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil
	}
	var pkgs []string
	for _, f := range files {
		if p := goPkgOf(dir, filepath.Dir(f)); p != "" && !slices.Contains(pkgs, "./"+p) {
			pkgs = append(pkgs, "./"+p)
		}
	}
	steps := [][]string{{"build", "./..."}}
	if len(pkgs) > 0 {
		steps = append(steps, append([]string{"vet"}, pkgs...))
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
	}
	return nil
}
