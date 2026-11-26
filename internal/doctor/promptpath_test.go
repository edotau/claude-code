package doctor

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hosttest"
)

const savedDoc = "skills/subagent-workflows/references/saved-workflows.md"

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

// stubSessionSeams swaps the H7 hooks and render for fixtures and restores them after the test.
func stubSessionSeams(t *testing.T, start func(*core.Input) core.Result, recall func(prompt string) (string, error)) {
	t.Helper()
	oStart, oRecall, oRender := memoryStart, memoryRecall, agentsRender
	t.Cleanup(func() { memoryStart, memoryRecall, agentsRender = oStart, oRecall, oRender })
	memoryStart, memoryRecall, agentsRender = start, recall, func(string) error { return nil }
}

func TestH7SessionPromptsNamePathsThatExist(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "memory.md")
	hosttest.WriteRel(t, root, "memory.md", "bank\n")
	ok := func(in *core.Input) core.Result { return core.Result{Context: "see " + real} }
	stubSessionSeams(t, ok, func(string) (string, error) { return "", nil })
	if got := sessionPrompts(root); len(got) != 0 {
		t.Fatalf("pass fixture findings = %q", got)
	}
	gone := filepath.Join(root, "gone.md")
	// A URL route (no such top-level dir) is not a path; a dead file under a real dir is.
	stale := func(string) (string, error) {
		return "claude `/anthropic` via /serving-endpoints/anthropic; pointer " + gone, nil
	}
	stubSessionSeams(t, ok, stale)
	wantOne(t, sessionPrompts(root), "memory-recall:1:", gone+" which does not exist")
}

// H7 skips template paths (<name>, globs, braces, YYYY-MM-DD names) but still reports a real missing path.
func TestH7MissingPathsSkipsPlaceholders(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"angle", "/reports/claude-code--2026-11-16-memory-optimize-<lens>.md", 0},
		{"star", "/gone/*.md", 0},
		{"question", "/gone/a?.md", 0},
		{"brace", "/gone/{a,b}.md", 0},
		{"date", "/plans/YYYY-MM-DD-slug.md", 0},
		{"real missing", "/plans/2026-11-16-slug.md", 1},
		{"month word", "/plans/MMDD-slug.md", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingPaths("memory-start", "see `"+root+tc.path+"` now"); len(got) != tc.want {
				t.Fatalf("findings = %q, want %d", got, tc.want)
			}
		})
	}
}

// H7 skips another OS's home root (shared memory bank) but still checks this machine's own home.
func TestH7ForeignHomePathsSkipped(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		name, goos, path string
		want             int
	}{
		{"darwin skips /home", "darwin", "/home/edotau/worktrees/x/gone.md", 0},
		{"linux skips /Users", "linux", "/Users/edotau/gone.md", 0},
		{"windows skips /home", "windows", "/home/edotau/gone.md", 0},
		{"windows skips /Users", "windows", "/Users/edotau/gone.md", 0},
		{"darwin own home checked", "darwin", home + "/gone.md", 1},
		{"linux own home checked", "linux", home + "/gone.md", 1},
		{"own worktrees skipped", "darwin", home + "/worktrees/claude/wave22", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingPathsOn("memory-start", "see `"+tc.path+"` now", tc.goos, home); len(got) != tc.want {
				t.Fatalf("findings = %q, want %d", got, tc.want)
			}
		})
	}
}

// H7's projection check renders the source in a scratch dir: stale or empty live homes pass, a broken source fails.
func TestH7ProjectionRendersFromSourceNotLiveHomes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // an empty live home: no agent projected anywhere
	hosttest.WriteRel(t, root, "agents/x.md", "---\nname: x\ndescription: does things\n---\nbody\n")
	hosttest.WriteRel(t, root, "CLAUDE.md", "# x\n")
	empty := func(*core.Input) core.Result { return core.Result{} }
	oStart, oRecall := memoryStart, memoryRecall
	t.Cleanup(func() { memoryStart, memoryRecall = oStart, oRecall })
	memoryStart, memoryRecall = empty, func(string) (string, error) { return "", nil }

	if got := sessionPrompts(root); len(got) != 0 {
		t.Fatalf("clean source with an empty live home: findings = %q", got)
	}
	hosttest.WriteRel(t, root, "agents/x.md", "---\nname: y\ndescription: does things\n---\nbody\n")
	wantOne(t, sessionPrompts(root), "agents projection:", "(fix at agents/")
}

func BenchmarkMissingPaths(b *testing.B) {
	ctx := strings.Repeat("see /tmp and ~/.claude/CLAUDE.md, /serving-endpoints, <lens> and `/usr/bin`\n", 50)
	for b.Loop() {
		missingPaths("bench", ctx)
	}
}

func BenchmarkPromptPathStatic(b *testing.B) {
	root := b.TempDir()
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Benchmark\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		claudeMD(root)
		rules(root)
		skills(root)
		workflows(root)
		agents(root)
	}
}

func runCaptured(t *testing.T, args ...string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan []byte, 1) // read while Run writes: output past the pipe buffer would block it
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	code := Run(args)
	os.Stdout = saved
	w.Close()
	return code, string(<-done)
}

func TestPromptsListJSONAccessorsAreArrays(t *testing.T) {
	code, out := runCaptured(t, "--prompts", "--list", "--format", "json")
	var rows []struct {
		Accessors []string `json:"accessors"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil || code != 0 || len(rows) != len(assets.Inventory()) || len(rows[0].Accessors) == 0 {
		t.Fatalf("exit %d, err %v, %d rows", code, err, len(rows))
	}
}

func TestPromptsListPrintsOneRowPerText(t *testing.T) {
	code, out := runCaptured(t, "--prompts", "--list")
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(rows) != len(assets.Inventory()) || !strings.Contains(rows[0], "files/catalog/ranks.json") {
		t.Fatalf("exit %d, %d rows, first %q", code, len(rows), rows[0])
	}
	if Run([]string{"--list"}) != 2 {
		t.Error("--list without --prompts should be a usage error")
	}
}
