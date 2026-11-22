package docs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
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
		name string
		hook func(*core.Input, io.Writer) int
		want string
		not  string
	}{
		{"placement", placeFiles, "docs-placement: BLOCKED", "plan-template: BLOCKED"},
		{"gate", Gate, "plan-template: BLOCKED", "docs-placement: BLOCKED"},
	} {
		t.Run(c.name, func(t *testing.T) {
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
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("docs/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"patchText", "patch", "input", "command"} {
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
					if code := placeFiles(core.ParseInput(bytes.NewReader(raw)), &stderr); code != core.ExitBlock {
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
	notes := os.WriteFile(filepath.Join(repo, "NOTES.md"), []byte("owner notes\n"), 0o644) // an existing untracked root doc
	if err := errors.Join(os.MkdirAll(filepath.Join(repo, ".claude", "docs"), 0o755), notes); err != nil {
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
		{"edit of existing untracked root doc", "Edit", filepath.Join(repo, "NOTES.md"), false},
		{"rewrite of existing untracked root doc", "Write", filepath.Join(repo, "NOTES.md"), false},
		{"claude local memory", "Write", filepath.Join(repo, "CLAUDE.local.md"), false},
		{"codex override", "Write", filepath.Join(repo, "AGENTS.override.md"), false},
		{"anchor .markdown ext", "Write", filepath.Join(repo, "README.markdown"), false},
		{"anchor-prefixed name", "Write", filepath.Join(repo, "README.old.md"), true},
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
			code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, c.tool, c.path))), &stderr)
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
	if code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "NOTES.md")))), &stderr); code != core.ExitBlock {
		t.Fatalf("code = %d, want block", code)
	}
	for _, want := range []string{
		filepath.Join(global, "plans"),
		"handoffs in ",
		"reports in ",
		"HARNESS_HOOK_SKIP=docs-placement",
		"`touch <file> && git add -N <file>`",
		"rules/harness/docs-placement.md",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("block message missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestDocsPlacementThroughSymlink: a symlinked root doc blocks like the physical one; a subdir under that name passes.
func TestDocsPlacementThroughSymlink(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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
			code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Write", c.path))), &stderr)
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

// isTracked passes the basename as a PATHSPEC: a glob-shaped name would match another tracked file and pass the write.
func TestDocsPlacementGlobShapedNameStillBlocks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)

	var stderr bytes.Buffer
	if code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "*.md")))), &stderr); code != core.ExitBlock {
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

// TestDocsPlacementIgnoredDocsDir: <repo>/docs is gated only where git REFUSES to keep the page.
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
		{"root anchor name inside it", filepath.Join(ignored, "docs", "README.md"), true},
		{"non-docs subdir", filepath.Join(ignored, "src", "notes.md"), false},
		{"docs dir the repo owns", filepath.Join(owned, "docs", "design.md"), false},
		{"nested page the repo owns", filepath.Join(owned, "docs", "adr", "0001.md"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Write", c.path))), &stderr)
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
		code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Edit", filepath.Join(repo, c.path)))), &stderr)
		if code != c.want {
			t.Errorf("%s: code = %d, want %d (stderr: %s)", c.path, code, c.want, stderr.String())
		}
	}
}

// TestDocsPlacementHarnessDocsTreeExempt: only docs/images and docs/templates pass; docs/templates-evil still blocks.
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
		if code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Write", p))), &stderr); code != want {
			t.Errorf("%s: code = %d, want %d (stderr: %s)", name, code, want, stderr.String())
		}
	}
}

// TestDocsPlacementGitFileRoot: a linked worktree's `.git` FILE must resolve its root; an unreadable repo fails open.
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
			if code := placeFiles(core.ParseInput(strings.NewReader(placementPayload(t, "Write", c.path))), &stderr); code != c.want {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, c.want, stderr.String())
			}
		})
	}
}

// TestDocsPlacementPlanHomeUnderMemoryRepo: a root .md in memory/<repo> names the untagged harness plans dir.
func TestDocsPlacementPlanHomeUnderMemoryRepo(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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
	in := core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "NOTES.md"))))
	if code := placeFiles(in, &stderr); code != core.ExitBlock {
		t.Fatalf("code = %d, want block", code)
	}
	if want := "Plans belong in " + filepath.Join(cfg, "plans") + ","; !strings.Contains(stderr.String(), want) {
		t.Errorf("plan line missing %q:\n%s", want, stderr.String())
	}
	if want := "handoffs in " + filepath.Join(cfg, "memory", "handoffs", "YYYY-MM-DD-<topic>.md") + ";"; !strings.Contains(stderr.String(), want) {
		t.Errorf("handoff line must name the store-level handoffs dir; missing %q:\n%s", want, stderr.String())
	}
	for _, sub := range []string{"handoffs", "reports"} {
		var e bytes.Buffer
		in := core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, sub, "2026-11-07-x.md"))))
		if code := placeFiles(in, &e); code != core.ExitProceed {
			t.Errorf("%s inside the store = %d, want proceed (%s)", sub, code, e.String())
		}
	}
	store := filepath.Join(cfg, "memory")
	if out, err := exec.Command("git", "-C", store, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	link := filepath.Join(cfg, "projects", "-x", "memory")
	if err := errors.Join(os.MkdirAll(filepath.Dir(link), 0o755), os.Symlink(store, link)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, dir := range []string{store, link} {
		var e bytes.Buffer
		in := core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(dir, "a-new-note.md"))))
		if code := placeFiles(in, &e); code != core.ExitProceed {
			t.Errorf("note at store root via %s = %d, want proceed (%s)", dir, code, e.String())
		}
	}
}

// TestMain stubs the live-process scan so no docs test reads the real machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	code := m.Run()
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}

func TestDocsPlacementBlocksShellWrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)
	for _, c := range []struct {
		cmd  string
		code int
	}{
		{"cat > NOTES.md <<'EOF'\nhi\nEOF", core.ExitBlock},
		{"echo hi > NOTES.md", core.ExitBlock},
		{"sed -i s/a/b/ TODO.md", core.ExitBlock},
		{"echo hi > README.md", core.ExitProceed},
		{"cat NOTES.md > /tmp/out.txt", core.ExitProceed},
	} {
		b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": repo, "tool_input": map[string]string{"command": c.cmd}})
		var stderr bytes.Buffer
		if got := placeFiles(core.ParseInput(bytes.NewReader(b)), &stderr); got != c.code {
			t.Errorf("%q: code %d, want %d (%s)", c.cmd, got, c.code, &stderr)
		}
	}
}

// A first plan-named root write is rewritten, not placement-denied; a non-plan root markdown stays denied.
func TestDocsPlacementYieldsToPlanRewrite(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := placementRepo(t)
	for name, rewrite := range map[string]bool{"my-plan.md": true, "notes.md": false} {
		b, _ := json.Marshal(map[string]any{"tool_name": "Write", "cwd": repo,
			"tool_input": map[string]string{"file_path": filepath.Join(repo, name), "content": "x"}})
		if r := Placement(core.ParseInput(bytes.NewReader(b))); (r.UpdatedInput != nil) != rewrite || (r.Decision == core.Block) == rewrite {
			t.Errorf("%s: result = %+v, want rewrite=%v", name, r, rewrite)
		}
	}
}

// A block whose name picks a kind (plan, report, handoff) names the one free dated path instead of three templates.
func TestDocsPlacementNamesExactDest(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := placementRepo(t)
	day := time.Now().Format("2006-01-02")
	planHome := plans.Home(planRepoRoot(repo))
	if err := errors.Join(os.MkdirAll(planHome, 0o700), os.WriteFile(filepath.Join(planHome, day+"-taken-plan.md"), nil, 0o600)); err != nil {
		t.Fatal(err)
	}
	patch := func(key, path string) map[string]any {
		return map[string]any{key: "*** Begin Patch\n*** Add File: " + path + "\n+hi\n*** End Patch"}
	}
	for _, c := range []struct {
		name, tool string
		input      map[string]any
		want       string
	}{
		{"bash plan", "Bash", map[string]any{"command": "echo x > migration-plan.md"}, filepath.Join(planHome, day+"-migration-plan.md")},
		{"codex patch plan", "apply_patch", patch("command", "rollout-plan.md"), filepath.Join(planHome, day+"-rollout-plan.md")},
		{"taken plan name", "Bash", map[string]any{"command": "echo x > taken-plan.md"}, filepath.Join(planHome, day+"-taken-plan-2.md")},
		{"report", "Write", map[string]any{"file_path": filepath.Join(repo, "audit-report.md")}, paths.MemorySub("reports", day+"-audit-report.md")},
		{"findings", "Bash", map[string]any{"command": "echo x | tee review-findings.md"}, paths.MemorySub("reports", day+"-review-findings.md")},
		{"handoff", "apply_patch", patch("patchText", "session-handoff.md"), paths.MemorySub("handoffs", day+"-session-handoff.md")},
		{"no kind word", "Write", map[string]any{"file_path": filepath.Join(repo, "NOTES.md")}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // each case forks git three times, so the cases overlap
			b, _ := json.Marshal(map[string]any{"tool_name": c.tool, "cwd": repo, "tool_input": c.input})
			var stderr bytes.Buffer
			if code := placeFiles(core.ParseInput(bytes.NewReader(b)), &stderr); code != core.ExitBlock {
				t.Fatalf("code = %d, want block (%s)", code, &stderr)
			}
			exact, generic := strings.Contains(stderr.String(), "Re-issue the write to "+c.want+"."), strings.Contains(stderr.String(), "Plans belong in ")
			if c.want != "" && (!exact || generic) || c.want == "" && !generic {
				t.Errorf("want exact dest %q (generic homes only when empty), got:\n%s", c.want, &stderr)
			}
		})
	}
}

func TestDocsPlacementFeedbackRedirect(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	store := filepath.Join(cfg, "memory")
	link := filepath.Join(cfg, "projects", "-x", "memory")
	if err := errors.Join(os.MkdirAll(filepath.Join(store, "feedback"), 0o755), os.MkdirAll(filepath.Dir(link), 0o755), os.Symlink(store, link)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, c := range []struct{ tool, path, want string }{
		{"Write", filepath.Join(store, "feedback-x.md"), filepath.Join(store, "feedback", "feedback-x.md")},
		{"Write", filepath.Join(link, "feedback-y.md"), filepath.Join(store, "feedback", "feedback-y.md")},
		{"Read", filepath.Join(store, "feedback-r.md"), filepath.Join(store, "feedback", "feedback-r.md")},
		{"Edit", filepath.Join(store, "feedback", "feedback-z.md"), ""},
		{"Write", filepath.Join(store, "progress.md"), ""},
	} {
		r := Placement(core.ParseInput(strings.NewReader(placementPayload(t, c.tool, c.path))))
		got, _ := r.UpdatedInput["file_path"].(string)
		if got != c.want || r.Decision == core.Block {
			t.Errorf("%s %s: updated %q block=%v, want %q", c.tool, c.path, got, r.Decision == core.Block, c.want)
		}
	}
}

func TestStoreNameRedirect(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	store := paths.MemoryStoreDir()
	if err := os.MkdirAll(filepath.Join(store, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "reports", "old.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(store, "reports", day+"-dup.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path, want string }{
		{"dated destination exists", "reports/dup.md", "reports/" + day + "-dup-2.md"},
		{"undated new report", "reports/foo.md", "reports/" + day + "-foo.md"},
		{"key prefix stripped", "handoffs/claude-code--2026-11-16-x.md", "handoffs/2026-11-16-x.md"},
		{"compact date", "worktrees/20261116-x.md", "worktrees/2026-11-16-x.md"},
		{"already dated", "reports/2026-11-16-x.md", ""},
		{"existing undated file", "reports/old.md", ""},
		{"knowledge untouched", "knowledge/foo.md", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Placement(core.ParseInput(strings.NewReader(placementPayload(t, "Write", filepath.Join(store, c.path)))))
			got := ""
			if r.UpdatedInput != nil {
				got, _ = r.UpdatedInput["file_path"].(string)
				got, _ = filepath.Rel(store, got)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func BenchmarkPlacementWrite(b *testing.B) {
	cfg := b.TempDir()
	b.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := b.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		b.Fatalf("git init: %v (%s)", err, out)
	}
	for name, path := range map[string]string{
		"anchor":  filepath.Join(repo, "README.md"),
		"subdir":  filepath.Join(repo, "pkg", "a.md"),
		"blocked": filepath.Join(repo, "NOTES.md"),
		"report":  filepath.Join(repo, "REPORT-findings.md"),
	} {
		payload := fmt.Sprintf(`{"tool_name":"Write","cwd":%q,"tool_input":{"file_path":%q}}`, repo, path)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Placement(core.ParseInput(strings.NewReader(payload)))
				placeFiles(core.ParseInput(strings.NewReader(payload)), io.Discard)
			}
		})
	}
}

func TestPlacementPlanAgentWritesOnlyPlans(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	cases := []struct {
		name, agent, path string
		block             bool
	}{
		{"plan to plans home", "Plan", filepath.Join(cfg, "plans", "2026-11-22-x.md"), false},
		{"plan to repo subdir of plans", "Plan", filepath.Join(cfg, "plans", "repo", "2026-11-22-x.md"), false},
		{"plan to source file", "Plan", filepath.Join(cfg, "internal", "foo.go"), true},
		{"plan dotdot escape", "Plan", filepath.Join(cfg, "plans", "..", "internal", "foo.go"), true},
		{"plan to a sibling dir", "Plan", filepath.Join(cfg, "plans-x", "a.md"), true},
		{"main session to source file", "", filepath.Join(cfg, "internal", "foo.go"), false},
		{"other agent to source file", "claude", filepath.Join(cfg, "internal", "foo.go"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"tool_name": "Write", "agent_type": c.agent, "tool_input": map[string]string{"file_path": c.path}})
			r := planAgentBlock(core.ParseInput(bytes.NewReader(b)))
			if r.Decision == core.Block != c.block {
				t.Fatalf("decision = %v, block want %v", r.Decision, c.block)
			}
		})
	}
}
