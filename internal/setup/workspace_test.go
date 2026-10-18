package setup

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wsRepo makes a git toplevel with one commit so GitToplevel answers.
func wsRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	for _, argv := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, argv...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	return root
}

// wsGlobal points CLAUDE_CONFIG_DIR at a fake harness with one agent, command and skill.
func wsGlobal(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	wsWrite(t, filepath.Join(cfg, "agents", "a.md"), "---\nname: a\n---\n")
	wsWrite(t, filepath.Join(cfg, "commands", "ns", "c.md"), "# c\n")
	wsWrite(t, filepath.Join(cfg, "skills", "s", "SKILL.md"), "# s\n")
	return cfg
}

func wsWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wsExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestWorkspaceInitCreatesLayoutAndIsIdempotent(t *testing.T) {
	wsGlobal(t)
	root := wsRepo(t)
	wsWrite(t, filepath.Join(root, ".claude", "settings.json"), `{"permissions":{"allow":["Bash(go test)"]}}`)
	var out bytes.Buffer
	if err := WorkspaceInit(WorkspaceInitOpts{Root: root, Out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"agents/.gitkeep", "commands/.gitkeep", "skills/.gitkeep"} {
		if !wsExists(filepath.Join(root, ".claude", p)) {
			t.Errorf("missing %s", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.json")); !strings.Contains(string(b), "go test") {
		t.Error("init overwrote an existing settings.json")
	}
	if !strings.Contains(out.String(), "keep    "+filepath.Join(root, ".claude", "settings.json")) {
		t.Errorf("settings.json not reported kept:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "hint    no CLAUDE.md") {
		t.Errorf("missing CLAUDE.md hint:\n%s", out.String())
	}
	// A second init appends the gitignore line no twice and creates nothing.
	out.Reset()
	if err := WorkspaceInit(WorkspaceInitOpts{Root: root, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "create") || strings.Contains(out.String(), "append") {
		t.Errorf("second init was not a no-op:\n%s", out.String())
	}
	ig, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if n := strings.Count(string(ig), workspaceIgnoreLine); n != 1 {
		t.Errorf(".gitignore has %d copies of the line, want 1:\n%s", n, ig)
	}
}

func TestWorkspaceInitDryRunWritesNothing(t *testing.T) {
	wsGlobal(t)
	root := wsRepo(t)
	var out bytes.Buffer
	if err := WorkspaceInit(WorkspaceInitOpts{Root: root, DryRun: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if wsExists(filepath.Join(root, ".claude")) || wsExists(filepath.Join(root, ".gitignore")) {
		t.Error("dry-run wrote files")
	}
	if !strings.Contains(out.String(), "create  "+filepath.Join(root, ".claude", "agents")) {
		t.Errorf("dry-run plan missing agents dir:\n%s", out.String())
	}
}

func TestWorkspaceInitWithSubsetAndBadPart(t *testing.T) {
	wsGlobal(t)
	root := wsRepo(t)
	var out bytes.Buffer
	if err := WorkspaceInit(WorkspaceInitOpts{Root: root, With: []string{"agents"}, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if wsExists(filepath.Join(root, ".claude", "settings.json")) || wsExists(filepath.Join(root, ".claude", "skills")) {
		t.Error("--with agents created more than agents/")
	}
	if err := WorkspaceInit(WorkspaceInitOpts{Root: root, With: []string{"hooks"}, Out: &out}); err == nil {
		t.Error("--with hooks accepted")
	}
}

func TestWorkspaceInitRefusals(t *testing.T) {
	cfg := wsGlobal(t)
	var out bytes.Buffer
	if err := WorkspaceInit(WorkspaceInitOpts{Root: t.TempDir(), Out: &out}); err == nil || !strings.Contains(err.Error(), "git toplevel") {
		t.Errorf("non-git root: err = %v", err)
	}
	inside := filepath.Join(cfg, "proj")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WorkspaceInit(WorkspaceInitOpts{Root: inside, Out: &out}); err == nil || !strings.Contains(err.Error(), "inside the harness") {
		t.Errorf("root inside config dir: err = %v", err)
	}
}

func TestWorkspaceLinkKindsAndRefusals(t *testing.T) {
	cfg := wsGlobal(t)
	root := wsRepo(t)
	var out bytes.Buffer
	cases := []struct{ kind, name, rel string }{
		{"agents", "a", "agents/a.md"}, {"commands", "ns:c", "commands/ns/c.md"}, {"skills", "s", "skills/s"},
	}
	for _, c := range cases {
		if err := WorkspaceLink(root, c.kind, c.name, &out); err != nil {
			t.Fatalf("link %s %s: %v", c.kind, c.name, err)
		}
		got, err := os.Readlink(filepath.Join(root, ".claude", c.rel))
		if err != nil || got != filepath.Join(cfg, c.rel) {
			t.Errorf("link %s → %q (%v), want %s", c.rel, got, err, filepath.Join(cfg, c.rel))
		}
	}
	if err := WorkspaceLink(root, "agents", "a", &out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate link: err = %v", err)
	}
	if err := WorkspaceLink(root, "agents", "nope", &out); err == nil || !strings.Contains(err.Error(), "no global agent") {
		t.Errorf("missing source: err = %v", err)
	}
	if err := WorkspaceLink(root, "commands", "c", &out); err == nil {
		t.Error("command without namespace accepted")
	}
	if err := WorkspaceLink(root, "agents", "../x", &out); err == nil {
		t.Error("traversal name accepted")
	}
}

func TestWorkspaceStatusClassifies(t *testing.T) {
	wsGlobal(t)
	root := wsRepo(t)
	var out bytes.Buffer
	if err := WorkspaceInit(WorkspaceInitOpts{Root: root, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if err := WorkspaceLink(root, "agents", "a", &out); err != nil {
		t.Fatal(err)
	}
	wsWrite(t, filepath.Join(root, ".claude", "commands", "ns", "c.md"), "# mine\n") // same name as global
	wsWrite(t, filepath.Join(root, ".claude", "skills", "only-here", "SKILL.md"), "# local\n")
	out.Reset()
	if err := WorkspaceStatus(root, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"agents    linked           a.md  →", "commands  shadows global   ns/c.md", "skills    local            only-here",
		"settings  present", "gitignore present",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q:\n%s", want, s)
		}
	}
}
