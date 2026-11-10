package docs

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// placementRepo makes a temp git repo with one tracked root doc, so the gate has real git state.
func placementRepo(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(hosttest.Repo(t))
	if err := os.WriteFile(filepath.Join(dir, "ARCHITECTURE.md"), []byte("# arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "ARCHITECTURE.md"}, {"commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return dir
}

func placementPayload(t *testing.T, tool, path string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tool_name": tool, "tool_input": map[string]string{"file_path": path}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDocsPlacementFirstDenial(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plans := filepath.Join(cfg, "plans")
	if err := os.MkdirAll(plans, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", plans, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	for _, c := range []struct {
		knob string
		hook func(*core.Input, io.Writer) int
		want string
		not  string
	}{
		{"1", Placement, "docs-placement: BLOCKED", "plan-template: BLOCKED"},
		{"0", Gate, "plan-template: BLOCKED", "docs-placement: BLOCKED"},
	} {
		t.Run("docs="+c.knob, func(t *testing.T) {
			t.Setenv(docsPlacementDisableEnv, c.knob)
			var stderr bytes.Buffer
			payload := writePayload(t, "Write", cfg, filepath.Join(plans, "draft.md"), "freeform")
			if code := c.hook(core.ParseInput(strings.NewReader(payload)), &stderr); code != core.ExitBlock {
				t.Fatalf("code = %d, want ExitBlock; stderr=%s", code, &stderr)
			}
			if !strings.HasPrefix(stderr.String(), c.want) || strings.Contains(stderr.String(), c.not) {
				t.Errorf("want only %s denial, got %s", c.want, &stderr)
			}
		})
	}
}

func TestDocsPlacementPatchArgumentKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(docsPlacementDisableEnv, "1")
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("docs/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"patch", "input", "command"} {
		for _, directive := range []string{"*** Add File: ", "*** Update File: ", "*** Move to: "} {
			for _, path := range []string{"STRAY.md", "docs/STRAY.md"} {
				t.Run(key+"/"+directive+path, func(t *testing.T) {
					input := map[string]any{"patch": 42, "input": "not a patch", "command": "not a patch"}
					input[key] = "*** Begin Patch\n" + directive + path + "\n+hi\n*** End Patch"
					raw, err := json.Marshal(map[string]any{"tool_name": "apply_patch", "cwd": repo, "tool_input": input})
					if err != nil {
						t.Fatal(err)
					}
					var stderr bytes.Buffer
					if code := Placement(core.ParseInput(bytes.NewReader(raw)), &stderr); code != core.ExitBlock {
						t.Fatalf("code = %d, want block: %s", code, &stderr)
					}
					if !strings.Contains(stderr.String(), "docs-placement: BLOCKED") {
						t.Fatalf("missing block message: %s", &stderr)
					}
				})
			}
		}
	}
}

func TestDocsPlacementGate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".claude", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		tool  string
		path  string
		block bool
	}{
		{"new plan at repo root", "Write", filepath.Join(repo, "IMPLEMENTATION_PLAN.md"), true},
		{"handoff at repo root", "Write", filepath.Join(repo, "HANDOFF.md"), true},
		{"markdown ext variant", "Write", filepath.Join(repo, "notes.markdown"), true},
		{"edit of untracked root doc", "Edit", filepath.Join(repo, "SCRATCH.md"), true},
		{"root anchor", "Write", filepath.Join(repo, "README.md"), false},
		{"cross-tool render", "Write", filepath.Join(repo, "AGENTS.md"), false},
		{"tracked repo doc", "Edit", filepath.Join(repo, "ARCHITECTURE.md"), false},
		{"repo subdir", "Write", filepath.Join(repo, "docs", "design.md"), false},
		{"inside .claude/docs", "Write", filepath.Join(repo, ".claude", "docs", "plans", "x.md"), false},
		{"non-markdown at root", "Write", filepath.Join(repo, "main.go"), false},
		{"other tool", "Bash", filepath.Join(repo, "X.md"), false},
		{"outside any repo", "Write", filepath.Join(t.TempDir(), "X.md"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, c.tool, c.path))), &stderr)
			want := core.ExitProceed
			if c.block {
				want = core.ExitBlock
			}
			if code != want {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, want, stderr.String())
			}
			if c.block && !strings.Contains(stderr.String(), "docs-placement: BLOCKED") {
				t.Errorf("block message missing the gate name: %s", stderr.String())
			}
		})
	}
}

// TestDocsPlacementNamesCanonicalPaths: a block must tell the model exactly where to write instead.
func TestDocsPlacementNamesCanonicalPaths(t *testing.T) {
	global := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", global)
	repo := placementRepo(t)

	var stderr bytes.Buffer
	if code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "PLAN.md")))), &stderr); code != core.ExitBlock {
		t.Fatalf("code = %d, want block", code)
	}
	for _, want := range []string{
		filepath.Join(global, "plans"),
		string(filepath.Separator) + "handoffs" + string(filepath.Separator),
		string(filepath.Separator) + "reports" + string(filepath.Separator),
		string(filepath.Separator) + "knowledge" + string(filepath.Separator),
		"(local-only)",
		"HARNESS_DOCS_PLACEMENT=0",
		"rules/harness/docs-placement.md",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("block message missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestDocsPlacementKnobOff: the gate is tunable off via HARNESS_DOCS_PLACEMENT=0.
func TestDocsPlacementKnobOff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(docsPlacementDisableEnv, "0")
	repo := placementRepo(t)

	var stderr bytes.Buffer
	if code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "PLAN.md")))), &stderr); code != core.ExitProceed {
		t.Fatalf("code = %d, want proceed when the knob is 0 (stderr: %s)", code, stderr.String())
	}
}

// TestDocsPlacementThroughSymlink: a sibling symlink into the repo still resolves to the realpath git
// reports, so a root doc written by its symlinked name must block exactly as the physical one does,
// and a subdir under that name must still be let through.
func TestDocsPlacementThroughSymlink(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)
	link := filepath.Join(t.TempDir(), "linked-repo")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		path  string
		block bool
	}{
		{"root doc through the link", filepath.Join(link, "HANDOFF.md"), true},
		{"root anchor through the link", filepath.Join(link, "README.md"), false},
		{"subdir through the link", filepath.Join(link, "docs", "design.md"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", c.path))), &stderr)
			want := core.ExitProceed
			if c.block {
				want = core.ExitBlock
			}
			if code != want {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, want, stderr.String())
			}
		})
	}
}

// isTracked hands the basename to git as a PATHSPEC: a glob-shaped name would otherwise match some
// other tracked file (ARCHITECTURE.md here) and wave the untracked root write straight through.
func TestDocsPlacementGlobShapedNameStillBlocks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)

	var stderr bytes.Buffer
	if code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "*.md")))), &stderr); code != core.ExitBlock {
		t.Fatalf("code = %d, want block for a glob-shaped root doc (stderr: %s)", code, stderr.String())
	}
}

// ignoredDocsRepo is placementRepo plus a gitignored docs/ — writes there land, then vanish on a fresh clone.
func ignoredDocsRepo(t *testing.T) string {
	t.Helper()
	dir := placementRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-q", "-m", "ignore docs"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return dir
}

// TestDocsPlacementIgnoredDocsDir: <repo>/docs is gated too, but only where git REFUSES to keep the
// page. A repo that owns docs/ is untouched (its new pages are merely not-yet-added, not ignored).
func TestDocsPlacementIgnoredDocsDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ignored := ignoredDocsRepo(t)
	owned := placementRepo(t)
	if err := os.MkdirAll(filepath.Join(owned, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A force-added page inside the ignored dir IS the repo's own doc — tracked wins over ignored.
	if err := os.WriteFile(filepath.Join(ignored, "docs", "kept.md"), []byte("# kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-f", "docs/kept.md"}, {"commit", "-q", "-m", "keep"}} {
		if out, err := exec.Command("git", append([]string{"-C", ignored}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	for _, c := range []struct {
		name  string
		path  string
		block bool
	}{
		{"handoff in an ignored docs dir", filepath.Join(ignored, "docs", "handoff-x.md"), true},
		{"report nested under it", filepath.Join(ignored, "docs", "reports", "y.md"), true},
		{"force-added page there", filepath.Join(ignored, "docs", "kept.md"), false},
		{"root anchor still exempt", filepath.Join(ignored, "README.md"), false},
		{"non-docs subdir", filepath.Join(ignored, "src", "notes.md"), false},
		{"docs dir the repo owns", filepath.Join(owned, "docs", "design.md"), false},
		{"nested page the repo owns", filepath.Join(owned, "docs", "adr", "0001.md"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", c.path))), &stderr)
			want := core.ExitProceed
			if c.block {
				want = core.ExitBlock
			}
			if code != want {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, want, stderr.String())
			}
		})
	}
}

// TestDocsPlacementExcludedLocalDoc: an existing git-excluded file is editable; a new excluded name still blocks.
func TestDocsPlacementExcludedLocalDoc(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := ignoredDocsRepo(t)
	exclude := "DESIGN.md\nNOTES.md\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(exclude), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"DESIGN.md", "docs/local.md"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("# local\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{"DESIGN.md", core.ExitProceed}, {"docs/local.md", core.ExitProceed}, {"NOTES.md", core.ExitBlock}, {"docs/new.md", core.ExitBlock},
	} {
		var stderr bytes.Buffer
		code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Edit", filepath.Join(repo, c.path)))), &stderr)
		if code != c.want {
			t.Errorf("%s: code = %d, want %d (stderr: %s)", c.path, code, c.want, stderr.String())
		}
	}
}

// TestDocsPlacementHarnessDocsTreeExempt: the harness docs tree admits only images/ and templates/ — the
// knowledge/ tier retired out of docs/ entirely, to memory/<bank>/knowledge/, so it blocks here like any
// other retired home. docs/templates-evil (a sibling, not a child) must still block — a prefix-string bug
// would wave it through.
func TestDocsPlacementHarnessDocsTreeExempt(t *testing.T) {
	harness := ignoredDocsRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", harness)

	for name, want := range map[string]int{
		"knowledge/topic.md": core.ExitBlock, "knowledge/repo/x.md": core.ExitBlock, "images/n.md": core.ExitProceed,
		"templates/README.md": core.ExitProceed, "templates/examples/memory/index.md": core.ExitProceed,
		"handoffs/HANDOFF.md": core.ExitBlock, "reports/repo/y.md": core.ExitBlock, "plans/2026-09-09-x.md": core.ExitBlock,
		"other/x.md": core.ExitBlock, "templates-evil/x.md": core.ExitBlock,
	} {
		var stderr bytes.Buffer
		p := filepath.Join(harness, "docs", filepath.FromSlash(name))
		if code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", p))), &stderr); code != want {
			t.Errorf("%s: code = %d, want %d (stderr: %s)", name, code, want, stderr.String())
		}
	}
}

// TestDocsPlacementGitFileRoot: a linked worktree's `.git` is a FILE, and the stat walk-up must still
// find its root; a `.git` file git rejects fails open rather than blocking on an unreadable repo.
func TestDocsPlacementGitFileRoot(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)
	wt := filepath.Join(filepath.Dir(repo), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	bogus, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(bogus, ".git"), []byte("gitdir: /nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		path string
		want int
	}{
		{"root doc in a linked worktree", filepath.Join(wt, "HANDOFF.md"), core.ExitBlock},
		{"tracked doc in a linked worktree", filepath.Join(wt, "ARCHITECTURE.md"), core.ExitProceed},
		{"root doc under an invalid .git file", filepath.Join(bogus, "HANDOFF.md"), core.ExitProceed},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", c.path))), &stderr); code != c.want {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, c.want, stderr.String())
			}
		})
	}
}

// TestDocsPlacementPlanHomeUnderMemoryRepo: a root .md in memory/<repo> (its own git repo) names the untagged harness plans dir.
func TestDocsPlacementPlanHomeUnderMemoryRepo(t *testing.T) {
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(cfg, "memory", "claude-code")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	var stderr bytes.Buffer
	in := core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "PLAN.md"))))
	if code := Placement(in, &stderr); code != core.ExitBlock {
		t.Fatalf("code = %d, want block", code)
	}
	if want := "  plan             -> " + filepath.Join(cfg, "plans") + "/YYYY"; !strings.Contains(stderr.String(), want) {
		t.Errorf("plan line missing %q:\n%s", want, stderr.String())
	}
	if want := "  handoff          -> " + filepath.Join(repo, "handoffs"); !strings.Contains(stderr.String(), want) {
		t.Errorf("handoff line must name the store itself, not memory/home; missing %q:\n%s", want, stderr.String())
	}
	for _, sub := range []string{"handoffs", "reports"} {
		var e bytes.Buffer
		in := core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, sub, "2026-11-07-x.md"))))
		if code := Placement(in, &e); code != core.ExitProceed {
			t.Errorf("%s inside the store = %d, want proceed (%s)", sub, code, e.String())
		}
	}
}

// TestMain stubs the live-process scan so no docs test reads the real machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	code := m.Run()
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
