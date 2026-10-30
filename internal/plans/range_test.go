package plans

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestTickRange ticks a plan from real commits: a partial commit, a completing one, dry-run, and the unticked base.
func TestTickRange(t *testing.T) {
	hermetic(t)
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	base := gitT(t, repo, "rev-parse", "HEAD")
	commit := func(rel string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, "x")
		gitT(t, repo, "add", rel)
		gitT(t, repo, "commit", "-q", "-m", rel)
	}
	commit("internal/a/a.go")
	commit("internal/b/b.go")
	commit("README.md")

	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Solo owns: internal/b/b.go\n- [ ] 1.2 Both owns: internal/a/a.go, internal/b/b.go\n- [ ] 1.3 Other owns: internal/zzz/\n")
	commits, err := RangeCommits(context.Background(), repo, base+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 || commits[0].Files[0] != "internal/a/a.go" || len(commits[0].Sha) != 7 {
		t.Fatalf("commits oldest first, 7-char shas: %+v", commits)
	}

	res, err := TickCommits(plan, commits, true)
	if err != nil || len(res) != 2 || len(res[0].Partial) != 1 || len(res[1].Ticked) != 2 || len(res[1].Partial) != 0 {
		t.Fatalf("dry-run results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); strings.Contains(got, "[x]") || strings.Contains(got, "partly") {
		t.Fatalf("dry-run wrote the plan:\n%s", got)
	}

	if _, err = TickCommits(plan, commits, false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, plan)
	if !strings.Contains(got, "- [x] 1.1 Solo") || !strings.Contains(got, "("+commits[1].Sha+")") || !strings.Contains(got, "- [ ] 1.3") {
		t.Fatalf("applied plan:\n%s", got)
	}
	if _, err := RangeCommits(context.Background(), repo, "HEAD"); err == nil {
		t.Fatal("a range without .. must be rejected")
	}
}

// A task whose owned files land across several commits ticks once the range covers them all.
func TestTickCommitsUnionsRange(t *testing.T) {
	hermetic(t)
	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Split owns: a.go, b.go, c.go\n")
	commits := []Commit{{Sha: "aaa1111", Files: []string{"a.go"}}, {Sha: "bbb2222", Files: []string{"b.go"}}, {Sha: "ccc3333", Files: []string{"c.go"}}}
	res, err := TickCommits(plan, commits, false)
	if err != nil || len(res) != 3 || len(res[2].Ticked) != 1 {
		t.Fatalf("results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); !strings.Contains(got, "- [x] 1.1") || !strings.Contains(got, "(ccc3333)") {
		t.Fatalf("plan:\n%s", got)
	}
}

// An owns entry abbreviated against the previous one resolves to the spliced path.
func TestCoversSplicesAbbreviatedOwns(t *testing.T) {
	owns := []string{"skills/subagent-workflows/references/prompts.md", "references/cost-routing.md", "references/lane.md"}
	if all, _ := Covers(owns, []string{"skills/subagent-workflows/references/prompts.md", "skills/subagent-workflows/references/cost-routing.md", "skills/subagent-workflows/references/lane.md"}); !all {
		t.Fatal("abbreviated entry not resolved")
	}
	if all, _ := Covers([]string{"a/b.go", "references/x.md"}, []string{"a/b.go", "references/x.md"}); !all {
		t.Fatal("repo-root entry must stay as is")
	}
}

// TestDirOwnsNeverComplete is the regression for a directory owns entry ticking a task on one stray file.
func TestDirOwnsNeverComplete(t *testing.T) {
	if all, any := Covers([]string{"internal/proxy"}, []string{"internal/proxy/x.go"}); all || !any {
		t.Fatalf("dir-only owns: all=%v any=%v, want touched only", all, any)
	}
	if all, _ := Covers([]string{"internal/proxy/", "internal/a.go"}, []string{"internal/a.go"}); all {
		t.Fatal("dir entry untouched must keep the task open")
	}
	if all, _ := Covers([]string{"internal/proxy", "internal/a.go"}, []string{"internal/a.go", "internal/proxy/x.go"}); !all {
		t.Fatal("files plus touched dir should complete")
	}
}

// TestSelectPicksOwningPlan is the regression for annotating the newest plan instead of the one the range belongs to.
func TestSelectPicksOwningPlan(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-roles.md"), "- [ ] 1.1 Roles owns: internal/roles/r.go\n")
	writeFile(t, filepath.Join(home, "2026-10-02-gateway.md"), "- [ ] 2.1 Proxy owns: internal/proxy\n- [ ] 2.2 Other owns: internal/gw/g.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/roles/r.go", "internal/proxy/h.go"}}}
	plan, tied := Select(home, "", nil, commits)
	if filepath.Base(plan) != "2026-10-01-roles.md" || tied != nil {
		t.Fatalf("plan=%q tied=%v", plan, tied)
	}
	writeFile(t, filepath.Join(home, "2026-10-03-dup.md"), "- [ ] 3.1 Roles owns: internal/roles/r.go\n")
	if plan, tied = Select(home, "", nil, commits); plan != "" || len(tied) != 2 {
		t.Fatalf("equal match must annotate nothing: plan=%q tied=%v", plan, tied)
	}
	writeFile(t, filepath.Join(home, "2026-10-03-dup.md"), "- [ ] 3.1 Roles owns: internal/roles/r.go <!-- working: x · worktree wt -->\n")
	if plan, _ = Select(home, "wt", nil, commits); filepath.Base(plan) != "2026-10-03-dup.md" {
		t.Fatalf("worktree claim should break the tie, got %q", plan)
	}
}
