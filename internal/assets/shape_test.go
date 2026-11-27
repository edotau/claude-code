package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSkillPassesItsShape(t *testing.T) {
	sk, err := RenderSkill(SkillInput{Name: "made-up", Description: `Does a made-up thing. Use for "do it".`,
		Triggers: []string{"do it", "try it", "try it"}, NonTriggers: []string{"other task"},
		TaskCases: []string{"do it and verify", "try it and report"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sk.Entry.Body, `Use for "do it", "try it".`) || strings.Count(sk.Entry.Body, "Use for") != 1 {
		t.Fatalf("description not folded: %s", sk.Entry.Body)
	}
	checkSkillEvals(t, sk)
	if _, err := RenderSkill(SkillInput{Name: "made-up", Description: strings.Repeat("a", 1010), Triggers: []string{"too long"}}); err == nil {
		t.Fatal("final description length was not checked")
	}
	shape, _ := ShapeByID("skill")
	if fs := shape.Validate("/c/"+sk.Entry.Path, []byte(sk.Entry.Body)); len(fs) != 0 {
		t.Fatalf("scaffold has findings: %v", fs)
	}
	cut, _, _ := strings.Cut(sk.Entry.Body, "## Gotchas")
	if fs := shape.Validate("/c/"+sk.Entry.Path, []byte(cut)); len(fs) != 1 || fs[0].Rule != "sec-required" {
		t.Fatalf("a skill without Gotchas gave %v, want one sec-required", fs)
	}
}

func checkSkillEvals(t *testing.T, sk SkillScaffold) {
	t.Helper()
	root := t.TempDir()
	entry := filepath.Join(root, sk.Entry.Path)
	if err := os.MkdirAll(filepath.Dir(entry), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sk.Evals.Path), []byte(sk.Evals.Body), 0644); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- do it", "- other task", "- do it and verify", "- try it and report", "## Fresh-agent check"} {
		if !strings.Contains(sk.Evals.Body, want) {
			t.Errorf("evals missing %q", want)
		}
	}
	shape, _ := ShapeByID("skill")
	if fs := shape.Validate(entry, []byte(sk.Entry.Body)); len(fs) != 0 {
		t.Fatalf("eval findings: %v", fs)
	}
	bad := strings.Replace(sk.Evals.Body, "- other task\n", "", 1)
	if err := os.WriteFile(filepath.Join(root, sk.Evals.Path), []byte(bad), 0644); err != nil {
		t.Fatal(err)
	}
	if fs := shape.Validate(entry, []byte(sk.Entry.Body)); len(fs) != 1 || fs[0].Rule != "sec-items" {
		t.Fatalf("empty non-trigger section: %v", fs)
	}
}
