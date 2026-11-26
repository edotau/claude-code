package docs

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/autofix"
	"github.com/edotau/claude-code/internal/worktree"
)

const (
	autofixGofmtOut   = "FAIL gofmt (0.0s)\npkg/x.go: gofmt: needs formatting\n"
	autofixComplexOut = "FAIL complexity (0.0s)\npkg/x.go:3: complexity/cyclomatic-complexity: warn 'f' has " +
		"cyclomatic complexity 12 (max 10). Flatten branching.\n"
)

// stubAutofix replaces every lane, launch and budget seam with a recorder; calls lists them in order.
func stubAutofix(t *testing.T, budgetOK bool) *[]string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	calls := &[]string{}
	old := []any{autofixRepair, autofixLeaf, autofixLaneCap, autofixBudget, autofixReserve, autofixVerb}
	t.Cleanup(func() {
		autofixRepair = old[0].(func(context.Context, string, string, []autofix.Finding) (autofix.Result, error))
		autofixLeaf = old[1].(func(context.Context, autofix.LeafRequest, func() (bool, string)) (autofix.Attempt, error))
		autofixLaneCap = old[2].(func(context.Context, string) error)
		autofixBudget = old[3].(func() (bool, string))
		autofixReserve = old[4].(func(string, string, time.Time) (bool, string))
		autofixVerb = old[5].(func(context.Context, string, ...string) (string, error))
	})
	autofixRepair = func(_ context.Context, _, sha string, _ []autofix.Finding) (autofix.Result, error) {
		*calls = append(*calls, "repair")
		return autofix.Result{Notice: "autofix " + sha[:7] + ": repaired"}, nil
	}
	autofixLeaf = func(_ context.Context, req autofix.LeafRequest, budget func() (bool, string)) (autofix.Attempt, error) {
		*calls = append(*calls, "leaf")
		if ok, why := budget(); !ok {
			return autofix.Attempt{}, autofix.ErrBudget
		} else if why != "" || req.Lane == "" {
			t.Errorf("leaf: why %q lane %q", why, req.Lane)
		}
		return autofix.Attempt{Verdict: "dispatched", DispatchID: "job1"}, nil
	}
	autofixLaneCap = func(context.Context, string) error { return nil }
	autofixBudget = func() (bool, string) {
		*calls = append(*calls, "budget")
		if !budgetOK {
			return false, "daily gate-fix count limit reached"
		}
		return true, ""
	}
	autofixReserve = func(string, string, time.Time) (bool, string) { *calls = append(*calls, "reserve"); return true, "" }
	autofixVerb = func(_ context.Context, _ string, args ...string) (string, error) {
		*calls = append(*calls, strings.Join(args[:2], " "))
		return "/tmp/lane\n", nil
	}
	oldSpend, oldAttempts, oldWait, oldSettled := autofixSpend, autofixAttempts, autofixLockWait, autofixSettled
	t.Cleanup(func() {
		autofixSpend, autofixAttempts, autofixLockWait, autofixSettled = oldSpend, oldAttempts, oldWait, oldSettled
	})
	autofixSpend = func(_, id string, _ time.Time) error { *calls = append(*calls, "spend "+id); return nil }
	autofixAttempts = func() ([]autofix.Attempt, error) { return nil, nil }
	autofixSettled = func(string) bool { return false }
	autofixLockWait = 20 * time.Millisecond
	return calls
}

func TestGateAutofix(t *testing.T) {
	tests := []struct {
		name, out, notice  string
		budgetOK, offTrunk bool
		want               []string
	}{
		{"tool-fixable only", autofixGofmtOut, "repaired", true, false, []string{"repair"}},
		{"judgment only", autofixComplexOut, "fix leaf job1 started",
			true, false, []string{"budget", "worktree create", "leaf", "budget", "reserve"}},
		{"both in order", autofixGofmtOut + autofixComplexOut, "fix leaf job1 started",
			true, false, []string{"repair", "budget", "worktree create", "leaf", "budget", "reserve"}},
		{"budget refused", autofixComplexOut, "budget refused: daily gate-fix count limit reached",
			false, false, []string{"budget"}},
		{"not primary trunk", autofixGofmtOut + autofixComplexOut, "skipped", true, true, nil},
		{"green gate", "", "", true, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubAutofix(t, tc.budgetOK)
			dir, sha := benchRepo(t)
			if tc.offTrunk {
				if out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "branch", "-m", "main", "feature").CombinedOutput(); err != nil {
					t.Fatalf("git branch -m: %v %s", err, out)
				}
			}
			lines := GateAutofix(dir, sha, tc.out)
			if !slices.Equal(*calls, tc.want) {
				t.Errorf("calls = %q, want %q", *calls, tc.want)
			}
			if got := strings.Join(lines, "\n"); tc.notice == "" && got != "" || !strings.Contains(got, tc.notice) {
				t.Errorf("notice = %q, want %q", got, tc.notice)
			}
		})
	}
}

// the sweep spends each of today's and yesterday's leaves once; a settled one is never swept again.
func TestSweepAutofixSpend(t *testing.T) {
	calls := stubAutofix(t, false)
	now := time.Now()
	autofixAttempts = func() ([]autofix.Attempt, error) {
		return []autofix.Attempt{{At: now, Sha: "x", Verdict: "dispatching"}, {At: now, Sha: "x", DispatchID: "job1"},
			{At: now, Sha: "y", DispatchID: "job1"}, {At: now.AddDate(0, 0, -1), Sha: "z", DispatchID: "job2"},
			{At: now.AddDate(0, 0, -3), Sha: "w", DispatchID: "job3"}}, nil
	}
	autofixSettled = func(id string) bool { return id == "job1" }
	SweepAutofixSpend(now)
	SweepAutofixSpend(now)
	if want := []string{"spend job1", "spend job2", "spend job2"}; !slices.Equal(*calls, want) {
		t.Fatalf("calls = %q, want %q", *calls, want)
	}
}

// any absolute path in a notice is reduced, whatever character precedes it.
func TestAutofixNoticeAbsPaths(t *testing.T) {
	got := autofixNotice("x open:/Users/x/y/z.go [/Users/x/y] done")
	if strings.Contains(got, "/Users") || !strings.Contains(got, "open:z.go") || !strings.Contains(got, "[y]") {
		t.Fatalf("notice = %q", got)
	}
}

// a held run lock is "autofix busy" at once, and nothing runs.
func TestGateAutofixBusy(t *testing.T) {
	calls := stubAutofix(t, true)
	dir, sha := benchRepo(t)
	var lines []string
	start := time.Now()
	err := paths.WithFileLock(filepath.Join(paths.StateDir(), "autofix", "run"), func() error {
		lines = GateAutofix(dir, sha, autofixComplexOut)
		return nil
	})
	if err != nil || len(*calls) != 0 || !strings.Contains(strings.Join(lines, "\n"), "autofix busy") ||
		time.Since(start) > time.Second {
		t.Fatalf("lines = %q calls = %q err = %v after %s", lines, *calls, err, time.Since(start))
	}
}

// notices carry no absolute path and no command line, and name lanes by name.
func TestGateAutofixNoticeSanitised(t *testing.T) {
	const lane = "autofix-abcdefa"
	stubAutofix(t, true)
	autofixRepair = func(context.Context, string, string, []autofix.Finding) (autofix.Result, error) {
		return autofix.Result{Notice: lane + ": fixer failed: read /Users/someone/worktrees/claude/" +
				lane + "/internal/x.go; lane removed"},
			errors.New("create autofix lane: run /Users/someone/.claude/bin/claude-code worktree create " +
				lane + ": exit status 1\n/Users/someone/second")
	}
	dir, sha := benchRepo(t)
	got := strings.Join(GateAutofix(dir, sha, autofixGofmtOut), "\n")
	if strings.Contains(got, "/Users") || strings.Contains(got, "worktree create") ||
		!strings.Contains(got, lane+"/internal/x.go") || !strings.Contains(got, "exit status 1") {
		t.Fatalf("notice = %q", got)
	}
}

// autofix works a failure it claims, skips one another session holds, and a reopened failure is claimable again.
func TestGateAutofixClaimsFailure(t *testing.T) {
	calls := stubAutofix(t, true)
	dir, sha := benchRepo(t)
	root := worktree.FailuresDir()
	mine, theirs := worktree.FailureID(worktree.FailPlanCheck, "p 1.1@abcdef0"), worktree.FailureID(worktree.FailPlanCheck, "p 1.2@abcdef0")
	if _, err := worktree.ClaimFailure(root, theirs, "other"); err != nil {
		t.Fatal(err)
	}
	if lines := GateAutofix(dir, sha, autofixComplexOut, theirs); len(*calls) != 0 || !strings.Contains(strings.Join(lines, "\n"), "held by another") {
		t.Fatalf("held failure ran: lines %q calls %q", lines, *calls)
	}
	GateAutofix(dir, sha, autofixComplexOut, mine)
	if h, _ := worktree.FailureHolder(root, mine); h != "autofix-"+sha[:7] || len(*calls) == 0 {
		t.Fatalf("holder %q calls %q", h, *calls)
	}
}
