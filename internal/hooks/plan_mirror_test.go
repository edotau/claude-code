package hooks

import (
	"bytes"
	"encoding/json"
	"io"
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
	code := checkPlanTemplate(ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, "just some free-form draft text\n"))), &stderr)
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
	code := checkPlanTemplate(ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, planTemplate))), &bytes.Buffer{})
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
	code := checkPlanTemplate(ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, "no headings now\n"))), &bytes.Buffer{})
	if code != ExitProceed {
		t.Fatalf("second write to an existing draft: PlanTemplate = %d, want ExitProceed", code)
	}
}

// TestPlanTemplateAllFourHeadingsPresent: the exact template content passes with all 4 headings as plain text.
func TestPlanTemplateAllFourHeadingsPresent(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "all-headings.md")
	content := "# Plan: Z\n\n## Context\n\nwhy\n\n## Phases\n\n### Phase 1\n\n## Tasks\n\n- [ ] Phase 1\n"
	code := checkPlanTemplate(ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, content))), &bytes.Buffer{})
	if code != ExitProceed {
		t.Fatalf("all 4 headings present: checkPlanTemplate = %d, want ExitProceed", code)
	}
}

// TestPlanTemplateRejectsFencedOnlyHeading: checkPlanTemplate now parses the mdsmith AST, so a heading that
// only appears inside a fenced code block does not count as present (fixed from a strings.Contains scan,
// which previously let this through as ExitProceed).
func TestPlanTemplateRejectsFencedOnlyHeading(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "fenced-heading.md")
	// "## Tasks" only appears inside the fence below, never as a real heading.
	content := "# Plan: Z\n\n## Context\n\nwhy\n\n## Phases\n\n```\n## Tasks\n```\n"
	var stderr bytes.Buffer
	code := checkPlanTemplate(ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, content))), &stderr)
	if code != ExitBlock {
		t.Fatalf("fenced-only heading = %d, want ExitBlock (fence content must not satisfy the template check)", code)
	}
	if !strings.Contains(stderr.String(), "## Tasks") {
		t.Fatalf("denial message = %q, want it to name the missing heading", &stderr)
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
		code := checkPlanTemplate(ParseInput(strings.NewReader(writePayload(t, "Write", cfg, path, "no headings\n"))), &bytes.Buffer{})
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
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", cfg, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("planMirror = %d", code)
	}
	want := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")
	got, err := os.ReadFile(want)
	if err != nil || string(got) != planTemplate {
		t.Fatalf("mirrored file at %s: err=%v content=%q", want, err, got)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "harness", "plans-scratch", "funny-random-words.md.dest")); err != nil {
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
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", repo, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("planMirror = %d", code)
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
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", cfg, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("first mirror = %d", code)
	}
	dest := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")

	edited := strings.Replace(planTemplate, "why\n", "why more\n", 1)
	if err := os.WriteFile(draft, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Edit", cfg, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("edit mirror = %d", code)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != edited {
		t.Fatalf("edit did not update %s: err=%v content=%q", dest, err, got)
	}

	renamed := strings.Replace(edited, "# Plan: Widget Refactor", "# Plan: Renamed Title", 1)
	if err := os.WriteFile(draft, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Edit", cfg, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
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
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", cfg, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("planMirror = %d", code)
	}
	if got, err := os.ReadFile(preexisting); err != nil || string(got) != unrelated {
		t.Fatalf("the unrelated dated file was touched: err=%v content=%q", err, got)
	}
	versioned := filepath.Join(plansDir, dateStamp()+"-widget-refactor-2.md")
	if got, err := os.ReadFile(versioned); err != nil || string(got) != planTemplate {
		t.Fatalf("mirror should have versioned to %s: err=%v content=%q", versioned, err, got)
	}
}

// TestPlanMirrorNeverAdoptsAnIdenticalDatedFile: two drafts share the same H1 and the same first-mirrored
// content on the same day. The second draft must never adopt the first's dated file as its own dest — even
// though the bytes are identical — or a later edit to draft B would silently overwrite draft A's file.
func TestPlanMirrorNeverAdoptsAnIdenticalDatedFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansDir := filepath.Join(cfg, "plans")
	if err := os.MkdirAll(plansDir, 0o700); err != nil {
		t.Fatal(err)
	}
	draftA := filepath.Join(plansDir, "aaa-words.md")
	draftB := filepath.Join(plansDir, "bbb-words.md")
	for _, d := range []string{draftA, draftB} {
		if err := os.WriteFile(d, []byte(planTemplate), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", cfg, draftA)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("mirror A = %d", code)
	}
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", cfg, draftB)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("mirror B = %d", code)
	}
	destA := filepath.Join(plansDir, dateStamp()+"-widget-refactor.md")
	destB := filepath.Join(plansDir, dateStamp()+"-widget-refactor-2.md")
	if destA == destB {
		t.Fatalf("drafts A and B must not share a dest")
	}
	if got, err := os.ReadFile(destA); err != nil || string(got) != planTemplate {
		t.Fatalf("destA %s: err=%v content=%q", destA, err, got)
	}
	if got, err := os.ReadFile(destB); err != nil || string(got) != planTemplate {
		t.Fatalf("destB %s: err=%v content=%q", destB, err, got)
	}

	edited := strings.Replace(planTemplate, "why\n", "draft B's own edit\n", 1)
	if err := os.WriteFile(draftB, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Edit", cfg, draftB)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("edit mirror B = %d", code)
	}
	if got, err := os.ReadFile(destA); err != nil || string(got) != planTemplate {
		t.Fatalf("editing draft B must never change destA %s: err=%v content=%q", destA, err, got)
	}
	if got, err := os.ReadFile(destB); err != nil || string(got) != edited {
		t.Fatalf("destB should hold draft B's edit %s: err=%v content=%q", destB, err, got)
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
	if code := runDirect("docs-plan-mirror", strings.NewReader(mirrorPayload(t, "Write", repo, draft)), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("planMirror = %d", code)
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
	if code := runDirect("docs-plan-save", bytes.NewReader(raw), io.Discard, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("savePlan = %d", code)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != planTemplate {
		t.Fatalf("final dest %s: err=%v content=%q", dest, err, got)
	}
	if _, err := os.Stat(draft); !os.IsNotExist(err) {
		t.Fatalf("the undated draft must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "harness", "plans-scratch", "other-words.md.dest")); !os.IsNotExist(err) {
		t.Fatalf("the .dest record must be removed, stat err = %v", err)
	}
}

// TestSavePlanKeepsDraftEditedAfterApproval: the draft on disk no longer matches the approved plan text
// (e.g. a concurrent agent kept editing it) — savePlan must file the approved plan but leave the draft alone.
func TestSavePlanKeepsDraftEditedAfterApproval(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(planTemplate, "why\n", "why, edited after approval\n", 1)
	if err := os.WriteFile(draft, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": cfg,
		"tool_input": map[string]any{}, "tool_response": map[string]any{"plan": planTemplate, "filePath": draft},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runDirect("docs-plan-save", bytes.NewReader(raw), io.Discard, &stderr); code != ExitProceed {
		t.Fatalf("savePlan = %d", code)
	}
	dest := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")
	if got, err := os.ReadFile(dest); err != nil || string(got) != planTemplate {
		t.Fatalf("approved plan not saved at %s: err=%v content=%q", dest, err, got)
	}
	if got, err := os.ReadFile(draft); err != nil || string(got) != edited {
		t.Fatalf("draft edited after approval must be kept untouched: err=%v content=%q", err, got)
	}
	if !strings.Contains(stderr.String(), "save-plan: ") || !strings.Contains(stderr.String(), draft) {
		t.Errorf("missing kept-draft diagnostic: %s", &stderr)
	}
}

func TestSavePlanPreservesRecordWhenDraftRemovalFails(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := filepath.Join(cfg, "plans", "draft.md")
	if err := os.MkdirAll(draft, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(draft, "keep")
	if err := os.WriteFile(child, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")
	if err := recordDest(filepath.Base(draft), dest); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": cfg,
		"tool_response": map[string]any{"plan": planTemplate, "filePath": draft},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runDirect("docs-plan-save", bytes.NewReader(raw), io.Discard, &stderr); code != ExitProceed {
		t.Fatalf("savePlan = %d, want ExitProceed", code)
	}
	if !strings.HasPrefix(stderr.String(), "save-plan: ") || !strings.Contains(stderr.String(), draft) {
		t.Errorf("missing draft removal diagnostic: %s", &stderr)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != planTemplate {
		t.Errorf("saved plan: err=%v content=%q", err, got)
	}
	if got, ok := readRecordedDest(filepath.Base(draft)); !ok || got != dest {
		t.Errorf("retry destination = %q, ok=%v; want %q", got, ok, dest)
	}
	if got, err := os.ReadFile(child); err != nil || string(got) != "keep" {
		t.Errorf("draft directory contents changed: err=%v content=%q", err, got)
	}
}
