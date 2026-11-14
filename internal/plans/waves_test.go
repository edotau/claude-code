package plans

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wavesFile(t *testing.T, raw string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "plan")
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func launchedCount(t *testing.T, w Wave) int {
	t.Helper()
	launched := 0
	for _, task := range w.Tasks {
		if strings.HasSuffix(task.ID, ".V") {
			if !task.Manual {
				t.Fatal("verifier launched")
			}
			continue
		}
		if !task.Manual {
			launched++
		}
	}
	return launched
}

func TestWavesFixture(t *testing.T) {
	p, err := ParseWaves("testdata/waves_plan.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Waves) != 4 || len(p.Manual) != 1 {
		t.Fatalf("waves=%d baseline=%d", len(p.Waves), len(p.Manual))
	}
	if launched := launchedCount(t, p.Waves[1]); launched != 17 {
		t.Fatalf("wave 2 launched %d", launched)
	}
	if p.Waves[1].Verify == "" || !strings.Contains(p.Waves[0].Gate, "go build") {
		t.Fatal("lost table proof or gate")
	}
}

func TestWavesRefusals(t *testing.T) {
	first := "- [ ] Phase 1.1 — leaf: codex, Agent: codex — owns: internal/a.go\n"
	cases := []struct{ name, raw, id string }{
		{"duplicate", first + first, "Phase 1.1"},
		{"overlap", first + "- [ ] Phase 1.2 — leaf: codex, Agent: codex — owns: internal/a.go", "Phase 1.2"},
		{"directory", strings.ReplaceAll(first, "internal/a.go", "internal/") + "- [ ] Phase 1.2 — leaf: codex, Agent: codex — owns: internal/a.go", "Phase 1.2"},
		{"missing owns", "- [ ] Phase 1.3 — leaf: codex, Agent: codex", "Phase 1.3"},
		{"unknown", strings.Replace(first, "Agent: codex", "Agent: bogus", 1), "Phase 1.1"},
		{"table", first + "\n## Waves\n| Wave | Packages | Verify |\n| 1 | 1.2 | proof |", "Phase 1.1"},
		{"gate", first + "\n## Phases\n```bash\ngit commit -m bad\n```", "wave 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseWaves(wavesFile(t, c.raw))
			var refusal *WaveError
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), c.id) {
				t.Fatalf("want typed refusal for %s, got %v", c.id, err)
			}
		})
	}
}

func TestWavesMetadataAndManifest(t *testing.T) {
	checkPlanMetadata(t)
	checkManifest(t)
}

func checkPlanMetadata(t *testing.T) {
	t.Helper()
	raw := "## Phases\n- **1.1 Work.** Make the feature.\n  Keep the detail.\n\n## Tasks\n" +
		"- [ ] Phase 1.1 — leaf: codex, Agent: codex — verify: go test ./internal/plans — findings: findings.json#one — owns: internal/a.go\n" +
		"- [ ] Phase 1.V — leaf: claude, Agent: claude\n"
	p, err := ParseWaves(wavesFile(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	task := p.Waves[0].Tasks[0]
	if task.Findings != "findings.json#one" || task.Verify != "go test ./internal/plans" || !strings.Contains(task.Task, "Keep the detail.") {
		t.Fatalf("metadata lost: %+v", task)
	}
	if !p.Waves[0].Tasks[1].Manual {
		t.Fatal("V with launch agent must remain manual")
	}
}

func checkManifest(t *testing.T) {
	t.Helper()
	manifest := `{"id":"1.1","wave":1,"agent":"codex","profile":"qa","owns":["internal/a.go"],"task":"Do work","verify":"test -f internal/a.go"}` + "\n" +
		`{"id":"1.V","wave":1,"agent":"in-context"}` + "\n" + `{"wave":1,"gate":"go vet ./internal/plans"}`
	got, err := ParseManifest(wavesFile(t, manifest))
	if err != nil {
		t.Fatal(err)
	}
	if got.Waves[0].Tasks[0].Profile != "qa" || !got.Waves[0].Tasks[1].Manual || got.Waves[0].Gate != "go vet ./internal/plans" {
		t.Fatalf("manifest: %+v", got)
	}
	_, err = ParseManifest(wavesFile(t, strings.Replace(manifest, "codex", "bogus", 1)))
	if err == nil || !strings.Contains(err.Error(), "Phase 1.1") {
		t.Fatalf("manifest refusal: %v", err)
	}
}

func TestWavesGateRefused(t *testing.T) {
	for _, verb := range []string{"add", "commit", "stash", "reset", "checkout", "restore", "clean", "push", "rebase", "merge"} {
		if GateRefused("go vet ./internal/plans && git "+verb+" x") == nil {
			t.Errorf("allowed %s", verb)
		}
	}
	if err := GateRefused("git diff --check && go vet ./internal/plans"); err != nil {
		t.Fatal(err)
	}
}

func TestWavesGateRefusedForms(t *testing.T) {
	for _, cmd := range []string{"git -C . commit -m x", "git -c a=b commit", "git --no-pager stash", "git --git-dir x rm f",
		"git --git-dir=.git tag v1", `git -C "my dir" push`, "git cherry-pick abc", "git rm a.go", "git am p.patch",
		"git apply --index p.diff", "git tag v1", "git switch main", "go vet ./... && git -C /r -c k=v reset --hard"} {
		if GateRefused(cmd) == nil {
			t.Errorf("%q must be refused", cmd)
		}
	}
	for _, cmd := range []string{"git -C . diff --stat", "git apply --check p.diff", "git log --grep commit", "git diff -- rm.go",
		"git status --short", "go test ./internal/plans"} {
		if err := GateRefused(cmd); err != nil {
			t.Errorf("%q refused: %v", cmd, err)
		}
	}
}

func TestWavesTaskVerifyAndOwnsRefused(t *testing.T) {
	task := func(attrs, owns string) string {
		return "- [ ] Phase 1.1 — leaf: codex, Agent: codex" + attrs + " — owns: " + owns + "\n"
	}
	cases := []struct{ name, raw, want string }{
		{"verify git -C", task(" — verify: git -C . commit -m x", "internal/a.go"), "verify:"},
		{"verify git rm", task(" — verify: go vet ./... && git rm a.go", "internal/a.go"), "verify:"},
		{"star", task("", "*"), `owns: "*"`},
		{"dot slash", task("", "./"), "repo root"},
		{"parent", task("", "../x"), "leaves the repo"},
		{"dotdot out", task("", "a/../../x"), "leaves the repo"},
		{"absolute", task("", "/etc/passwd"), "absolute"},
		{"collapses to root", task("", "a/.."), "repo root"},
		{"double star", task("", "**"), "everything"},
		{"bad glob", task("", "internal/[a"), "bad glob"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseWaves(wavesFile(t, c.raw))
			var refusal *WaveError
			if !errors.As(err, &refusal) || refusal.ID != "Phase 1.1" || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want Phase 1.1 refusal containing %q, got %v", c.want, err)
			}
		})
	}
	manifest := `{"id":"1.1","wave":1,"agent":"codex","owns":["internal/a.go",""],"task":"x"}`
	if _, err := ParseManifest(wavesFile(t, manifest)); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty owns entry: %v", err)
	}
	for _, ok := range []string{"internal/plans", "internal/plans/", "internal/*.go", "./internal/a.go"} {
		if _, err := ParseWaves(wavesFile(t, task(" — verify: git diff --check", ok))); err != nil {
			t.Errorf("owns %q refused: %v", ok, err)
		}
	}
}
