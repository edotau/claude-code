package waves

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/inspect"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
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

// VerifyTask runs custom checks and one default diff check; Go validation is never duplicated.
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
	if cmd := plans.TaskVerify(in.Task); cmd != "" && !redundantGoVerify(in.Dir, cmd, in.Files) {
		if err := plans.VerifyShape(cmd); err != nil {
			out = append(out, TaskCheck{Task: id, Cmd: cmd, Skip: err.Error()})
		} else {
			out = append(out, taskCheckRun(ctx, in, id, "verify", cmd))
		}
	}
	if len(in.Files) == 0 {
		return out
	}
	if gate := nonGoVerifyGate(in.Dir, in.Files); gate != "" {
		out = append(out, taskCheckRun(ctx, in, id, "gate", gate))
	}
	diff := DefaultVerify(in.Files)
	if in.Base != "" {
		diff += " --base " + in.Base
	}
	return append(out, taskCheckRun(ctx, in, id, "diff", diff))
}

var taskCheckRun = runCheck
var acquireVerifySlot = worktree.AcquireSlot

// DeferredChecks preserves custom requirements and non-Go toolchain checks without running a verifier at handback.
func DeferredChecks(dir string, task plans.Task, files []string) []inspect.PendingCheck {
	var checks []inspect.PendingCheck
	cmd := plans.TaskVerify(task)
	if cmd != "" && cmd != DefaultVerify(files) && cmd != "claude-code agents diff --check" && !redundantGoVerify(dir, cmd, files) {
		checks = append(checks, inspect.PendingCheck{Command: cmd})
	}
	if gate := nonGoVerifyGate(dir, files); gate != "" {
		checks = append(checks, inspect.PendingCheck{Command: gate, Generated: true})
	}
	return checks
}

func nonGoVerifyGate(dir string, files []string) string {
	if len(goVerifyPackages(files)) > 0 || paths.FileExists(filepath.Join(dir, "go.mod")) {
		return ""
	}
	dirs := slices.DeleteFunc(dirsOf(files), func(d string) bool { return !paths.FileExists(filepath.Join(dir, d)) })
	if len(dirs) == 0 {
		return ""
	}
	gate, src := worktree.ScopedVerifyGate(dir, dirs, false)
	if src == "none" {
		return ""
	}
	return gate
}

// redundantGoVerify removes covered checks and test-name filters, never alternate build modes or environments.
func redundantGoVerify(dir, cmd string, files []string) bool {
	pkgs := goVerifyPackages(files)
	steps, err := plans.VerifyArgv(cmd)
	if err != nil || len(pkgs) == 0 || len(steps) == 0 {
		return false
	}
	return !slices.ContainsFunc(steps, func(step plans.VerifyStep) bool { return !goStepCovered(dir, step, pkgs) })
}

func goStepCovered(dir string, step plans.VerifyStep, pkgs []string) bool {
	a := step.Argv
	if len(a) > 0 && a[0] == "rtk" {
		a = a[1:]
	}
	if len(step.Env) > 0 || len(a) < 3 || a[0] != "go" || !slices.Contains([]string{"build", "vet", "test"}, a[1]) {
		return false
	}
	args, ok := redundantGoArgs(a[1], a[2:])
	return ok && !slices.ContainsFunc(args, func(p string) bool {
		return (a[1] != "build" || p != "./...") && !goVerifyCovered(dir, p, pkgs)
	})
}

func redundantGoArgs(verb string, args []string) ([]string, bool) {
	var pkgs []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			pkgs = append(pkgs, args[i])
			continue
		}
		used, ok := redundantTestFlag(verb, args[i:])
		if !ok {
			return nil, false
		}
		i += used - 1
	}
	return pkgs, len(pkgs) > 0
}

func redundantTestFlag(verb string, args []string) (int, bool) {
	if verb != "test" {
		return 0, false
	}
	name, value, inline := strings.Cut(args[0], "=")
	if name == "-v" || name == "-json" {
		return 1, !inline || value == "true" || value == "false"
	}
	if name != "-run" && name != "-skip" {
		return 0, false
	}
	used := 1
	if !inline {
		if len(args) < 2 {
			return 0, false
		}
		used, value = 2, args[1]
	}
	_, err := regexp.Compile(value)
	return used, err == nil
}

func goVerifyCovered(dir, pattern string, pkgs []string) bool {
	if pattern != "." && !strings.HasPrefix(pattern, "./") {
		return false
	}
	if slices.Contains(pkgs, "...") && pattern == "./..." {
		return true
	}
	if !strings.HasSuffix(pattern, "/...") {
		return slices.Contains(pkgs, filepath.ToSlash(filepath.Clean(pattern)))
	}
	prefix := filepath.Clean(strings.TrimSuffix(pattern, "/..."))
	if dir == "" || prefix == ".." || strings.HasPrefix(prefix, ".."+string(filepath.Separator)) {
		return false
	}
	return recursiveGoCovered(dir, prefix, pkgs)
}

func recursiveGoCovered(dir, prefix string, pkgs []string) bool {
	found, covered := false, true
	root := filepath.Join(dir, prefix)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return verifyWalkDir(p, root, d.Name())
		}
		if strings.HasSuffix(p, ".go") {
			rel, err := filepath.Rel(dir, filepath.Dir(p))
			if err != nil {
				return err
			}
			found = true
			covered = covered && (slices.Contains(pkgs, "...") || slices.Contains(pkgs, filepath.ToSlash(rel)))
		}
		return nil
	})
	return err == nil && found && covered
}

func verifyWalkDir(p, root, name string) error {
	if p == root {
		return nil
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
		return filepath.SkipDir
	}
	if paths.FileExists(filepath.Join(p, "go.mod")) {
		return filepath.SkipDir
	}
	return nil
}

func goVerifyPackages(files []string) []string {
	var pkgs []string
	for _, f := range files {
		if f == "go.mod" || f == "go.sum" {
			return []string{"..."}
		}
		if strings.HasSuffix(f, ".go") {
			pkgs = append(pkgs, goVerifyDir(f))
		}
	}
	slices.Sort(pkgs)
	return slices.Compact(pkgs)
}

func goVerifyDir(file string) string {
	file = filepath.ToSlash(file)
	if strings.HasPrefix(file, "testdata/") {
		return "."
	}
	if owner, _, ok := strings.Cut(file, "/testdata/"); ok {
		return owner
	}
	return filepath.ToSlash(filepath.Dir(file))
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
	files, err := commitVerifyFiles(ctx, top, sha)
	if err != nil {
		return nil, err
	}
	if out, ok := reuseCommitStamp(ctx, top, sha, tasks, files); ok {
		return out, nil
	}
	root := filepath.Join(paths.StateDir(), "verify")
	pruneTrees(ctx, top, root, worktree.SlotRoot())
	own := sha[:min(12, len(sha))] + "-" + strconv.Itoa(os.Getpid())
	slot, err := acquireVerifySlot(ctx, top, sha, own)
	if err != nil {
		return nil, err
	}
	defer slot.Release()
	out := map[string][]TaskCheck{}
	for _, t := range tasks {
		out[cmp.Or(t.ID, t.Title)] = VerifyTask(ctx, TaskVerifyInput{Dir: slot.Dir, Base: sha + "^", Task: t, Files: files,
			OutDir: filepath.Join(root, "out-"+own)})
	}
	return out, nil
}

func commitVerifyFiles(ctx context.Context, top, sha string) ([]string, error) {
	raw, err := proc.RunGit(ctx, top, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "--root", "-m", "--first-parent", sha)
	return slices.DeleteFunc(strings.Split(raw, "\x00"), func(f string) bool { return f == "" }), err
}

func reuseCommitStamp(ctx context.Context, top, sha string, tasks []plans.Task, files []string) (map[string][]TaskCheck, bool) {
	pkgs := goVerifyPackages(files)
	if len(pkgs) == 0 || len(tasks) == 0 || !commitStampEligible(tasks, files) {
		return nil, false
	}
	key, err := inspect.ComputeCommitDiffStampKey(ctx, top, sha, pkgs, files)
	if err != nil {
		return nil, false
	}
	meta, fresh := inspect.HasFreshStamp(key, inspect.StampTTL)
	if !fresh || meta.Gates != inspect.DefaultStampGates {
		return nil, false
	}
	out := map[string][]TaskCheck{}
	for _, t := range tasks {
		id := cmp.Or(t.ID, t.Title)
		out[id] = []TaskCheck{{Task: id, Cmd: "stamp-reused"}}
	}
	return out, true
}

func commitStampEligible(tasks []plans.Task, files []string) bool {
	for _, t := range tasks {
		if cmd := plans.TaskVerify(t); cmd != "" && !redundantGoVerify("", cmd, files) {
			return false
		}
	}
	return !slices.ContainsFunc(files, func(f string) bool {
		return !strings.HasSuffix(f, ".go") && f != "go.mod" && f != "go.sum"
	})
}

// pruneTrees removes in-repo trees, trees whose verifier pid is gone, and outputs past their keep.
func pruneTrees(ctx context.Context, top, root, treeRoot string) {
	// in-repo trees and slots are from before snapshots moved out of the module; go build and gopls walk into them
	for _, pattern := range []string{"slot-*-[0-9]", "tree-*"} {
		old, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, t := range old {
			worktree.RemoveTree(ctx, top, t)
		}
	}
	trees, _ := filepath.Glob(filepath.Join(treeRoot, "tree-*"))
	for _, t := range trees {
		pid, err := strconv.Atoi(t[strings.LastIndexByte(t, '-')+1:])
		if _, serr := proc.StartTime(pid); err != nil || errors.Is(serr, proc.ErrNoProcess) {
			worktree.RemoveIdleTree(ctx, t) // a dead owner's orphaned run may still be in it
		}
	}
	plans.PruneVerify(root, "out-*")
}
