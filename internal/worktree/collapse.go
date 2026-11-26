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
	"strings"
)

// ErrCollapseStopped marks a collapse that stopped on a refusal, conflict or failed build/vet; the collector is clean.
var ErrCollapseStopped = errors.New("collapse stopped")

// CollapseRequest names the collector lane and the finished lanes to cherry-pick into it, in order.
type CollapseRequest struct {
	Collector string
	Lanes     []string
	DryRun    bool
	Out       io.Writer
}

type collapseLane struct{ name, dir, branch string }

// Collapse picks each lane's own commits into the collector, building and vetting after every lane; it never tests.
func Collapse(ctx context.Context, repo *Repo, req CollapseRequest) error {
	lanes, err := collapseLanes(ctx, repo, req)
	if err != nil {
		return err
	}
	dir, err := collapseCollector(repo, req)
	if err != nil {
		return err
	}
	for i, l := range lanes {
		shas, files, err := collapsePlan(ctx, dir, l.branch)
		if err != nil {
			return err
		}
		if !req.DryRun {
			if err := collapsePick(ctx, dir, shas, files); err != nil {
				return collapseStop(req, l.name, lanes[i:], err)
			}
		}
		verb := "picked"
		if req.DryRun {
			verb = "would pick"
		}
		fmt.Fprintf(req.Out, "%s %s: %d commits, %d files\n", verb, l.name, len(shas), len(files))
		if req.DryRun {
			fmt.Fprintf(req.Out, "  files: %s\n", strings.Join(files, " "))
		}
	}
	fmt.Fprintf(req.Out, "collector %s: %d lanes; next: claude-code worktree land --verify %s %s\n", req.Collector,
		len(lanes), "'go build ./... && go vet ./... && go test ./...'", req.Collector)
	return nil
}

func collapseStop(req CollapseRequest, failed string, rest []collapseLane, cause error) error {
	names := make([]string, len(rest))
	for i, l := range rest {
		names[i] = l.name
	}
	fmt.Fprintf(req.Out, "resume: claude-code worktree collapse %s %s\n", req.Collector, strings.Join(names, " "))
	return fmt.Errorf("%w at lane %s: %v", ErrCollapseStopped, failed, cause)
}

// collapseLanes resolves every lane and refuses, by name and before any pick, one that is missing or has changes.
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
		out, err := gitCmd(ctx, "-C", dest, "status", "--porcelain").Output()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			bad = append(bad, name+" (uncommitted changes)")
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
		_ = gitCmd(ctx, "-C", dir, "cherry-pick", "--abort").Run()
		if len(conflicts) == 0 {
			return fmt.Errorf("cherry-pick: %s", strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("conflict in %s", strings.Join(conflicts, ", "))
	}
	if err := collapseVerify(ctx, dir, files); err != nil {
		_ = gitCmd(ctx, "-C", dir, "reset", "--hard", before).Run()
		return err
	}
	return nil
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
