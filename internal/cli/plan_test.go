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
