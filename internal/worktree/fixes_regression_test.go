package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResumeRejectsCommittedOutOfScopeEdits(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (resolveAgentResult, error) {
		regressionWriteFile(t, filepath.Join(wt, "f.txt"), "resolved\n")
		regressionWriteFile(t, filepath.Join(wt, "outside.txt"), "unauthorized\n")
		mergeFixtureRun(t, wt, "add", "f.txt", "outside.txt")
		mergeFixtureRun(t, wt, "commit", "--no-edit")
		return resolveAgentResult{}, nil
	})
	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("fresh resolution must reject committed scope violation: result=%+v err=%v", result, err)
	}
	if !op.AgentExitOK {
		t.Fatal("fixture must preserve the successful agent exit for resume")
	}
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (resolveAgentResult, error) {
		t.Fatal("resume must not invoke the agent")
		return resolveAgentResult{}, nil
	})
	_, result, err = ResolveWithAgent(context.Background(), repo, plan, "claude", op)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("resume accepted the same committed scope violation: result=%+v err=%v", result, err)
	}
}

func TestCleanAllDoesNotAliasNestedWorktree(t *testing.T) {
	repo := testRepo(t)
	sibling, err := Create(repo, "feature", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo.Root, "group", "feature")
	commitIn(t, repo.Main, "worktree", "add", "-b", "nested-feature", nested)
	results, err := CleanAll(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, result := range results {
		if result.Path == nested {
			found = true
			if result.Removed || !strings.Contains(result.Reason, "nested below") {
				t.Fatalf("nested worktree must be refused under its own path: %+v", result)
			}
		}
	}
	if !found {
		t.Fatalf("nested worktree was aliased to sibling %s: %+v", sibling, results)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatal(err)
	}
}

func TestListKeepsDotDotPrefixedChildRegistered(t *testing.T) {
	repo := testRepo(t)
	wt, err := Create(repo, "..scratch", CreateOptions{Branch: "scratch"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != wt || !entries[0].Registered || entries[0].Orphaned {
		t.Fatalf("safe dot-dot-prefixed child lost its registration: %+v", entries)
	}
	results, err := CleanAll(repo, false)
	if err != nil || len(results) != 1 || results[0].Path != wt {
		t.Fatalf("CleanAll skipped registered child: results=%+v err=%v", results, err)
	}
}

func TestRequireBranchPreservesGitFailures(t *testing.T) {
	repo := testRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", ctx, context.Canceled},
		{"git-missing", context.Background(), exec.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "git-missing" {
				t.Setenv("PATH", t.TempDir())
			}
			_, err := requireBranch(tc.ctx, repo.Main)
			if !errors.Is(err, tc.want) || errors.Is(err, errDetachedHead) {
				t.Fatalf("git infrastructure failure misreported as detached HEAD: %v; want %v", err, tc.want)
			}
			plan := &MergePlan{}
			stop, err := screenTargetBranch(tc.ctx, repo, plan, "main")
			if !errors.Is(err, tc.want) || stop || len(plan.Issues) != 0 {
				t.Fatalf("target screening hid infrastructure failure: stop=%v issues=%v err=%v", stop, plan.Issues, err)
			}
		})
	}
}

func TestCheckResolutionRejectsCommittedConflictMarkers(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	op := crashedOperation(t, repo, wt, plan)
	mergeFixtureRun(t, wt, "add", "f.txt")
	mergeFixtureRun(t, wt, "commit", "--no-edit")
	state, reason, err := CheckResolution(context.Background(), op, wt)
	if err != nil || state != resolutionInvalid || !strings.Contains(reason, "conflict markers") {
		t.Fatalf("committed markers accepted: state=%v reason=%q err=%v", state, reason, err)
	}
}

func TestResolutionAllowsTrailingWhitespace(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (resolveAgentResult, error) {
		regressionWriteFile(t, filepath.Join(wt, "f.txt"), "resolved with trailing space \n")
		if out, err := runGit(context.Background(), wt, nil, "diff", "--check"); err == nil {
			t.Fatalf("fixture must trigger git's whitespace warning: %q", out)
		}
		return resolveAgentResult{}, nil
	})
	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("whitespace without conflict markers must resolve: %v", err)
	}
	requirePhase(t, "operation", op.Phase, "resolved")
	requireIntegrate(t, result, headOf(wt))
	requireFileText(t, filepath.Join(wt, "f.txt"), "resolved with trailing space \n")
}

func TestUnionFilesystemPropagatesRegisteredStatError(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "loop")
	if err := os.Symlink("loop", wt); err != nil {
		t.Fatal(err)
	}
	entries, err := unionFilesystem(&Repo{Root: root}, []Entry{{Path: wt, Registered: true}})
	if !errors.Is(err, syscall.ELOOP) || !strings.Contains(err.Error(), "stat registered worktree") {
		t.Fatalf("registered stat error became a missing entry: entries=%+v err=%v", entries, err)
	}
}

func TestRunGitCancellationBoundsInheritedPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	regressionFakeGit(t, fmt.Sprintf("/bin/sleep 10 &\necho $! > %q\nwait\n", pidFile))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := runGit(ctx, t.TempDir(), nil, "status")
	elapsed := time.Since(start)
	regressionKillChild(t, pidFile)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("git call must fail after cancellation: err=%v ctx=%v", err, ctx.Err())
	}
	if elapsed >= 8*time.Second {
		t.Fatalf("cancelled git waited for inherited output pipes: %s", elapsed)
	}
}

func TestResolveRepoBoundsDiscoverySubprocess(t *testing.T) {
	regressionFakeGit(t, "exec /bin/sleep 34\n")
	start := time.Now()
	_, err := ResolveRepo(t.TempDir())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung discovery must fail")
	}
	if elapsed >= 33*time.Second {
		t.Fatalf("git discovery ignored its deadline: %s", elapsed)
	}
}

func TestResolveWorktreePropagatesLstatError(t *testing.T) {
	repo := testRepo(t)
	repo.Root = filepath.Join(t.TempDir(), "not-a-directory")
	regressionWriteFile(t, repo.Root, "file\n")
	dest, err := ResolveWorktree(repo, "feature")
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "inspect") || dest != "" {
		t.Fatalf("Lstat failure bypassed validation: dest=%q err=%v", dest, err)
	}
}

func TestActivityChoosesNewerIndex(t *testing.T) {
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * StaleAfter).Truncate(time.Second)
	recent := old.Add(StaleAfter)
	for file, stamp := range map[string]time.Time{"logs/HEAD": old, "index": recent} {
		path := filepath.Join(gitDir, filepath.FromSlash(file))
		regressionWriteFile(t, path, "activity\n")
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	got, source := activity(gitDir, t.TempDir())
	if !got.Equal(recent) || source != "index" {
		t.Fatalf("newer index ignored: time=%v source=%q; want %v, index", got, source, recent)
	}
}

func TestListCleanEligibilityReflectsLiveRefusals(t *testing.T) {
	t.Run("in-use", func(t *testing.T) {
		stubProcesses(t, func(string) ([]Proc, error) {
			return []Proc{{PID: 999999, Comm: "busy-agent"}}, nil
		})
		entry := regressionListEntry(t, func(string) {})
		if entry.Busy == "" || entry.EligibleForClean() {
			t.Fatalf("in-use worktree advertised for clean: %+v", entry)
		}
	})
	t.Run("operation", func(t *testing.T) {
		entry := regressionListEntry(t, func(wt string) {
			gitDir, err := gitDirFromPath(wt)
			if err != nil {
				t.Fatal(err)
			}
			regressionWriteFile(t, filepath.Join(gitDir, "MERGE_HEAD"), headOf(wt)+"\n")
		})
		if !strings.Contains(entry.Busy, "in progress") || entry.EligibleForClean() {
			t.Fatalf("mid-merge worktree advertised for clean: %+v", entry)
		}
	})
	t.Run("inspection", func(t *testing.T) {
		entry := Entry{Exists: true, Merged: true, Problems: []string{"status inspection failed"}}
		if entry.EligibleForClean() {
			t.Fatal("inspection failures must block eligibility even when other clean flags pass")
		}
		entry.Problems = nil
		if !entry.EligibleForClean() {
			t.Fatal("clean, unoccupied worktree must remain eligible")
		}
	})
}

func regressionListEntry(t *testing.T, prepare func(string)) Entry {
	t.Helper()
	repo := testRepo(t)
	wt, err := Create(repo, "feature", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepare(wt)
	entries, err := List(repo)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List: entries=%+v err=%v", entries, err)
	}
	return entries[0]
}

func regressionFakeGit(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func regressionKillChild(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		t.Error(err)
	}
}

func regressionWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
