package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// planRepo is a repo (the cwd) whose plans home holds one plan per name=body pair.
func planRepo(t *testing.T, named map[string]string) string {
	t.Helper()
	cfgDir(t)
	repo := paths.Real(hosttest.Repo(t))
	t.Chdir(repo)
	for name, body := range named {
		hosttest.WriteRel(t, plans.Home(repo), name, body)
	}
	return repo
}

func readPlan(t *testing.T, repo, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(plans.Home(repo), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// plan status prints the dropped count only for a plan that has dropped tasks.
func TestPlanStatusRowsShowDroppedOnlyWhenPresent(t *testing.T) {
	planRepo(t, map[string]string{
		"2026-11-01-a.md": "- [x] 1.1 one\n- [ ] 1.2 two\n- [ ] ~~1.3 three~~ — superseded: x\n",
		"2026-11-02-b.md": "- [x] 2.1 one\n- [ ] 2.2 two\n",
	})
	code, out, _ := runCLI(t, "plan", "status")
	if code != 0 || !strings.Contains(out, "1/2 (1 dropped)  ") || !strings.Contains(out, "1/2  ") || strings.Count(out, "dropped") != 1 {
		t.Fatalf("exit %d, out:\n%s", code, out)
	}
}

// plan task: done, reopen and strike by hand, and a refusal naming every plan when the id is not unique.
func TestPlanTaskDoneReopenStrikeAndAmbiguousId(t *testing.T) {
	repo := planRepo(t, map[string]string{
		"2026-11-01-a.md": "- [ ] 1.1 Manual step\n- [ ] 1.2 Two lines first part\n  and the second part\n",
		"2026-11-02-b.md": "- [ ] 1.1 Same id elsewhere\n",
	})
	code, _, stderr := runCLI(t, "plan", "task", "1.1", "--done")
	if code == 0 || !strings.Contains(stderr, "2026-11-01-a.md") || !strings.Contains(stderr, "2026-11-02-b.md") {
		t.Fatalf("a non-unique id must be refused naming both plans: exit %d %q", code, stderr)
	}
	a := filepath.Join(plans.Home(repo), "2026-11-01-a.md")
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"plan", "task", "1.1", "--done", "--sha", "abc1234", "--plan", a}, "- [x] 1.1 Manual step (abc1234)\n"},
		{[]string{"plan", "task", "1.1", "--reopen", "--plan", a}, "- [ ] 1.1 Manual step\n"},
		{[]string{"plan", "task", "1.2", "--strike", "folded in", "--plan", a},
			"- [ ] ~~1.2 Two lines first part and the second part~~ — superseded: folded in\n"},
	}
	for _, s := range steps {
		if code, _, stderr := runCLI(t, s.args...); code != 0 || !strings.Contains(readPlan(t, repo, "2026-11-01-a.md"), s.want) {
			t.Fatalf("%v: exit %d %q\n%s", s.args, code, stderr, readPlan(t, repo, "2026-11-01-a.md"))
		}
	}
	if code, _, _ := runCLI(t, "plan", "task", "1.1"); code == 0 {
		t.Fatal("no mode must fail")
	}
}

// plan task is offered by completion, so TestCompletionMatchesHelpFlags then holds its flags to -h.
func TestPlanSpecOffersTask(t *testing.T) {
	if planSpec().Sub["task"] == nil {
		t.Fatal("completion spec has no plan task")
	}
}

func TestPlanExplicitBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := planRepo(t, map[string]string{"2026-11-18-safe.md": "- [ ] 1.1 Safe\n", "draft.md": "- [ ] 1.1 Draft\n"})
	home := plans.Home(repo)
	outside := filepath.Join(t.TempDir(), "2026-11-18-outside.md")
	hosttest.WriteRel(t, filepath.Dir(outside), filepath.Base(outside), "- [ ] 1.1 Outside\n")
	link := filepath.Join(home, "2026-11-18-link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	insideLink := filepath.Join(home, "2026-11-18-inside-link.md")
	if err := os.Symlink(filepath.Join(home, "2026-11-18-safe.md"), insideLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link, insideLink, filepath.Join(home, "draft.md"), home, filepath.Join(home, "..", filepath.Base(outside))} {
		if _, err := plans.TaskTrailers(repo, path, []string{"1.1"}); err == nil {
			t.Errorf("commit accepted %s", path)
		}
		code, _, stderr := runCLI(t, "plan", "tick", "--range", "HEAD..HEAD", "--plan", path)
		if code != 2 || !strings.Contains(stderr, home) {
			t.Errorf("tick %s: exit=%d stderr=%q", path, code, stderr)
		}
		code, _, stderr = runCLI(t, "plan", "task", "1.1", "--done", "--plan", path)
		if code != 2 || !strings.Contains(stderr, home) || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
			t.Errorf("%s: exit=%d stderr=%q", path, code, stderr)
		}
	}
	for _, name := range []string{"2026-11-18-safe.md", "2026-11-18-safe"} {
		code, _, stderr := runCLI(t, "plan", "task", "1.1", "--done", "--plan", name)
		if code != 0 {
			t.Errorf("%s: exit=%d %s", name, code, stderr)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "- [ ] 1.1 Outside\n" {
		t.Fatalf("outside changed: %s %v", got, err)
	}
}
