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
		filepath.Join(global, "docs", "handoffs"),
		filepath.Join(global, "docs", "reports"),
		"HARNESS_DOCS_PLACEMENT=0",
		"rules/standards/documentation.md",
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

// TestDocsPlacementHarnessDocsTreeExempt: the canonical destination is itself gitignored inside this
// checkout, so the gate must never fire on it — every plan/handoff/report write would break.
func TestDocsPlacementHarnessDocsTreeExempt(t *testing.T) {
	harness := ignoredDocsRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", harness)

	for _, name := range []string{"plans/2026-09-09-x.md", "handoffs/HANDOFF.md", "reports/repo/y.md", "memory/bank/z.md"} {
		var stderr bytes.Buffer
		p := filepath.Join(harness, "docs", filepath.FromSlash(name))
		if code := DocsPlacement(strings.NewReader(placementPayload(t, "Write", p)), &stderr); code != ExitProceed {
			t.Errorf("%s: code = %d, want proceed inside the harness docs tree (stderr: %s)", name, code, stderr.String())
		}
	}
}
