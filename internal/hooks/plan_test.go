package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// planPayload is the real PostToolUse shape: ExitPlanMode's tool_input carries plan + planFilePath.
func planPayload(t *testing.T, cwd, plan, planFilePath string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": cwd,
		"tool_input": map[string]string{"plan": plan, "planFilePath": planFilePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func savedPlans(t *testing.T, cfg string) []string {
	t.Helper()
	var names []string
	root := filepath.Join(cfg, "plans")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	return names
}

func TestSavePlanResponseFallbacksAndAudit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inputPlan    string
		responsePlan string
		responseFile bool
		missingFile  bool
		inputFile    bool
		want         string
	}{
		{"input wins", "# Input\n", "# Response\n", true, false, true, "# Input\n"},
		{"response-only empty input", "", "# Response\n", false, false, false, "# Response\n"},
		{"response plan", "", "# Response\n", true, false, true, "# Response\n"},
		{"response file", "", "", true, false, true, "# Response file\n"},
		{"input file", "", "", false, false, true, "# Input file\n"},
		{"missing response file", "", "", true, true, true, "# Input file\n"},
		{"empty", "", "", false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", cfg)
			input := map[string]any{}
			if tc.inputPlan != "" {
				input["plan"] = tc.inputPlan
			}
			response := map[string]any{"plan": tc.responsePlan}
			for _, f := range []struct {
				enabled bool
				missing bool
				fields  map[string]any
				key     string
				text    string
			}{
				{tc.responseFile, tc.missingFile, response, "filePath", "# Response file\n"},
				{tc.inputFile, false, input, "planFilePath", "# Input file\n"},
			} {
				if !f.enabled {
					continue
				}
				path := filepath.Join(t.TempDir(), "scratch.md")
				if !f.missing {
					if err := os.WriteFile(path, []byte(f.text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				f.fields[f.key] = path
			}
			raw, err := json.Marshal(map[string]any{
				"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": cfg,
				"tool_input": input, "tool_response": response,
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if code := SavePlan(bytes.NewReader(raw), &bytes.Buffer{}); code != ExitProceed {
					t.Fatalf("SavePlan = %d", code)
				}
			}
			names := savedPlans(t, cfg)
			events, err := audit.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(names) != 0 || len(events) != 0 {
					t.Fatalf("empty input saved plans %v or audit events %v", names, events)
				}
				return
			}
			if len(names) != 1 {
				t.Fatalf("saved plans = %v, want one", names)
			}
			dest := filepath.Join(cfg, "plans", filepath.FromSlash(names[0]))
			got, err := os.ReadFile(dest)
			if err != nil || !bytes.Equal(got, withTasks([]byte(tc.want))) {
				t.Fatalf("saved plan = %q, err = %v, want %q", got, err, tc.want)
			}
			if len(events) != 1 || string(events[0].Action) != "plan_saved" ||
				events[0].Resource != "save-plan" || events[0].Detail != dest {
				t.Fatalf("audit events = %+v, want one plan_saved with destination only", events)
			}
		})
	}
}

func TestSavePlanWriteFailureDoesNotAudit(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "plans"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := SavePlan(strings.NewReader(planPayload(t, cfg, "# Probe\n", "")), &stderr); code != ExitProceed {
		t.Fatalf("write failure must fail open, got %d", code)
	}
	if !strings.Contains(stderr.String(), "save-plan:") {
		t.Fatalf("write failure was not reported: %s", &stderr)
	}
	events, err := audit.Read(time.Time{})
	if err != nil || len(events) != 0 {
		t.Fatalf("failed save audit events = %v, err = %v", events, err)
	}
}

func TestSavePlanFilesTaggedOnceAndVersionsEdits(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(t.TempDir(), "My Repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	save := func(plan string) {
		if code := SavePlan(strings.NewReader(planPayload(t, repo, plan, "")), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("SavePlan must never block, got %d", code)
		}
	}
	plan := "# Plan: Port the Thing\n\nsteps\n"
	save(plan)
	save(plan) // a repeat approval of the same text is not a new version
	save(plan + "more\n")
	date := time.Now().Format("2006-01-02")
	want := []string{"my-repo/" + date + "-port-the-thing-2.md", "my-repo/" + date + "-port-the-thing.md"}
	if got := savedPlans(t, cfg); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("saved = %v, want %v", got, want)
	}
	got, _ := os.ReadFile(filepath.Join(cfg, "plans", filepath.FromSlash(want[1])))
	if !strings.HasPrefix(string(got), plan) || !strings.Contains(string(got), "\n## Tasks\n") {
		t.Errorf("saved plan must be the approved text followed by a Tasks checklist, got %q", got)
	}
}

// TestWithTasks: headings naming a phase/step become items, the closeout tail follows, and a plan that already
// carries a checklist is left exactly as approved.
func TestWithTasks(t *testing.T) {
	got := string(withTasks([]byte("# Plan: X\n\n### Phase 1 — parse\n\n## Step 2: render\n\n## Context\n")))
	for _, want := range []string{"- [ ] Phase 1 — parse\n", "- [ ] Step 2: render\n", "- [ ] Full suite once\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- [ ] Context") {
		t.Errorf("a non-step heading became a task:\n%s", got)
	}
	own := "# Plan: Y\n\n## Tasks\n\n- [x] done\n- [ ] next\n"
	if string(withTasks([]byte(own))) != own {
		t.Errorf("a plan with its own checklist must be unchanged")
	}
}

func TestSavePlanReadsPlanFilePathAndSkipsEmpty(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	src := filepath.Join(t.TempDir(), "scratch.md")
	if err := os.WriteFile(src, []byte("no heading here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	SavePlan(strings.NewReader(planPayload(t, cfg, "", src)), &bytes.Buffer{})
	SavePlan(strings.NewReader(planPayload(t, cfg, "  \n", "")), &bytes.Buffer{})
	want := time.Now().Format("2006-01-02") + "-plan.md" // config dir: untagged; no heading: "plan"
	if got := savedPlans(t, cfg); len(got) != 1 || got[0] != want {
		t.Errorf("saved = %v, want [%s]", got, want)
	}
}
