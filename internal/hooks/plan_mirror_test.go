package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dateStamp() string { return time.Now().Format("2006-01-02") }

const planTemplate = "# Plan: Widget Refactor\n\n## Context\n\nwhy\n\n## Phases\n\n### Phase 1 — do thing\n\n" +
	"**Files:** x\n\n## Tasks\n\n- [ ] Phase 1\n"

func writePayload(t *testing.T, tool, cwd, path, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": tool, "cwd": cwd,
		"tool_input": map[string]string{"file_path": path, "content": content},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPlanTemplateTeeth proves the gate actually fires: today (no gate consulted) a free-form first write to
// an undated draft passes; with PlanTemplate wired in, the same write is denied, and the message names Plan.md.
func TestPlanTemplateTeeth(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")

	var stderr bytes.Buffer
	code := PlanTemplate(strings.NewReader(writePayload(t, "Write", cfg, draft, "just some free-form draft text\n")), &stderr)
	if code != ExitBlock {
		t.Fatalf("free-form first write: PlanTemplate = %d, want ExitBlock", code)
	}
	if !strings.Contains(stderr.String(), "plan-template") || !strings.Contains(stderr.String(), "Plan.md") {
		t.Fatalf("denial message = %q, want it to name plan-template and Plan.md", &stderr)
	}
	if _, err := os.Stat(draft); err == nil {
		t.Fatalf("a denied write must not have landed on disk")
	}
}

func TestPlanTemplateAllowsTemplatedFirstWrite(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	code := PlanTemplate(strings.NewReader(writePayload(t, "Write", cfg, draft, planTemplate)), &bytes.Buffer{})
	if code != ExitProceed {
		t.Fatalf("templated first write: PlanTemplate = %d, want ExitProceed", code)
	}
}

// TestPlanTemplateOnlyGatesTheFirstWrite: once the draft exists on disk, a later Write to it (e.g. the
// model rewriting its own draft) is not re-gated, even with content missing the headings.
func TestPlanTemplateOnlyGatesTheFirstWrite(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	code := PlanTemplate(strings.NewReader(writePayload(t, "Write", cfg, draft, "no headings now\n")), &bytes.Buffer{})
	if code != ExitProceed {
		t.Fatalf("second write to an existing draft: PlanTemplate = %d, want ExitProceed", code)
	}
}

func TestPlanTemplateIgnoresDatedAndOutsidePlans(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, path := range []string{
		filepath.Join(cfg, "plans", "2026-10-20-already-dated.md"),
		filepath.Join(cfg, "plans", "sub", "funny-random-words.md"),
		filepath.Join(t.TempDir(), "elsewhere.md"),
	} {
		code := PlanTemplate(strings.NewReader(writePayload(t, "Write", cfg, path, "no headings\n")), &bytes.Buffer{})
		if code != ExitProceed {
			t.Errorf("PlanTemplate(%s) = %d, want ExitProceed (not an undated top-level draft)", path, code)
		}
	}
}

func mirrorPayload(t *testing.T, tool, cwd, path string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": tool, "cwd": cwd,
		"tool_input": map[string]string{"file_path": path},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlanMirrorTopLevelWhenCWDIsConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Write", cfg, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("PlanMirror = %d", code)
	}
	want := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")
	got, err := os.ReadFile(want)
	if err != nil || string(got) != planTemplate {
		t.Fatalf("mirrored file at %s: err=%v content=%q", want, err, got)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "plans-scratch", "funny-random-words.md.dest")); err != nil {
		t.Fatalf("expected a recorded dest: %v", err)
	}
}

func TestPlanMirrorRepoTaggedWhenCWDIsAnotherRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := gitRepo(t)
	draft := filepath.Join(cfg, "plans", "other-words.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Write", repo, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("PlanMirror = %d", code)
	}
	want := filepath.Join(cfg, "plans", filepath.Base(repo), dateStamp()+"-widget-refactor.md")
	if _, err := os.ReadFile(want); err != nil {
		t.Fatalf("mirrored repo-tagged file missing at %s: %v", want, err)
	}
}

// TestPlanMirrorEditReusesDestAndSurvivesH1Change: an Edit updates the same dated file, and renaming the
// H1 afterward still lands in that same file rather than forking a second one.
func TestPlanMirrorEditReusesDestAndSurvivesH1Change(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Write", cfg, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("first mirror = %d", code)
	}
	dest := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")

	edited := strings.Replace(planTemplate, "why\n", "why more\n", 1)
	if err := os.WriteFile(draft, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Edit", cfg, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("edit mirror = %d", code)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != edited {
		t.Fatalf("edit did not update %s: err=%v content=%q", dest, err, got)
	}

	renamed := strings.Replace(edited, "# Plan: Widget Refactor", "# Plan: Renamed Title", 1)
	if err := os.WriteFile(draft, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Edit", cfg, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("H1-change mirror = %d", code)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != renamed {
		t.Fatalf("H1 change must keep the same dest %s: err=%v content=%q", dest, err, got)
	}
	var count int
	entries, err := os.ReadDir(filepath.Join(cfg, "plans"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	if count != 2 { // the dated file + the still-live undated draft
		t.Fatalf("plans/ holds %d files, want exactly the dated file and the draft", count)
	}
}

// TestPlanMirrorNeverClobbersAnUnrelatedDatedFile: a dated file that already exists under the computed name
// (and isn't the one this mirror created) is left untouched; the mirror versions around it instead.
func TestPlanMirrorNeverClobbersAnUnrelatedDatedFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansDir := filepath.Join(cfg, "plans")
	if err := os.MkdirAll(plansDir, 0o700); err != nil {
		t.Fatal(err)
	}
	preexisting := filepath.Join(plansDir, dateStamp()+"-widget-refactor.md")
	unrelated := "# Plan: Widget Refactor\n\nsome other plan, not ours\n"
	if err := os.WriteFile(preexisting, []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(plansDir, "funny-random-words.md")
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Write", cfg, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("PlanMirror = %d", code)
	}
	if got, err := os.ReadFile(preexisting); err != nil || string(got) != unrelated {
		t.Fatalf("the unrelated dated file was touched: err=%v content=%q", err, got)
	}
	versioned := filepath.Join(plansDir, dateStamp()+"-widget-refactor-2.md")
	if got, err := os.ReadFile(versioned); err != nil || string(got) != planTemplate {
		t.Fatalf("mirror should have versioned to %s: err=%v content=%q", versioned, err, got)
	}
}

// TestSavePlanReusesMirroredDestAndCleansUpDraft: approval with EMPTY tool_input and tool_response{plan,
// filePath} reuses the dest plan-mirror already recorded, verifies the dated file, then removes the draft
// and its .dest record.
func TestSavePlanReusesMirroredDestAndCleansUpDraft(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := gitRepo(t)
	draft := filepath.Join(cfg, "plans", "other-words.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := PlanMirror(strings.NewReader(mirrorPayload(t, "Write", repo, draft)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("PlanMirror = %d", code)
	}
	dest := filepath.Join(cfg, "plans", filepath.Base(repo), dateStamp()+"-widget-refactor.md")
	if _, err := os.ReadFile(dest); err != nil {
		t.Fatalf("mirror did not create %s: %v", dest, err)
	}

	raw, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": repo,
		"tool_input": map[string]any{}, "tool_response": map[string]any{"plan": planTemplate, "filePath": draft},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := SavePlan(bytes.NewReader(raw), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("SavePlan = %d", code)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != planTemplate {
		t.Fatalf("final dest %s: err=%v content=%q", dest, err, got)
	}
	if _, err := os.Stat(draft); !os.IsNotExist(err) {
		t.Fatalf("the undated draft must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "plans-scratch", "other-words.md.dest")); !os.IsNotExist(err) {
		t.Fatalf("the .dest record must be removed, stat err = %v", err)
	}
}
