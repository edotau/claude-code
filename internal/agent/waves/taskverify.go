package waves

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

const shapeSkip = "verify not run: shape"

// TaskCheck is one verify step's outcome for a plan task.
type TaskCheck struct {
	Task, Cmd, Skip string
	Exit            int
	Took            time.Duration
	Tail            []string
	Saved           string
}

// Line is "verify Phase 2.1: <cmd> ok (0.4 s)"; a failure adds its last 10 output lines below.
func (c TaskCheck) Line() string {
	switch {
	case c.Skip != "":
		return fmt.Sprintf("verify %s: %s (%s)", c.Task, c.Skip, c.Cmd)
	case c.Exit == 0:
		return fmt.Sprintf("verify %s: %s ok (%.1f s)", c.Task, c.Cmd, c.Took.Seconds())
	}
	tail := c.Tail[max(0, len(c.Tail)-10):]
	return fmt.Sprintf("verify %s: %s FAIL exit %d (%.1f s); full output %s\n    %s",
		c.Task, c.Cmd, c.Exit, c.Took.Seconds(), c.Saved, strings.Join(tail, "\n    "))
}

// AllOK is every check green; a shape-refused verify counts as no verify (D12), a timeout as not green.
func AllOK(cs []TaskCheck) bool {
	return !slices.ContainsFunc(cs, func(c TaskCheck) bool {
		return c.Exit != 0 || (c.Skip != "" && c.Skip != shapeSkip)
	})
}

// TaskVerifyInput is one task's check: Dir the tree, Base the diff check's base ("" = HEAD), Files its scope.
type TaskVerifyInput struct {
	Dir, Base, OutDir string
	Task              plans.Task
	Files             []string
}

// VerifyTask runs the task's allowlisted verify:, then the scoped gate and diff check on Files; ctx bounds all.
func VerifyTask(ctx context.Context, in TaskVerifyInput) []TaskCheck {
	out := verifySteps(ctx, in)
	if slices.ContainsFunc(out, func(c TaskCheck) bool { return c.Skip == "" }) {
		return out
	}
	return append(out, TaskCheck{Task: cmp.Or(in.Task.ID, in.Task.Title), Cmd: "-", Skip: "no checks ran"})
}

func verifySteps(ctx context.Context, in TaskVerifyInput) []TaskCheck {
	id := cmp.Or(in.Task.ID, in.Task.Title)
	var out []TaskCheck
	if cmd := plans.TaskVerify(in.Task); cmd != "" {
		if err := plans.VerifyShape(cmd); err != nil {
			out = append(out, TaskCheck{Task: id, Cmd: cmd, Skip: err.Error()})
		} else {
			out = append(out, runCheck(ctx, in, id, "verify", cmd))
		}
	}
	if len(in.Files) == 0 {
		return out
	}
	if gate, src := worktree.ScopedVerifyGate(in.Dir, dirsOf(in.Files), false); src != "none" {
		out = append(out, runCheck(ctx, in, id, "gate", gate))
	}
	diff := DefaultVerify(in.Files)
	if in.Base != "" {
		diff += " --base " + in.Base
	}
	return append(out, runCheck(ctx, in, id, "diff", diff))
}

func runCheck(ctx context.Context, in TaskVerifyInput, id, step, cmd string) TaskCheck {
	start := time.Now()
	env := CheckEnv{}
	if step == "verify" {
		env.Exec = verifyArgv // plan-written, untrusted: argv only, never bash
	}
	r, err := GateRun(ctx, env, GateInput{Dir: in.Dir, Cmd: cmd,
		OutPath: filepath.Join(in.OutDir, paths.Slug(id, "", 40, "task")+"-"+step+".txt")})
	c := TaskCheck{Task: id, Cmd: cmd, Exit: r.Exit, Took: time.Since(start), Tail: r.Tail, Saved: r.Saved}
	switch {
	case ctx.Err() != nil:
		c.Skip = fmt.Sprintf("not finished in %.0f s; the commit-time verify still runs", c.Took.Seconds())
	case err != nil:
		c.Exit, c.Tail = -1, append(c.Tail, err.Error())
	}
	return c
}

// dirsOf is the sorted distinct directories of files.
func dirsOf(files []string) []string {
	var dirs []string
	for _, f := range files {
		dirs = append(dirs, filepath.ToSlash(filepath.Dir(f)))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// VerifyCommit checks each task on a locked scratch checkout of sha (diff check vs sha^), private to this process.
func VerifyCommit(ctx context.Context, top, sha string, tasks []plans.Task) (map[string][]TaskCheck, error) {
	if !plans.ValidSHA(sha) {
		return nil, fmt.Errorf("not a commit sha: %q", sha)
	}
	root := filepath.Join(paths.StateDir(), "verify")
	pruneTrees(ctx, top, root)
	own := sha[:min(12, len(sha))] + "-" + strconv.Itoa(os.Getpid())
	dir, release, err := acquireTree(ctx, top, root, sha, own)
	if err != nil {
		return nil, err
	}
	defer release()
	// diff against the first parent so a merge lists its files; -z keeps spaces and quotes intact
	raw, err := proc.RunGit(ctx, top, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "--root", "-m", "--first-parent", sha)
	if err != nil {
		return nil, err
	}
	files := slices.DeleteFunc(strings.Split(raw, "\x00"), func(f string) bool { return strings.TrimSpace(f) == "" })
	out := map[string][]TaskCheck{}
	for _, t := range tasks {
		out[cmp.Or(t.ID, t.Title)] = VerifyTask(ctx, TaskVerifyInput{Dir: dir, Base: sha + "^", Task: t, Files: files,
			OutDir: filepath.Join(root, "out-"+own)})
	}
	return out, nil
}

// pruneTrees removes scratch trees whose verifier pid is gone (killed or crashed) and outputs past their keep.
func pruneTrees(ctx context.Context, top, root string) {
	trees, _ := filepath.Glob(filepath.Join(root, "tree-*"))
	for _, t := range trees {
		pid, err := strconv.Atoi(t[strings.LastIndexByte(t, '-')+1:])
		if _, serr := proc.StartTime(pid); err != nil || errors.Is(serr, proc.ErrNoProcess) {
			removeTree(ctx, top, t)
		}
	}
	plans.PruneVerify(root, "out-*")
}

const verifySlots = 4

// acquireTree checks sha out in a per-repo slot held by a lock file, so the build cache sees one stable path.
// A dead holder's flock is released by the OS; with every slot busy it falls back to a private tree.
func acquireTree(ctx context.Context, top, root, sha, own string) (string, func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(top)))[:8]
	for i := range verifySlots {
		base := filepath.Join(root, fmt.Sprintf("slot-%s-%d", key, i))
		f, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return "", nil, err
		}
		if !paths.TryLock(f) {
			_ = f.Close()
			continue
		}
		if err := resetSlot(ctx, top, base, sha); err != nil {
			_ = f.Close()
			return "", nil, err
		}
		return base, func() { _ = f.Close() }, nil
	}
	dir := filepath.Join(root, "tree-"+own)
	removeTree(ctx, top, dir) // a reused pid's leftover is ours
	if _, err := proc.RunGit(ctx, top, "worktree", "add", "--detach", dir, sha); err != nil {
		return "", nil, err
	}
	return dir, func() { removeTree(context.Background(), top, dir) }, nil
}

// resetSlot moves the slot's worktree to sha and drops untracked and ignored files, recreating it when it is missing or broken.
func resetSlot(ctx context.Context, top, dir, sha string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if _, err = proc.RunGit(ctx, dir, "checkout", "--force", "--detach", sha); err == nil {
			if _, err = proc.RunGit(ctx, dir, "clean", "-ffdxq"); err == nil {
				return nil
			}
		}
	}
	removeTree(ctx, top, dir)
	_, err := proc.RunGit(ctx, top, "worktree", "add", "--detach", dir, sha)
	return err
}

func removeTree(ctx context.Context, top, dir string) {
	_, _ = proc.RunGit(ctx, top, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_, _ = proc.RunGit(ctx, top, "worktree", "prune")
}
