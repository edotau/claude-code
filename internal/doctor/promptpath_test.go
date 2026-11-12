package doctor

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hosttest"
)

const savedDoc = "skills/subagent-workflows/references/saved-workflows.md"

// wantOne asserts exactly one finding that contains each fragment.
func wantOne(t *testing.T, got []string, frags ...string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("findings = %q, want one", got)
	}
	for _, f := range frags {
		if !strings.Contains(got[0], f) {
			t.Errorf("finding %q lacks %q", got[0], f)
		}
	}
}

func TestH1ClaudeMDPathsResolve(t *testing.T) {
	root := t.TempDir()
	hosttest.WriteRel(t, root, "rules/standards/quality.md", "# q\n")
	hosttest.WriteRel(t, root, "CLAUDE.md", "# x\nSee `rules/standards/quality.md`.\n")
	if got := claudeMD(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	hosttest.WriteRel(t, root, "CLAUDE.md", "# x\n`rules/gone.md`\n")
	wantOne(t, claudeMD(root), "CLAUDE.md:2:", "`rules/gone.md` does not exist", "(fix at CLAUDE.md)")

	// A gitignored path (local-only, absent on a fresh clone) is not a finding; an unignored one still is.
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	hosttest.WriteRel(t, root, ".gitignore", "/skills/synced/\n")
	hosttest.WriteRel(t, root, "CLAUDE.md", "# x\n`skills/synced/` and `rules/gone.md`\n")
	wantOne(t, claudeMD(root), "`rules/gone.md` does not exist")
}

func TestH2RulesPathsAndSymbols(t *testing.T) {
	root := t.TempDir()
	hosttest.WriteRel(t, root, "internal/x/bar.go", "package x\n\nfunc foo() {}\n")
	body := "---\npaths:\n  - x\n---\nUse `foo` (`internal/x/bar.go`).\n"
	hosttest.WriteRel(t, root, "rules/harness/a.md", body)
	if got := rules(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	hosttest.WriteRel(t, root, "rules/harness/a.md", "---\npaths:\n  - x\n---\nUse `gone` (`internal/x/bar.go`).\n")
	wantOne(t, rules(root), "rules/harness/a.md:5:", "`gone` not found in `internal/x/bar.go`", "(fix at rules/harness/a.md)")
}

func TestH3SkillNameMatchesDir(t *testing.T) {
	root := t.TempDir()
	hosttest.WriteRel(t, root, "skills/foo/SKILL.md", "---\nname: foo\ndescription: x\n---\n")
	if got := skills(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	hosttest.WriteRel(t, root, "skills/foo/SKILL.md", "---\nname: bar\ndescription: x\n---\n")
	wantOne(t, skills(root), `skills/foo/SKILL.md:1: name "bar" != "foo"`, "(fix at skills/foo/SKILL.md)")
}

func TestH4PlanTemplateHasEveryHeading(t *testing.T) {
	root := t.TempDir()
	heads := docs.PlanTemplateHeadings()
	hosttest.WriteRel(t, root, "docs/templates/plan.md", strings.Join(heads, "\n")+"\n")
	if got := planTemplate(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	hosttest.WriteRel(t, root, "docs/templates/plan.md", strings.Join(heads[1:], "\n")+"\n")
	wantOne(t, planTemplate(root), `docs/templates/plan.md:1: template lacks "# Plan:"`, "(fix at docs/templates/plan.md)")
}

func TestH5SavedWorkflowsInSync(t *testing.T) {
	root := t.TempDir()
	hosttest.WriteRel(t, root, "workflows/a.js", "// a\n")
	hosttest.WriteRel(t, root, savedDoc, "| Name |\n|---|\n| `a` | runs a |\n")
	if got := workflows(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	hosttest.WriteRel(t, root, savedDoc, "| Name |\n|---|\n| `a` | runs a |\n| `ghost` | gone |\n")
	wantOne(t, workflows(root), savedDoc+":1:", "ghost has no workflows/ghost.js", "(fix at "+savedDoc+")")
}

func TestH6AgentNameStemDescriptionModel(t *testing.T) {
	root := t.TempDir()
	hosttest.WriteRel(t, root, "agents/x.md", "---\nname: x\ndescription: does things\nmodel: sonnet\n---\nbody\n")
	if got := agents(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	hosttest.WriteRel(t, root, "agents/x.md", "---\nname: y\ndescription: does things\nmodel: sonnet\n---\nbody\n")
	wantOne(t, agents(root), `agents/x.md:1: name "y" != "x"`, "(fix at agents/x.md)")
}

// stubSessionSeams swaps the H7 hooks and sync for fixtures and restores them after the test.
func stubSessionSeams(t *testing.T, start func(*core.Input) core.Result, recall func(root, prompt string) (string, error), sync func() int) {
	t.Helper()
	oStart, oRecall, oSync := memoryStart, memoryRecall, agentsSync
	t.Cleanup(func() { memoryStart, memoryRecall, agentsSync = oStart, oRecall, oSync })
	memoryStart, memoryRecall, agentsSync = start, recall, sync
}

func TestH7SessionPromptsNamePathsThatExist(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "memory.md")
	hosttest.WriteRel(t, root, "memory.md", "bank\n")
	ok := func(in *core.Input) core.Result { return core.Result{Context: "see " + real} }
	stubSessionSeams(t, ok, func(string, string) (string, error) { return "", nil }, func() int { return 0 })
	if got := sessionPrompts(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	gone := filepath.Join(root, "gone.md")
	// A URL route (no such top-level dir) is not a path; a dead file under a real dir is.
	stale := func(string, string) (string, error) {
		return "claude `/anthropic` via /serving-endpoints/anthropic; pointer " + gone, nil
	}
	stubSessionSeams(t, ok, stale, func() int { return 0 })
	wantOne(t, sessionPrompts(root), "memory-recall:1:", gone+" which does not exist")
}
