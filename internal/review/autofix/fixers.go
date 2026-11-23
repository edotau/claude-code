package autofix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
)

// Result is the lane a repair left (empty when none) and the one-line notice for the owner.
type Result struct {
	Lane, Path, Notice string
	Fixed              []Class
}

var (
	run        = repairRun
	harnessBin = paths.SelfBinary
)

// Repair fixes sha's deterministic findings in lane autofix-<sha7> and commits there; it never merges (D1).
func Repair(ctx context.Context, top, sha string, findings []Finding) (Result, error) {
	if !plans.ValidSHA(sha) {
		return Result{}, fmt.Errorf("autofix: invalid SHA %q", sha)
	}
	findings = slices.DeleteFunc(slices.Clone(findings), func(f Finding) bool { return !f.Class.Deterministic() })
	if len(findings) == 0 {
		return Result{Notice: "autofix " + sha[:7] + ": no deterministic findings"}, nil
	}
	name := "autofix-" + sha[:7]
	// an existing branch means this sha was tried; a lane left open waits for its merge
	if out, err := run(ctx, top, "git", "branch", "--list", "worktree-"+name); err != nil {
		return Result{}, fmt.Errorf("check autofix branch: %w", err)
	} else if strings.TrimSpace(out) != "" {
		return Result{Notice: "autofix " + sha[:7] + ": already tried (branch worktree-" + name + ")"}, nil
	}
	bin, err := harnessBin()
	if err != nil {
		return Result{}, fmt.Errorf("locate autofix harness: %w", err)
	}
	out, err := run(ctx, top, bin, "worktree", "create", name)
	if err != nil {
		return Result{}, fmt.Errorf("create autofix lane: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	res := Result{Lane: name, Path: lines[len(lines)-1]}
	if err := checkRepairLane(ctx, top, res); err != nil {
		return res, err
	}
	res.Fixed, err = applyFixers(ctx, bin, res.Path, findings)
	if err != nil {
		return discardRepair(ctx, top, bin, res, "fixer failed: "+err.Error())
	}
	return finishRepair(ctx, top, bin, sha, res)
}

func repairRun(ctx context.Context, dir, name string, args ...string) (string, error) {
	if name == "git" {
		return proc.RunGitRaw(ctx, dir, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("run %s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

func checkRepairLane(ctx context.Context, top string, res Result) error {
	lane, err := filepath.EvalSymlinks(res.Path)
	if err != nil {
		return fmt.Errorf("resolve autofix lane: %w", err)
	}
	live, err := filepath.EvalSymlinks(top)
	if err != nil {
		return fmt.Errorf("resolve live checkout: %w", err)
	}
	if !filepath.IsAbs(lane) || lane == live {
		return fmt.Errorf("autofix lane is not separate from live checkout: %s", res.Path)
	}
	info, err := os.Lstat(filepath.Join(lane, ".git"))
	if err != nil {
		return fmt.Errorf("inspect linked autofix lane: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("autofix lane is not a linked worktree: %s", lane)
	}
	branch, err := run(ctx, lane, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("check autofix lane branch: %w", err)
	}
	if strings.TrimSpace(branch) != "worktree-"+res.Lane {
		return fmt.Errorf("unexpected autofix lane branch: %s", strings.TrimSpace(branch))
	}
	return nil
}

func repairPath(lane, file string) (string, bool) {
	file = filepath.Clean(file)
	if !filepath.IsLocal(file) || file == "." || file == "state" || strings.HasPrefix(file, "state/") {
		return "", false
	}
	real, err := filepath.EvalSymlinks(filepath.Join(lane, file))
	if err != nil || !paths.Within(real, lane) {
		return "", false
	}
	info, err := os.Stat(real)
	return file, err == nil && info.Mode().IsRegular()
}

func fixerPaths(lane string, findings []Finding, class Class) []string {
	var files []string
	for _, f := range findings {
		if f.Class != class {
			continue
		}
		file := f.Path
		if class == ClassWriterRows {
			file, _, _ = strings.Cut(f.Message, ":")
			if filepath.Base(file) != file || !strings.HasSuffix(file, ".txt") {
				continue
			}
			file = filepath.Join("internal/schemas/testdata/writers", file)
		}
		if file, ok := repairPath(lane, file); ok {
			files = append(files, file)
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}

func applyFixers(ctx context.Context, bin, lane string, findings []Finding) ([]Class, error) {
	lane, err := filepath.EvalSymlinks(lane)
	if err != nil {
		return nil, fmt.Errorf("resolve fixer directory: %w", err)
	}
	var fixed []Class
	for _, class := range []Class{ClassGofmt, ClassMarkdownLong, ClassWriterRows, ClassWorkflowBlock} {
		files := fixerPaths(lane, findings, class)
		if class == ClassWorkflowBlock {
			if !slices.ContainsFunc(findings, func(f Finding) bool { return f.Class == class }) {
				continue
			}
		} else if len(files) == 0 {
			continue
		}
		if err := applyFixer(ctx, bin, lane, class, files); err != nil {
			return fixed, fmt.Errorf("apply %s fixer: %w", class, err)
		}
		fixed = append(fixed, class)
	}
	return fixed, nil
}

func applyFixer(ctx context.Context, bin, lane string, class Class, files []string) error {
	switch class {
	case ClassGofmt:
		args := []string{"-w"}
		for _, file := range files {
			args = append(args, "./"+file)
		}
		_, err := run(ctx, lane, "gofmt", args...)
		return err
	case ClassMarkdownLong:
		for _, file := range files {
			if _, err := run(ctx, lane, bin, "agents", "fix", "--path", file); err != nil {
				return err
			}
		}
	case ClassWorkflowBlock:
		_, err := run(ctx, lane, bin, "workflow", "sync", lane)
		return err
	case ClassWriterRows:
		for _, file := range files {
			if err := sortWriterRows(filepath.Join(lane, file)); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortWriterRows(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read writer rows %s: %w", file, err)
	}
	rows := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	slices.Sort(rows)
	if err := os.WriteFile(file, []byte(strings.Join(slices.Compact(rows), "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write sorted writer rows %s: %w", file, err)
	}
	return nil
}

func laneChanges(ctx context.Context, lane string) ([]string, error) {
	status, err := run(ctx, lane, "git", "status", "--porcelain=v1", "-z")
	if err != nil {
		return nil, fmt.Errorf("inspect autofix changes: %w", err)
	}
	var files []string
	for _, e := range proc.StatusEntries(status) {
		files = append(files, e.Path)
	}
	return files, nil
}

func finishRepair(ctx context.Context, top, bin, sha string, res Result) (Result, error) {
	files, err := laneChanges(ctx, res.Path)
	if err != nil {
		return res, err
	}
	if len(files) == 0 {
		return cleanRepair(ctx, top, bin, res, "fixers changed nothing")
	}
	args := []string{"agents", "diff", "--check"}
	for _, f := range files {
		args = append(args, "--path", f)
	}
	if out, err := run(ctx, res.Path, bin, args...); err != nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return discardRepair(ctx, top, bin, res, "gate still red: "+strings.Join(lines[max(0, len(lines)-6):], "\n"))
	}
	classes := repairClasses(res.Fixed)
	msg := fmt.Sprintf("chore(autofix): repair %s found on %s", classes, sha[:7])
	args = append([]string{"worktree", "commit", "-m", msg, "--"}, files...)
	if _, err := run(ctx, res.Path, bin, args...); err != nil {
		return discardRepair(ctx, top, bin, res, "commit refused: "+err.Error())
	}
	res.Notice = fmt.Sprintf("autofix %s: fixed %s in lane %s; gate green; land it: claude-code worktree merge %s",
		sha[:7], classes, res.Lane, res.Lane)
	return res, nil
}

func repairClasses(classes []Class) string {
	names := make([]string, len(classes))
	for i, class := range classes {
		names[i] = string(class)
	}
	return strings.Join(names, ", ")
}

func cleanRepair(ctx context.Context, top, bin string, res Result, why string) (Result, error) {
	if _, err := run(ctx, top, bin, "worktree", "clean", res.Lane); err != nil {
		res.Notice = res.Lane + ": " + why
		return res, fmt.Errorf("clean autofix lane: %w", err)
	}
	return Result{Notice: res.Lane + ": " + why + "; lane removed"}, nil
}

// discardRepair reverses the lane's uncommitted fixes so a plain, unforced clean can remove it.
func discardRepair(ctx context.Context, top, bin string, res Result, why string) (Result, error) {
	res.Notice = res.Lane + ": " + why
	files, err := laneChanges(ctx, res.Path)
	if err != nil {
		return res, err
	}
	if len(files) > 0 {
		patch, err := run(ctx, res.Path, "git", "diff", "--binary")
		if err != nil {
			return res, fmt.Errorf("capture autofix patch: %w", err)
		}
		if err := reverseRepair(ctx, res.Path, patch); err != nil {
			return res, err
		}
	}
	return cleanRepair(ctx, top, bin, res, why)
}

func reverseRepair(ctx context.Context, lane, patch string) (err error) {
	f, err := os.CreateTemp("", "autofix-*.patch")
	if err != nil {
		return fmt.Errorf("create autofix rollback patch: %w", err)
	}
	defer func() { err = errors.Join(err, os.Remove(f.Name())) }()
	_, writeErr := f.WriteString(patch)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("write autofix rollback patch: %w", err)
	}
	if _, err := run(ctx, lane, "git", "apply", "-R", f.Name()); err != nil {
		return fmt.Errorf("reverse autofix patch: %w", err)
	}
	return nil
}
