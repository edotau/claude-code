package project

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The copilot pass manages ~/.copilot/skills: links added, stale re-pointed, orphans pruned; real dirs, foreign
// symlinks and the `synced` link survive.
func TestCopilotSkillsDirOwnership(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	writeSource(t, claude, "skills", "new-dir", "fresh")
	writeSource(t, claude, "skills", "moved-new", "moved")
	dir := CopilotSkillsDir(home)
	projectWriteFile(t, filepath.Join(dir, "impeccable", "SKILL.md"), "real dir\n")
	foreign := filepath.Join(home, "vendor", "foreign")
	projectWriteFile(t, filepath.Join(foreign, "SKILL.md"), "vendor\n")
	for link, target := range map[string]string{
		"foreign": foreign,
		"synced":  filepath.Join(home, "elsewhere", "synced"),
		"moved":   filepath.Join(claude, "skills", "moved-old"),
		"gone":    filepath.Join(claude, "skills", "gone"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
	}

	var out bytes.Buffer
	if code := Sync(Options{Agent: "copilot", Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	if dest, _ := os.Readlink(filepath.Join(dir, "fresh")); dest != filepath.Join(claude, "skills", "new-dir") {
		t.Errorf("fresh link -> %q, want new-dir", dest)
	}
	if dest, _ := os.Readlink(filepath.Join(dir, "moved")); dest != filepath.Join(claude, "skills", "moved-new") {
		t.Errorf("stale link not re-pointed: %q", dest)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
		t.Error("orphaned managed link not pruned")
	}
	if fi, err := os.Lstat(filepath.Join(dir, "impeccable")); err != nil || fi.IsDir() == false {
		t.Errorf("real dir impeccable disturbed: %v", err)
	}
	for name, want := range map[string]string{"foreign": foreign, "synced": filepath.Join(home, "elsewhere", "synced")} {
		if dest, _ := os.Readlink(filepath.Join(dir, name)); dest != want {
			t.Errorf("%s link changed to %q, want %q", name, dest, want)
		}
	}
	if code := Sync(Options{Agent: "copilot", Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check after sync = %d, want 0", code)
	}
}

// A managed-looking link into skills/synced is reported preserved and never pruned.
func TestPruneKeepsSyncedLink(t *testing.T) {
	agentsSkills := filepath.Join(t.TempDir(), "skills-dest")
	skillsDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(agentsSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(agentsSkills, "synced")
	if err := os.Symlink(filepath.Join(skillsDir, "synced"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	var out bytes.Buffer
	res := pruneSkillLinks(&out, agentsSkills, skillsDir, map[string]bool{}, false)
	if _, err := os.Lstat(link); err != nil || res.pruned != 0 {
		t.Errorf("synced link pruned (err %v, pruned %d)", err, res.pruned)
	}
	if !bytes.Contains(out.Bytes(), []byte("EXTRA")) {
		t.Errorf("no preserved-extra report: %q", out.String())
	}
}
