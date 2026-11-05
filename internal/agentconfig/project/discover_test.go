package project

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSkill(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillNames(t *testing.T, skillsDir string) []string {
	t.Helper()
	got, err := DiscoverSkills(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name())
	}
	return names
}

func TestDiscoverSkillsFollowsInsideSymlinkExcludesSynced(t *testing.T) {
	skills := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, filepath.Join(skills, "real"), "real")
	writeSkill(t, filepath.Join(skills, "vendor", "deep"), "deep")
	writeSkill(t, filepath.Join(skills, "synced", "acct"), "acct")
	writeSkill(t, filepath.Join(skills, "synced"), "synced")
	if err := os.Symlink(filepath.Join(skills, "vendor", "deep"), filepath.Join(skills, "linked")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(skills, "synced", "acct"), filepath.Join(skills, "viasynced")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(skills, "synced"), filepath.Join(skills, "viasyncedroot")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(skills, "outside")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(skills, "outside"), "outside")
	got := skillNames(t, skills)
	want := []string{"deep", "real"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		// "linked" resolves to the deep dir; its frontmatter name is deep, vendor has no SKILL.md.
		t.Errorf("DiscoverSkills = %v, want %v", got, want)
	}
}

func TestDiscoverSkillsDuplicateRejected(t *testing.T) {
	skills := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, filepath.Join(skills, "a"), "same")
	writeSkill(t, filepath.Join(skills, "b"), "same")
	got, err := DiscoverSkills(skills)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateUniqueNames(got); err == nil {
		t.Error("validateUniqueNames accepted duplicate logical names")
	}
}
