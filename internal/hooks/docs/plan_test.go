package docs

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
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

// planFallbackCase is one input/response combination for the Save plan-text fallback chain.
type planFallbackCase struct {
	name         string
	inputPlan    string
	responsePlan string
	responseFile bool
	missingFile  bool
	inputFile    bool
	want         string
}

// scratchFile writes text to a fresh scratch.md (or just names it when missing) and returns its path.
func scratchFile(t *testing.T, missing bool, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scratch.md")
	if missing {
		return path
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fallbackPayload builds the ExitPlanMode payload for tc, pointing its file fields at real scratch files.
func fallbackPayload(t *testing.T, cfg string, tc planFallbackCase) []byte {
	t.Helper()
	input := map[string]any{}
	if tc.inputPlan != "" {
		input["plan"] = tc.inputPlan
	}
	response := map[string]any{"plan": tc.responsePlan}
	if tc.responseFile {
		response["filePath"] = scratchFile(t, tc.missingFile, "# Response file\n")
	}
	if tc.inputFile {
		input["planFilePath"] = scratchFile(t, false, "# Input file\n")
	}
	raw, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": cfg,
		"tool_input": input, "tool_response": response,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// wantSavedPlan requires exactly one saved plan holding the tasked want text and one matching plan_saved event.
func wantSavedPlan(t *testing.T, cfg, want string, names []string, events []audit.Event) {
	t.Helper()
	if len(names) != 1 {
		t.Fatalf("saved plans = %v, want one", names)
	}
	dest := filepath.Join(cfg, "plans", filepath.FromSlash(names[0]))
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, withTasks([]byte(want))) {
		t.Fatalf("saved plan = %q, err = %v, want %q", got, err, want)
	}
	if len(events) != 1 || string(events[0].Action) != "plan_saved" ||
		events[0].Resource != "save-plan" || events[0].Detail != dest {
		t.Fatalf("audit events = %+v, want one plan_saved with destination only", events)
	}
}

func TestSavePlanResponseFallbacksAndAudit(t *testing.T) {
	for _, tc := range []planFallbackCase{
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
			raw := fallbackPayload(t, cfg, tc)
			for range 2 {
				if code := Save(core.ParseInput(bytes.NewReader(raw)), &bytes.Buffer{}); code != core.ExitProceed {
					t.Fatalf("Save = %d", code)
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
			wantSavedPlan(t, cfg, tc.want, names, events)
		})
	}
}

func TestSavePlanRelativeFallback(t *testing.T) {
	cfg, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	const plan = "# Relative approved plan\n"
	if err := os.WriteFile(filepath.Join(cwd, "scratch.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	in := core.ParseInput(strings.NewReader(planPayload(t, cwd, "", "scratch.md")))
	if code := Save(in, &stderr); code != core.ExitProceed || stderr.Len() != 0 {
		t.Fatalf("Save = %d, stderr = %s", code, &stderr)
	}
	names := savedPlans(t, cfg)
	if len(names) != 1 {
		t.Fatalf("saved plans = %v, want one", names)
	}
	got, err := os.ReadFile(filepath.Join(cfg, "plans", names[0]))
	if err != nil || !bytes.Equal(got, withTasks([]byte(plan))) {
		t.Fatalf("saved plan = %q, err = %v", got, err)
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
		if code := Save(core.ParseInput(strings.NewReader(planPayload(t, repo, plan, ""))), &bytes.Buffer{}); code != core.ExitProceed {
			t.Fatalf("Save must never block, got %d", code)
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

// TestWithTasks: phase/step headings become items plus the closeout tail; an existing checklist is left as approved.
func TestWithTasks(t *testing.T) {
	got := string(withTasks([]byte("# Plan: X\n\n### Phase 1 — parse\n\n## Step 2: render\n\n## Context\n")))
	for _, want := range []string{"- [ ] Phase 1 — parse\n", "- [ ] Step 2: render\n", "- [ ] Full suite once\n",
		"- [ ] Review (`claude-code codex review --adversarial --base <branch>`)\n"} {
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

// TestPlanTitlePrefersSlugLine: the template's "Suggested slug:" line names the file; the long H1 is the fallback.
func TestPlanTitlePrefersSlugLine(t *testing.T) {
	plan := "# Plan: refine fable-method against upstream v1.4.0 and push its proven artifacts\n\n" +
		"Suggested slug: `fable-method-upstream-refine`\n\n## Context\n"
	if got, want := planTitle([]byte(plan)), "fable-method-upstream-refine"; got != want {
		t.Errorf("planTitle(slug line) = %q, want %q", got, want)
	}
	bare := "# Plan: X\n\nslug: Fable Refine\n"
	if got, want := planTitle([]byte(bare)), "x"; got != want {
		t.Errorf("planTitle(non-slug value) must fall back to the H1, got %q, want %q", got, want)
	}
}

// TestWithTasksOwnTasksHeadingWithoutCheckbox: a plan's own "## Tasks" heading is kept, never doubled.
func TestWithTasksOwnTasksHeadingWithoutCheckbox(t *testing.T) {
	plan := "# Plan: X\n\n## Tasks\n\nsome prose, no checkbox items yet\n"
	if got := string(withTasks([]byte(plan))); got != plan {
		t.Errorf("withTasks(heading-only) = %q, want the plan unchanged", got)
	}
}

// TestWithTasksSkipsFencedHeadings: a Phase heading shown inside a code fence is example text, not a task.
func TestWithTasksSkipsFencedHeadings(t *testing.T) {
	plan := "# Plan: Z\n\n```markdown\n## Phase fake\n```\n\n## Phase real\n"
	got := string(withTasks([]byte(plan)))
	if !strings.Contains(got, "- [ ] Phase real\n") || strings.Contains(got, "- [ ] Phase fake") {
		t.Errorf("withTasks(fenced) = %q, want only the real Phase as a task", got)
	}
}

// TestWithTasksMixedHeadingLevels: phase/step/task headings at any level 2-4 all become checklist items.
func TestWithTasksMixedHeadingLevels(t *testing.T) {
	plan := "# Plan: Y\n\n## Phase A\n\n#### Phase B (deep)\n\n### Task: inner\n"
	got := string(withTasks([]byte(plan)))
	for _, want := range []string{"- [ ] Phase A\n", "- [ ] Phase B (deep)\n", "- [ ] Task: inner\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestWithTasksCRLF characterizes CURRENT behavior: a stray \r stays before the \n\n at the plan/checklist join.
func TestWithTasksCRLF(t *testing.T) {
	plan := "# Plan: X\r\n\r\n### Phase 1 \xe2\x80\x94 parse\r\n\r\n## Step 2: render\r\n"
	got := string(withTasks([]byte(plan)))
	for _, want := range []string{"- [ ] Phase 1 \xe2\x80\x94 parse\n", "- [ ] Step 2: render\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q (CRLF item should still be captured without \\r) in:\n%q", want, got)
		}
	}
	if !strings.Contains(got, "render\r\n\n## Tasks\n") {
		t.Errorf("current: the join before the appended checklist keeps the draft's trailing \\r, got:\n%q", got)
	}
}

func TestSavePlanReadsPlanFilePathAndSkipsEmpty(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	src := filepath.Join(t.TempDir(), "scratch.md")
	if err := os.WriteFile(src, []byte("no heading here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Save(core.ParseInput(strings.NewReader(planPayload(t, cfg, "", src))), &bytes.Buffer{})
	Save(core.ParseInput(strings.NewReader(planPayload(t, cfg, "  \n", ""))), &bytes.Buffer{})
	want := time.Now().Format("2006-01-02") + "-plan.md" // config dir: untagged; no heading: "plan"
	if got := savedPlans(t, cfg); len(got) != 1 || got[0] != want {
		t.Errorf("saved = %v, want [%s]", got, want)
	}
}

// planGroupEnv is a hermetic config dir + HOME, optionally with the docs/templates/plan.md template.
func planGroupEnv(t *testing.T, withTemplate bool) (cfg, home string) {
	t.Helper()
	cfg, home = t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	if withTemplate {
		md := "# Plan: <title>\n\n## Context\n\n## Phases\n\n## Tasks\n\n- [ ] skeleton-body-line\n"
		if err := os.MkdirAll(filepath.Dir(plans.TemplatePath(cfg)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plans.TemplatePath(cfg), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, home
}

func hookJSON(t *testing.T, m map[string]any) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(b))
}

// runDocs feeds one payload to a docs Runner and returns the Result's context.
func runDocs(t *testing.T, run core.Runner, m map[string]any) string {
	t.Helper()
	return run(core.ParseInput(hookJSON(t, m))).Context
}

// plan-gate: the first template-less write to an undated draft is blocked with the skeleton; docs-placement is silent.
func TestPlanGateBlocksWithSkeleton(t *testing.T) {
	cfg, _ := planGroupEnv(t, true)
	draft := filepath.Join(cfg, "plans", "funny-words.md")
	payload := map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": cfg,
		"tool_input": map[string]string{"file_path": draft, "content": "free-form text\n"},
	}
	var gateErr bytes.Buffer
	code := Gate(core.ParseInput(hookJSON(t, payload)), &gateErr)
	if code != core.ExitBlock || !strings.Contains(gateErr.String(), "plan-template: BLOCKED") ||
		!strings.Contains(gateErr.String(), "## Tasks") || !strings.Contains(gateErr.String(), plans.TemplatePath(cfg)) {
		t.Fatalf("code=%d err=%q, want a block with the skeleton and the template path", code, &gateErr)
	}
	var e bytes.Buffer
	placeFiles(core.ParseInput(hookJSON(t, payload)), &e)
	if strings.Contains(e.String(), "plan-template") {
		t.Fatalf("docs-placement still emits plan-template: %q", e.String())
	}
}

func dateStamp() string { return time.Now().Format("2006-01-02") }

const planTemplate = "# Plan: Widget Refactor\n\n## Context\n\nwhy\n\n## Phases\n\n### Phase 1 — do thing\n\n" +
	"**Files:** x\n\n## Tasks\n\n- [ ] Phase 1\n"

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

// mirrorRun feeds one docs-plan-mirror PostToolUse for tool/path and requires the hook to fail open.
func mirrorRun(t *testing.T, label, tool, cwd, path string) {
	t.Helper()
	if code := Mirror(core.ParseInput(strings.NewReader(mirrorPayload(t, tool, cwd, path))), &bytes.Buffer{}); code != core.ExitProceed {
		t.Fatalf("%s = %d", label, code)
	}
}

// writeMirrorDraft writes content to dir/name (creating dir) and returns the file's path.
func writeMirrorDraft(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantMirrorFile fails unless path exists and holds exactly want.
func wantMirrorFile(t *testing.T, path, want, what string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s %s: err=%v content=%q", what, path, err, got)
	}
}

// approvalPayload is an ExitPlanMode PostToolUse with planTemplate and the draft's filePath; nil toolInput omits it.
func approvalPayload(t *testing.T, cwd, draft string, toolInput map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "ExitPlanMode", "cwd": cwd,
		"tool_response": map[string]any{"plan": planTemplate, "filePath": draft},
	}
	if toolInput != nil {
		m["tool_input"] = toolInput
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// approvePlan runs docs-plan-save on raw, requires it to fail open, and returns what it wrote to stderr.
func approvePlan(t *testing.T, raw []byte) string {
	t.Helper()
	var stderr bytes.Buffer
	if code := Save(core.ParseInput(bytes.NewReader(raw)), &stderr); code != core.ExitProceed {
		t.Fatalf("Save = %d", code)
	}
	return stderr.String()
}

func TestPlanMirrorTopLevelWhenCWDIsConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "funny-random-words.md", planTemplate)
	mirrorRun(t, "Mirror", "Write", cfg, draft)
	wantMirrorFile(t, filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md"), planTemplate, "mirrored file at")
	if _, err := os.Stat(filepath.Join(cfg, "state", "harness", "plans-scratch", "funny-random-words.md.dest")); err != nil {
		t.Fatalf("expected a recorded dest: %v", err)
	}
}

func TestPlanMirrorRepoTaggedWhenCWDIsAnotherRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := hosttest.Repo(t)
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "other-words.md", planTemplate)
	mirrorRun(t, "Mirror", "Write", repo, draft)
	want := filepath.Join(cfg, "plans", filepath.Base(repo), dateStamp()+"-widget-refactor.md")
	if _, err := os.ReadFile(want); err != nil {
		t.Fatalf("mirrored repo-tagged file missing at %s: %v", want, err)
	}
}

// countPlanFiles counts the non-directory entries directly under dir.
func countPlanFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	return count
}

// TestPlanMirrorEditReusesDestAndSurvivesH1Change: an Edit and a later H1 rename still land in the same dated file.
func TestPlanMirrorEditReusesDestAndSurvivesH1Change(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansDir := filepath.Join(cfg, "plans")
	draft := writeMirrorDraft(t, plansDir, "funny-random-words.md", planTemplate)
	mirrorRun(t, "first mirror", "Write", cfg, draft)
	dest := filepath.Join(plansDir, dateStamp()+"-widget-refactor.md")

	edited := strings.Replace(planTemplate, "why\n", "why more\n", 1)
	writeMirrorDraft(t, plansDir, "funny-random-words.md", edited)
	mirrorRun(t, "edit mirror", "Edit", cfg, draft)
	wantMirrorFile(t, dest, edited, "edit did not update")

	renamed := strings.Replace(edited, "# Plan: Widget Refactor", "# Plan: Renamed Title", 1)
	writeMirrorDraft(t, plansDir, "funny-random-words.md", renamed)
	mirrorRun(t, "H1-change mirror", "Edit", cfg, draft)
	wantMirrorFile(t, dest, renamed, "H1 change must keep the same dest")
	if count := countPlanFiles(t, plansDir); count != 2 { // the dated file + the still-live undated draft
		t.Fatalf("plans/ holds %d files, want exactly the dated file and the draft", count)
	}
}

// TestPlanMirrorNeverClobbersAnUnrelatedDatedFile: a dated file the mirror did not create is versioned around.
func TestPlanMirrorNeverClobbersAnUnrelatedDatedFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansDir := filepath.Join(cfg, "plans")
	unrelated := "# Plan: Widget Refactor\n\nsome other plan, not ours\n"
	preexisting := writeMirrorDraft(t, plansDir, dateStamp()+"-widget-refactor.md", unrelated)
	draft := writeMirrorDraft(t, plansDir, "funny-random-words.md", planTemplate)
	mirrorRun(t, "Mirror", "Write", cfg, draft)
	wantMirrorFile(t, preexisting, unrelated, "the unrelated dated file was touched:")
	wantMirrorFile(t, filepath.Join(plansDir, dateStamp()+"-widget-refactor-2.md"), planTemplate, "mirror should have versioned to")
}

// TestSavePlanReusesMirroredDestAndCleansUpDraft: approval reuses the mirror's dest, then removes the draft and .dest.
func TestSavePlanReusesMirroredDestAndCleansUpDraft(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := hosttest.Repo(t)
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "other-words.md", planTemplate)
	mirrorRun(t, "Mirror", "Write", repo, draft)
	dest := filepath.Join(cfg, "plans", filepath.Base(repo), dateStamp()+"-widget-refactor.md")
	if _, err := os.ReadFile(dest); err != nil {
		t.Fatalf("mirror did not create %s: %v", dest, err)
	}

	approvePlan(t, approvalPayload(t, repo, draft, map[string]any{}))
	wantMirrorFile(t, dest, planTemplate, "final dest")
	if _, err := os.Stat(draft); !os.IsNotExist(err) {
		t.Fatalf("the undated draft must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "harness", "plans-scratch", "other-words.md.dest")); !os.IsNotExist(err) {
		t.Fatalf("the .dest record must be removed, stat err = %v", err)
	}
}

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

// TestPlanTemplateTeeth: a free-form first write passes with no gate, and is denied naming plan.md once wired.
func TestPlanTemplateTeeth(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")

	var stderr bytes.Buffer
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, "just some free-form draft text\n"))), &stderr)
	if code != core.ExitBlock {
		t.Fatalf("free-form first write: PlanTemplate = %d, want ExitBlock", code)
	}
	if !strings.Contains(stderr.String(), "plan-template") || !strings.Contains(stderr.String(), "plan.md") {
		t.Fatalf("denial message = %q, want it to name plan-template and plan.md", &stderr)
	}
	if _, err := os.Stat(draft); err == nil {
		t.Fatalf("a denied write must not have landed on disk")
	}
}

type planGateCase struct {
	name     string
	tool     string
	path     string
	content  string
	existing bool
	want     int
}

func planGateCases() []planGateCase {
	return []planGateCase{
		{"freeform Write", "Write", "plans/draft.md", "freeform", false, core.ExitBlock},
		{"freeform create", "create", "plans/draft.md", "freeform", false, core.ExitBlock},
		{"legacy empty tool", "", "plans/draft.md", "freeform", false, core.ExitBlock},
		{"missing content", "Write", "plans/draft.md", "", false, core.ExitBlock},
		{"templated Write", "Write", "plans/draft.md", planTemplate, false, core.ExitProceed},
		{"existing draft", "Write", "plans/draft.md", "freeform", true, core.ExitProceed},
		{"Edit bypass", "Edit", "plans/draft.md", "freeform", false, core.ExitProceed},
		{"patch bypass", "apply_patch", "plans/draft.md", "freeform", false, core.ExitProceed},
		{"other tool", "Bash", "plans/draft.md", "freeform", false, core.ExitProceed},
		{"dated plan", "Write", "plans/2026-10-20-draft.md", "freeform", false, core.ExitProceed},
		{"nested plan", "Write", "plans/project/draft.md", "freeform", false, core.ExitProceed},
		{"lowercase title", "Write", "plans/draft.md", strings.Replace(planTemplate, "# Plan:", "# plan:", 1), false, core.ExitProceed},
		{"plans README", "Write", "plans/README.md", "index of plans", false, core.ExitProceed},
		{"plans readme lowercase", "Write", "plans/readme.markdown", "index", false, core.ExitProceed},
		{"README-named draft", "Write", "plans/README-x.md", "freeform", false, core.ExitBlock},
		{"outside plans", "Write", "other/draft.md", "freeform", false, core.ExitProceed},
	}
}

// seedPlanGateDraft creates an empty draft at cfg/rel so the gate sees an existing file.
func seedPlanGateDraft(t *testing.T, cfg, rel string) {
	t.Helper()
	path := filepath.Join(cfg, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// requirePlanGateDenied asserts the block diagnostic and that the denied draft stays absent.
func requirePlanGateDenied(t *testing.T, cfg, rel string, stderr *bytes.Buffer) {
	t.Helper()
	if !strings.HasPrefix(stderr.String(), "plan-template: BLOCKED") ||
		!strings.Contains(stderr.String(), `missing "# Plan:"`) ||
		!strings.Contains(stderr.String(), "docs/templates/plan.md") {
		t.Errorf("missing first heading/template diagnostic: %s", stderr)
	}
	// An absent template is never named as the file to copy (cfg need not be the harness clone).
	if tpl := plans.TemplatePath(cfg); paths.IsFile(tpl) != strings.Contains(stderr.String(), tpl) {
		t.Errorf("template path %s named iff it exists, got: %s", tpl, stderr)
	}
	if _, err := os.Stat(filepath.Join(cfg, rel)); !os.IsNotExist(err) {
		t.Errorf("denied draft must remain absent: %v", err)
	}
}

func TestPlanGateTemplate(t *testing.T) {
	for _, c := range planGateCases() {
		t.Run(c.name, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", cfg)
			if c.existing {
				seedPlanGateDraft(t, cfg, c.path)
			}
			var stderr bytes.Buffer
			code := Gate(core.ParseInput(strings.NewReader(writePayload(t, c.tool, cfg, c.path, c.content))), &stderr)
			if code != c.want {
				t.Fatalf("code = %d, want %d; stderr=%s", code, c.want, &stderr)
			}
			if c.want == core.ExitBlock {
				requirePlanGateDenied(t, cfg, c.path, &stderr)
			} else if stderr.Len() != 0 {
				t.Errorf("allowed write emitted a diagnostic: %s", &stderr)
			}
		})
	}
}

// codexPatchPayload is Codex's PreToolUse document for an apply_patch call: the patch body under tool_input.command.
func codexPatchPayload(t *testing.T, cwd string, hunks ...string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "apply_patch", "cwd": cwd,
		"tool_input": map[string]string{"command": "*** Begin Patch\n" + strings.Join(hunks, "") + "*** End Patch\n"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// addHunk is an apply_patch Add File hunk writing content to path.
func addHunk(path, content string) string {
	return "*** Add File: " + path + "\n+" + strings.ReplaceAll(strings.TrimSuffix(content, "\n"), "\n", "\n+") + "\n"
}

// A Codex Add File hunk is a first write: judged like Claude's Write to the same draft, every added file on its own.
func TestPlanGateCodexPatch(t *testing.T) {
	for name, c := range map[string]struct {
		hunks []string
		want  int
	}{
		"freeform add":         {[]string{addHunk("plans/draft.md", "freeform")}, core.ExitBlock},
		"templated add":        {[]string{addHunk("plans/draft.md", planTemplate)}, core.ExitProceed},
		"update is an edit":    {[]string{"*** Update File: plans/draft.md\n@@\n+freeform\n"}, core.ExitProceed},
		"second file freeform": {[]string{addHunk("notes/a.md", "x"), addHunk("plans/draft.md", "freeform")}, core.ExitBlock},
		"templated then other": {[]string{addHunk("plans/draft.md", planTemplate), addHunk("other/b.md", "x")}, core.ExitProceed},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", cfg)
			var stderr bytes.Buffer
			if code := Gate(core.ParseInput(strings.NewReader(codexPatchPayload(t, cfg, c.hunks...))), &stderr); code != c.want {
				t.Fatalf("code = %d, want %d; stderr=%s", code, c.want, &stderr)
			}
			if c.want == core.ExitBlock {
				requirePlanGateDenied(t, cfg, "plans/draft.md", &stderr)
			}
		})
	}
}

// A first write of a plan-named markdown at the repo root is rewritten to the dated plans home; others are not.
func TestPlanGateRewritesStrandedPlan(t *testing.T) {
	cfg, _ := planGroupEnv(t, true)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	run := func(mode, path string) core.Result {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": repo, "permission_mode": mode,
			"tool_input": map[string]string{"file_path": path, "content": "x"}})
		return Placement(core.ParseInput(bytes.NewReader(b)))
	}
	res := run("default", filepath.Join(repo, "my-plan.md"))
	got, _ := res.UpdatedInput["file_path"].(string)
	want := filepath.Join(plans.Home(repo), time.Now().Format("2006-01-02")+"-my-plan.md")
	if res.Decision == core.Block || got != want {
		t.Errorf("rewrite = %+v, want file_path %s", res, want)
	}
	if r := run("auto", filepath.Join(repo, "my-plan.md")); r.Decision == core.Block || r.UpdatedInput["file_path"] != want {
		t.Errorf("auto = %+v, want the same rewrite to %s", r, want)
	}
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": repo,
		"tool_input": map[string]string{"file_path": filepath.Join(repo, "my-plan.md"), "content": "x"}})
	codex := Placement(core.ParseInputFor(bytes.NewReader(b), core.AgentCodex)) // codex cannot rewrite: deny names both ways out
	if codex.Decision != core.Block || !strings.Contains(codex.Reason, want) || !strings.Contains(codex.Reason, "touch "+filepath.Join(repo, "my-plan.md")) {
		t.Errorf("codex deny = %q, want the dated path and the create-it-empty escape", codex.Reason)
	}
	for _, p := range []string{filepath.Join(repo, "notes.md"), filepath.Join(repo, "src", "plan.md"), filepath.Join(cfg, "plans", "2026-01-01-x.md")} {
		if r := run("default", p); r.UpdatedInput != nil || strings.Contains(r.Reason, "plan-path") {
			t.Errorf("%s = %+v, want no plan-path rewrite", p, r)
		}
	}
	bare := t.TempDir() // a cwd in no git repo passes, as docs-placement says
	b, _ = json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": bare,
		"tool_input": map[string]string{"file_path": filepath.Join(bare, "my-plan.md"), "content": "x"}})
	if r := Placement(core.ParseInput(bytes.NewReader(b))); r.UpdatedInput != nil || r.Decision == core.Block {
		t.Errorf("non-repo cwd = %+v, want untouched", r)
	}
}

// A blocked plans home denies at once naming the path; a name merely containing "plan" is not a plan.
func TestPlanGateFailsFastAndIgnoresPlanSubstrings(t *testing.T) {
	cfg, _ := planGroupEnv(t, true)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	run := func(name string) core.Result {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": repo,
			"tool_input": map[string]string{"file_path": filepath.Join(repo, name), "content": "x"}})
		return Placement(core.ParseInput(bytes.NewReader(b)))
	}
	for _, name := range []string{"explanation.md", "planet.md"} {
		if r := run(name); r.UpdatedInput != nil || strings.Contains(r.Reason, "plan-path") {
			t.Errorf("%s = %+v, want no plan-path rewrite", name, r)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg, "plans"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := run("my-plan.md")
	if r.Decision != core.Block || !strings.Contains(r.Reason, filepath.Join(cfg, "plans")) || time.Since(start) > time.Second {
		t.Errorf("blocked plans home = %+v after %v, want an immediate deny naming %s", r, time.Since(start), filepath.Join(cfg, "plans"))
	}
}

func checkPlanDenied(t *testing.T, res core.Result, missing string) {
	t.Helper()
	if res.Decision != core.Block || res.UpdatedInput != nil || !strings.Contains(res.Reason, "missing "+missing) {
		t.Errorf("want a deny listing %s, got %+v", missing, res)
	}
}

func checkPlanFixed(t *testing.T, res core.Result, draft, wantPrefix string) {
	t.Helper()
	got, _ := res.UpdatedInput["content"].(string)
	if res.Decision == core.Block || !strings.HasPrefix(got, wantPrefix) || res.UpdatedInput["file_path"] != draft ||
		!strings.HasPrefix(res.SystemMessage, "plan-template: added ") || len(missingTemplate(got)) != 0 {
		t.Errorf("want content starting %q and a note, got %+v", wantPrefix, res)
	}
}

// A Claude first draft missing only the "# Plan:" prefix or ## Tasks is rewritten in one updatedInput; else denied.
func TestPlanGateFixesTitleAndTasks(t *testing.T) {
	const body = "## Context\n\nwhy\n\n## Phases\n\n### Phase 1: go\n"
	for _, c := range []struct {
		name, agent, content string
		want                 string // updatedInput content prefix, or the deny's missing list
		fixed                bool
	}{
		{"prefix and tasks", core.AgentClaude, "# Migrate X\n\n" + body, "# Plan: Migrate X\n\n" + body + "\n## Tasks\n", true},
		{"tasks only", core.AgentClaude, "# Plan: X\n\n" + body, "# Plan: X\n\n" + body + "\n## Tasks\n", true},
		{"prefix only", core.AgentClaude, "# X\n\n" + body + "\n## Tasks\n\n- [ ] a\n", "# Plan: X\n\n" + body + "\n## Tasks\n\n- [ ] a\n", true},
		{"blank lines then title", core.AgentClaude, "\n\n# X\n" + body, "\n\n# Plan: X\n" + body, true},
		{"prose before title", core.AgentClaude, "Intro.\n\n# X\n" + body, `"# Plan:", "## Tasks" from`, false},
		{"no phases", core.AgentClaude, "# X\n\n## Context\n", `"# Plan:", "## Phases", "## Tasks" from`, false},
		{"no title", core.AgentClaude, body, `"# Plan:", "## Tasks" from`, false},
		{"checkbox, no Tasks heading", core.AgentClaude, "# Plan: X\n\n" + body + "\n- [ ] a\n", `"## Tasks" from`, false},
		{"codex keeps the deny", core.AgentCodex, "# X\n\n" + body, `"# Plan:", "## Tasks" from`, false},
		{"opencode keeps the deny", core.AgentOpencode, "# X\n\n" + body, `"# Plan:", "## Tasks" from`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := planGroupEnv(t, false)
			draft := filepath.Join(cfg, "plans", "happy-otter.md")
			res := Placement(core.ParseInputFor(strings.NewReader(writePayload(t, "Write", cfg, draft, c.content)), c.agent))
			if c.fixed {
				checkPlanFixed(t, res, draft, c.want)
			} else {
				checkPlanDenied(t, res, c.want)
			}
		})
	}
}

// Every apply_patch body key, opencode's patchText included, reaches the draft gate.
func TestPlanGatePatchKeys(t *testing.T) {
	cfg, _ := planGroupEnv(t, false)
	for _, key := range []string{"patchText", "patch", "input", "command"} {
		b, _ := json.Marshal(map[string]any{"tool_name": "apply_patch", "cwd": cfg,
			"tool_input": map[string]string{key: "*** Begin Patch\n" + addHunk("plans/draft.md", "freeform") + "*** End Patch\n"}})
		var stderr bytes.Buffer
		if code := Gate(core.ParseInput(bytes.NewReader(b)), &stderr); code != core.ExitBlock {
			t.Errorf("%s: code = %d, want block (%s)", key, code, &stderr)
		}
	}
}
