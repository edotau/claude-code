package docs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
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

// TestWithTasks: headings naming a phase/step become items, the closeout tail follows, and a plan that already
// carries a checklist is left exactly as approved.
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

// TestPlanTitlePrefersSlugLine: the template's "Suggested slug:" line names the file; the long H1 is only the
// fallback (a 70-char title-derived name was the complaint).
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

// TestWithTasksOwnTasksHeadingWithoutCheckbox characterizes CURRENT behavior: withTasks only looks for a
// checkbox line (checkboxLine), not a "## Tasks" heading by itself — a heading with no "- [ ]" items yet
// still gets a second "## Tasks" section appended.
func TestWithTasksOwnTasksHeadingWithoutCheckbox(t *testing.T) {
	plan := "# Plan: X\n\n## Tasks\n\nsome prose, no checkbox items yet\n"
	got := string(withTasks([]byte(plan)))
	if strings.Count(got, "## Tasks") != 2 {
		t.Errorf("withTasks(heading-only) = %q, want two \"## Tasks\" sections (current: heading alone doesn't count)", got)
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

// TestWithTasksCRLF characterizes CURRENT behavior on CRLF input: the task-heading regex already strips the
// trailing \r from each extracted item, but the plan/checklist join point keeps a stray \r before the \n\n.
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

// Skeleton: injected on EnterPlanMode, once per plan on plan-mode prompts, re-armed by a non-plan prompt.
func TestPlanSkeletonInjection(t *testing.T) {
	planGroupEnv(t, true)
	prompt := func(mode string) string {
		return runDocs(t, Skeleton, map[string]any{
			"hook_event_name": "UserPromptSubmit", "session_id": "s1", "permission_mode": mode, "prompt": "hello",
		})
	}
	out := runDocs(t, Skeleton, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "EnterPlanMode",
	})
	if !strings.Contains(out, "# Plan:") {
		t.Fatalf("EnterPlanMode context = %q, want the skeleton", out)
	}
	if got := prompt("plan"); strings.Contains(got, "# Plan:") {
		t.Fatalf("plan prompt after EnterPlanMode re-injected: %q", got)
	}
	// marker is set by the first plan-mode prompt only after a clear; a default prompt clears it
	if got := prompt("default"); strings.Contains(got, "# Plan:") {
		t.Fatalf("default prompt injected the skeleton: %q", got)
	}
	if got := prompt("plan"); !strings.Contains(got, "# Plan:") {
		t.Fatalf("plan prompt after a default prompt = %q, want the skeleton again", got)
	}
	if got := prompt("plan"); strings.Contains(got, "# Plan:") {
		t.Fatalf("second plan prompt re-injected: %q", got)
	}
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
		!strings.Contains(gateErr.String(), "skeleton-body-line") {
		t.Fatalf("code=%d err=%q, want a block with the skeleton", code, &gateErr)
	}
	var e bytes.Buffer
	Placement(core.ParseInput(hookJSON(t, payload)), &e)
	if strings.Contains(e.String(), "plan-template") {
		t.Fatalf("docs-placement still emits plan-template: %q", e.String())
	}
}

// The Stop reminder names a partly-landed task once, then stays quiet.
func TestPlanTickReminderOnce(t *testing.T) {
	_, sha, tick := tickFixture(t)
	tick(tickCmd, "[main "+sha+"] x\n 2 files changed\n")
	stop := func() string {
		return Reminder(core.ParseInput(hookJSON(t, map[string]any{"hook_event_name": "Stop", "session_id": "tick1"}))).SystemMessage
	}
	if first := stop(); !strings.Contains(first, "1.2") {
		t.Fatalf("first Stop = %q, want a systemMessage naming 1.2", first)
	}
	if second := stop(); second != "" {
		t.Fatalf("second Stop = %q, want nothing", second)
	}
}

// The Stop reminder drops a partial that a later commit fully ticked.
func TestPlanReminderDropsLaterTicked(t *testing.T) {
	_, _ = planGroupEnv(t, false)
	p := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(p, []byte("- [x] 1.1 Done owns: a (abc1234)\n- [ ] 1.2 Open owns: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordPartials("rem1", "plan", p, []string{"1.1 Done owns: a", "1.2 Open owns: b"}, os.Stderr)
	out := stillPartial(readPartial(t, "rem1"))
	if strings.Contains(out, "1.1") || !strings.Contains(out, "1.2") {
		t.Fatalf("stillPartial = %q, want only 1.2", out)
	}
}

func readPartial(t *testing.T, session string) string {
	t.Helper()
	b, err := os.ReadFile(partialFile(session))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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

// approvalPayload is an ExitPlanMode PostToolUse whose tool_response carries planTemplate + the draft's filePath.
// A nil toolInput omits the tool_input key entirely.
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

// TestPlanMirrorEditReusesDestAndSurvivesH1Change: an Edit updates the same dated file, and renaming the
// H1 afterward still lands in that same file rather than forking a second one.
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

// TestPlanMirrorNeverClobbersAnUnrelatedDatedFile: a dated file that already exists under the computed name
// (and isn't the one this mirror created) is left untouched; the mirror versions around it instead.
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

// TestSavePlanReusesMirroredDestAndCleansUpDraft: approval with EMPTY tool_input and tool_response{plan,
// filePath} reuses the dest plan-mirror already recorded, verifies the dated file, then removes the draft
// and its .dest record.
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

// An unrecordable destination must not leave a dated copy that a retry would duplicate.
func TestPlanMirrorFailedRecordDefersCopy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	writeMirrorDraft(t, filepath.Join(cfg, "state", "harness"), "plans-scratch", "x") // the scratch dir cannot be created
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "funny-random-words.md", planTemplate)
	mirrorRun(t, "unrecordable mirror", "Write", cfg, draft)
	dated, err := filepath.Glob(filepath.Join(cfg, "plans", "2*.md"))
	if err != nil || len(dated) != 0 {
		t.Fatalf("dated copies = %v (err %v), want none until the destination can be recorded", dated, err)
	}
}

// TestPlanSkeletonMissingTemplateReportsOnStderr: a missing template fails open but names the cause.
func TestPlanSkeletonMissingTemplateReportsOnStderr(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	in := core.ParseInput(hookJSON(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "EnterPlanMode"}))
	res := Skeleton(in)
	if res.Context != "" || !strings.Contains(res.Stderr, "plan-skeleton: ") {
		t.Fatalf("result = %+v, want no context and a plan-skeleton diagnostic", res)
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

// TestPlanTemplateTeeth proves the gate actually fires: today (no gate consulted) a free-form first write to
// an undated draft passes; with PlanTemplate wired in, the same write is denied, and the message names plan.md.
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
		!strings.Contains(stderr.String(), plans.TemplatePath(cfg)) {
		t.Errorf("missing first heading/template diagnostic: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(cfg, rel)); !os.IsNotExist(err) {
		t.Errorf("denied draft must remain absent: %v", err)
	}
}

func TestPlanGateTemplate(t *testing.T) {
	for _, knob := range []string{"1", "0"} {
		for _, c := range planGateCases() {
			t.Run("docs="+knob+"/"+c.name, func(t *testing.T) {
				cfg := t.TempDir()
				t.Setenv("CLAUDE_CONFIG_DIR", cfg)
				t.Setenv(docsPlacementDisableEnv, knob)
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
