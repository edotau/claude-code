package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// placementRepo makes a temp git repo with one tracked root doc, so the gate has real git state.
func placementRepo(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
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

func TestDocsPlacementPlanTemplate(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		path     string
		content  string
		existing bool
		want     int
	}{
		{"freeform Write", "Write", "plans/draft.md", "freeform", false, ExitBlock},
		{"freeform create", "create", "plans/draft.md", "freeform", false, ExitBlock},
		{"legacy empty tool", "", "plans/draft.md", "freeform", false, ExitBlock},
		{"missing content", "Write", "plans/draft.md", "", false, ExitBlock},
		{"templated Write", "Write", "plans/draft.md", planTemplate, false, ExitProceed},
		{"existing draft", "Write", "plans/draft.md", "freeform", true, ExitProceed},
		{"Edit bypass", "Edit", "plans/draft.md", "freeform", false, ExitProceed},
		{"patch bypass", "apply_patch", "plans/draft.md", "freeform", false, ExitProceed},
		{"other tool", "Bash", "plans/draft.md", "freeform", false, ExitProceed},
		{"dated plan", "Write", "plans/2026-10-20-draft.md", "freeform", false, ExitProceed},
		{"nested plan", "Write", "plans/project/draft.md", "freeform", false, ExitProceed},
		{"outside plans", "Write", "other/draft.md", "freeform", false, ExitProceed},
	}
	for _, knob := range []string{"1", "0"} {
		for _, c := range cases {
			t.Run("docs="+knob+"/"+c.name, func(t *testing.T) {
				cfg := t.TempDir()
				t.Setenv("CLAUDE_CONFIG_DIR", cfg)
				t.Setenv(docsPlacementDisableEnv, knob)
				if c.existing {
					path := filepath.Join(cfg, c.path)
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				var stdout, stderr bytes.Buffer
				code := Run("docs-placement", strings.NewReader(writePayload(t, c.tool, cfg, c.path, c.content)), &stdout, &stderr)
				if code != c.want {
					t.Fatalf("code = %d, want %d; stderr=%s", code, c.want, &stderr)
				}
				if stdout.Len() != 0 {
					t.Errorf("unexpected stdout: %s", &stdout)
				}
				if c.want == ExitBlock {
					if !strings.HasPrefix(stderr.String(), "plan-template: BLOCKED") ||
						!strings.Contains(stderr.String(), `missing "# Plan:"`) ||
						!strings.Contains(stderr.String(), filepath.Join(cfg, "agents", "Plan.md")) {
						t.Errorf("missing first heading/template diagnostic: %s", &stderr)
					}
					if _, err := os.Stat(filepath.Join(cfg, c.path)); !os.IsNotExist(err) {
						t.Errorf("denied draft must remain absent: %v", err)
					}
				} else if stderr.Len() != 0 {
					t.Errorf("allowed write emitted a diagnostic: %s", &stderr)
				}
			})
		}
	}
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
		want string
		not  string
	}{
		{"1", "docs-placement: BLOCKED", "plan-template: BLOCKED"},
		{"0", "plan-template: BLOCKED", "docs-placement: BLOCKED"},
	} {
		t.Run("docs="+c.knob, func(t *testing.T) {
			t.Setenv(docsPlacementDisableEnv, c.knob)
			var stdout, stderr bytes.Buffer
			payload := writePayload(t, "Write", cfg, filepath.Join(plans, "draft.md"), "freeform")
			if code := Run("docs-placement", strings.NewReader(payload), &stdout, &stderr); code != ExitBlock {
				t.Fatalf("code = %d, want ExitBlock; stderr=%s", code, &stderr)
			}
			if !strings.HasPrefix(stderr.String(), c.want) || strings.Contains(stderr.String(), c.not) {
				t.Errorf("want only %s denial, got %s", c.want, &stderr)
			}
		})
	}
}

func TestPlanTemplateCommandRetired(t *testing.T) {
	if _, ok := Registry["plan-template"]; ok {
		t.Fatal("retired plan-template command remains registered")
	}
	var stdout, stderr bytes.Buffer
	if code := Run("plan-template", strings.NewReader("{}"), &stdout, &stderr); code != 1 {
		t.Fatalf("retired command returned %d, want 1", code)
	}
	if stdout.Len() != 0 || stderr.String() != "claude-code hook: unknown hook \"plan-template\"\n" {
		t.Fatalf("stdout=%q stderr=%q", &stdout, &stderr)
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
					if code := DocsPlacement(bytes.NewReader(raw), &stderr); code != ExitBlock {
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
			code := DocsPlacement(strings.NewReader(placementPayload(t, c.tool, c.path)), &stderr)
			want := ExitProceed
			if c.block {
				want = ExitBlock
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
	if code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "PLAN.md"))), &stderr); code != ExitBlock {
		t.Fatalf("code = %d, want block", code)
	}
	for _, want := range []string{
		filepath.Join(global, "plans"),
		string(filepath.Separator) + "handoffs" + string(filepath.Separator),
		string(filepath.Separator) + "reports" + string(filepath.Separator),
		filepath.Join(global, "docs", "knowledge"),
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
	if code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "PLAN.md"))), &stderr); code != ExitProceed {
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
			code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", c.path)), &stderr)
			want := ExitProceed
			if c.block {
				want = ExitBlock
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
	if code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", filepath.Join(repo, "*.md"))), &stderr); code != ExitBlock {
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
			code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", c.path)), &stderr)
			want := ExitProceed
			if c.block {
				want = ExitBlock
			}
			if code != want {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, want, stderr.String())
			}
		})
	}
}

// TestDocsPlacementHarnessDocsTreeExempt: the harness docs tree admits only knowledge/ and images/; the retired
// handoffs/reports homes block even though this checkout no longer gitignores them.
func TestDocsPlacementHarnessDocsTreeExempt(t *testing.T) {
	harness := ignoredDocsRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", harness)

	for name, want := range map[string]int{
		"knowledge/topic.md": ExitProceed, "knowledge/repo/x.md": ExitProceed, "images/n.md": ExitProceed,
		"handoffs/HANDOFF.md": ExitBlock, "reports/repo/y.md": ExitBlock, "plans/2026-09-09-x.md": ExitBlock,
	} {
		var stderr bytes.Buffer
		p := filepath.Join(harness, "docs", filepath.FromSlash(name))
		if code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", p)), &stderr); code != want {
			t.Errorf("%s: code = %d, want %d (stderr: %s)", name, code, want, stderr.String())
		}
	}
}
