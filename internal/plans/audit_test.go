package plans

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// trailerCommit lands an empty commit naming plan task id and returns its short sha.
func trailerCommit(t *testing.T, repo, plan, id string) string {
	t.Helper()
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: "+id+"\n\nTask: "+plan+" "+id)
	return strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "--short=7", "HEAD"))
}

func auditProblems(r AuditReport) string {
	var rows []string
	for _, f := range r.Reported {
		rows = append(rows, f.Plan+"|"+f.ID+"|"+f.Problem)
	}
	return strings.Join(rows, "\n")
}

// auditFixture is a repo whose origin has one trailer commit, with plans a and c dated today and b yesterday.
func auditFixture(t *testing.T) (repo, a, b, c, pushed string) {
	t.Helper()
	repo = gitRepo(t, "main")
	origin := t.TempDir()
	hosttest.In(t, origin, "init", "-q", "--bare")
	hosttest.In(t, repo, "remote", "add", "origin", origin)
	pushed = trailerCommit(t, repo, "2026-10-05-a", "1.1")
	hosttest.In(t, repo, "push", "-q", "origin", "main")
	trailerCommit(t, repo, "2026-10-05-a", "1.2")
	trailerCommit(t, repo, "2026-10-05-a", "9.9")
	steps := "- [ ] **you** — Decide D1\n- [ ] Live check on the real surface\n- [ ] Scoped tests for the touched packages\n- [ ] Review (x)\n" +
		"- [ ] Full suite once\n- [ ] Commit, push per task\n- [ ] Handoff / memory update\n"
	home := Home(repo)
	a, b, c = filepath.Join(home, "2026-10-05-a.md"), filepath.Join(home, "2026-10-04-b.md"), filepath.Join(home, "2026-10-05-c.md")
	writeFile(t, a, "- [ ] 1.1 One\n- [ ] 1.2 Two\n")
	writeFile(t, b, "- [x] 1.1 One ("+pushed+")\n- [x] 1.2 Two <!-- working: x --> ("+pushed+")\n"+steps)
	writeFile(t, c, "- [ ] 1.1 One\n- [ ] 1.1 Again\n- [ ] 1.3 Open (abc1234)\n")
	return repo, a, b, c, pushed
}

func TestAuditTicksPushedAndReportsTheRest(t *testing.T) {
	hermetic(t)
	repo, a, b, c, pushed := auditFixture(t)
	all := func() string { return readFile(t, a) + readFile(t, b) + readFile(t, c) }
	before, ctx := all(), context.Background()
	dry, err := Audit(ctx, repo, true)
	if err != nil || len(dry.Changed) != 7 || all() != before {
		t.Fatalf("dry run must plan 7 changes and write none: %d %v", len(dry.Changed), err)
	}
	if _, err := os.Stat(auditLogFile()); err == nil {
		t.Fatal("a dry run must not log")
	}
	got, err := Audit(ctx, repo, false)
	if err != nil || len(got.Changed) != 7 {
		t.Fatalf("changed=%d err=%v", len(got.Changed), err)
	}
	for _, want := range []string{"2026-10-05-a|1.2|landed locally, not pushed", "2026-10-05-a|9.9|trailer names a task the plan does not have",
		"2026-10-04-b|1.2|ticked task still holds a claim", "2026-10-05-c|1.1|duplicate id", "2026-10-05-c|1.3|open task carries a sha"} {
		if !strings.Contains(auditProblems(got), want) {
			t.Errorf("missing finding %q in:\n%s", want, auditProblems(got))
		}
	}
	checkAuditWrites(t, a, b, pushed)
	after := all()
	if again, err := Audit(ctx, repo, false); err != nil || len(again.Changed) != 0 || all() != after {
		t.Fatalf("a second run must change nothing: %d %v", len(again.Changed), err)
	}
}

func checkAuditWrites(t *testing.T, a, b, pushed string) {
	t.Helper()
	if planA := readFile(t, a); !strings.Contains(planA, "[x] 1.1 One ("+pushed+")") || !strings.Contains(planA, "[ ] 1.2") {
		t.Fatalf("only the pushed trailer ticks:\n%s", planA)
	}
	if planB := readFile(t, b); strings.Count(planB, "("+pushed+")") != 2+6 || !strings.Contains(planB, "[ ] **you**") {
		t.Fatalf("process steps tick with the landing sha, the owner line stays:\n%s", planB)
	}
	if log := readFile(t, auditLogFile()); strings.Count(log, "\n") != 7 || !strings.Contains(log, "--reopen --plan") {
		t.Fatalf("each write logs its undo:\n%s", log)
	}
}

// A plan whose build tasks were all struck has its closing steps struck with a reason, never ticked.
func TestAuditStrikesProcessStepsOfAStruckPlan(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "main")
	plan := filepath.Join(Home(repo), "2026-10-04-d.md")
	writeFile(t, plan, "- [ ] ~~1.1 Gone~~\n- [ ] Full suite once\n- [ ] Handoff\n")
	got, err := Audit(context.Background(), repo, false)
	if err != nil || len(got.Changed) != 2 || strings.Count(readFile(t, plan), "~~ — superseded: plan closed: every build task struck") != 2 {
		t.Fatalf("closing steps strike with the reason: %+v %v\n%s", got.Changed, err, readFile(t, plan))
	}
}
