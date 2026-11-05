package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
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

func TestSavePlanWriteFailureDoesNotAudit(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "plans"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := Save(core.ParseInput(strings.NewReader(planPayload(t, cfg, "# Probe\n", ""))), &stderr); code != core.ExitProceed {
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

// TestPlanTitleSkipsFrontMatter: planTitle parses via the mdsmith AST, so a "#"-prefixed line inside the
// "---" front-matter fences is never mistaken for the real H1 below it (fixed from a raw line scan).
func TestPlanTitleSkipsFrontMatter(t *testing.T) {
	plan := "---\n# title: Real Title\ntitle: Something\n---\n\n# Actual Heading\n\nbody\n"
	if got, want := planTitle([]byte(plan)), "actual-heading"; got != want {
		t.Errorf("planTitle(frontmatter) = %q, want %q", got, want)
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

// TestPlanTitleSkipsFencedHeading: a "#" line inside a fenced code block is not an AST heading, so planTitle
// correctly falls through to the real heading after the fence (fixed from a raw line scan).
func TestPlanTitleSkipsFencedHeading(t *testing.T) {
	plan := "```\n# Not A Title\n```\n\n# Real Title\n\nbody\n"
	if got, want := planTitle([]byte(plan)), "real-title"; got != want {
		t.Errorf("planTitle(fenced) = %q, want %q", got, want)
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

func TestPlanSkeletonNoTemplateFailsOpen(t *testing.T) {
	planGroupEnv(t, false)
	out := runDocs(t, Skeleton, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s2", "tool_name": "EnterPlanMode",
	})
	if strings.Contains(out, "# Plan:") {
		t.Fatalf("no template but output = %q", out)
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

// tickFixture commits a/one.go and "b/two file.go" in a fresh repo with a two-task plan; tick feeds a Bash commit result.
func tickFixture(t *testing.T) (planPath, sha string, tick func(command, out string) string) {
	t.Helper()
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "widgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.In(t, repo, "init", "-q")
	gittest.In(t, repo, "config", "commit.gpgsign", "false")
	gittest.WriteRel(t, repo, "a/one.go", "package a\n")
	gittest.WriteRel(t, repo, "b/two file.go", "package b\n")
	gittest.In(t, repo, "add", "a/one.go", "b/two file.go")
	gittest.In(t, repo, "commit", "-q", "-m", "x", "--no-gpg-sign")
	sha = strings.TrimSpace(gittest.In(t, repo, "rev-parse", "--short=7", "HEAD"))

	dir := plans.Home(repo)
	planPath = filepath.Join(dir, "2026-10-29-widgets.md")
	gittest.WriteRel(t, dir, "2026-10-29-widgets.md", "# Plan: w\n\n## Tasks\n\n"+
		"- [ ] 1.1 Full task owns: a/one.go, b/two file.go\n"+
		"- [ ] 1.2 Partial task owns: a/one.go, c/never.go\n")
	t.Setenv("HOME", home)

	tick = func(command, out string) string {
		return runDocs(t, Tick, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "tick1", "tool_name": "Bash", "cwd": t.TempDir(),
			"tool_input":    map[string]string{"command": command},
			"tool_response": map[string]string{"stdout": out},
		})
	}
	return planPath, sha, tick
}

const tickCmd = "git -C ~/widgets commit -m x"

// Tick spanning: real commit -> PostToolUse Bash -> full tick + partial note + context.
func TestPlanTickSpanning(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	cmd := tickCmd

	before, _ := os.ReadFile(planPath)
	// corners: no commit line / unknown sha change nothing
	tick(cmd, "nothing to commit\n")
	tick(cmd, "[main deadbee] x\n 1 file changed\n")
	if after, _ := os.ReadFile(planPath); !bytes.Equal(before, after) {
		t.Fatalf("plan changed on a non-landing commit:\n%s", after)
	}

	out := tick(cmd, "[main "+sha+"] x\n 2 files changed\n")
	got, _ := os.ReadFile(planPath)
	text := string(got)
	if !strings.Contains(text, "- [x] 1.1 Full task owns: a/one.go, b/two file.go ("+sha+")") {
		t.Fatalf("task 1.1 not ticked (space filename should match):\n%s", text)
	}
	if !strings.Contains(text, "- [ ] 1.2") || !strings.Contains(strings.ToLower(text), "partly landed") {
		t.Fatalf("task 1.2 should stay open with a partly-landed note:\n%s", text)
	}
	if !strings.Contains(out, "1.1") || !strings.Contains(out, "1.2") {
		t.Fatalf("additionalContext = %q, want both tasks named", out)
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

// One Bash call with two commits (one detached, one with a quoted filename) ticks both; a fenced checkbox is untouched.
func TestPlanTickMultiCommitDetachedQuoted(t *testing.T) {
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "gadgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.In(t, repo, "init", "-q")
	gittest.In(t, repo, "config", "commit.gpgsign", "false")
	commit := func(rel string) string {
		gittest.WriteRel(t, repo, rel, "x\n")
		gittest.In(t, repo, "add", rel)
		gittest.In(t, repo, "commit", "-q", "-m", "x", "--no-gpg-sign")
		return strings.TrimSpace(gittest.In(t, repo, "rev-parse", "--short=7", "HEAD"))
	}
	s1 := commit("a/one.go")
	s2 := commit("b/café two.go")

	dir := plans.Home(repo)
	planPath := filepath.Join(dir, "2026-10-29-gadgets.md")
	gittest.WriteRel(t, dir, "2026-10-29-gadgets.md", "# Plan: g\n\n- [ ] 1.1 First owns: a/one.go\n"+
		"- [ ] 1.2 Second owns: b/café two.go\n\n```md\n- [ ] 1.3 Fenced owns: a/one.go\n```\n")
	runDocs(t, Tick, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "mc1", "tool_name": "Bash", "cwd": repo,
		"tool_input":    map[string]string{"command": "git commit -m a && git commit -m b"},
		"tool_response": map[string]string{"stdout": "[main " + s1 + "] a\n 1 file changed\n[detached HEAD " + s2 + "] b\n 1 file changed\n"},
	})
	got, _ := os.ReadFile(planPath)
	text := string(got)
	for _, want := range []string{"- [x] 1.1 First owns: a/one.go (" + s1 + ")",
		"- [x] 1.2 Second owns: b/café two.go (" + s2 + ")", "\n- [ ] 1.3 Fenced owns: a/one.go\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
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

// commitRepoFixture builds a command and the (sha, cwd, want) lookups commitRepoDir must answer for it.
type commitLookup struct{ sha, cwd, want string }

func commitRepoFixture(t *testing.T, root string, worktrees bool) (string, []commitLookup) {
	t.Helper()
	g := func(dir string, args ...string) string {
		return strings.TrimSpace(gittest.In(t, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...))
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	g(root, "init", "-q", a)
	g(a, "commit", "-q", "--allow-empty", "-m", "base")
	if worktrees {
		g(a, "worktree", "add", "-q", "-b", "wt", b)
	} else {
		g(root, "init", "-q", b)
	}
	g(a, "commit", "-q", "--allow-empty", "-m", "a")
	g(b, "commit", "-q", "--allow-empty", "-m", "b")
	cmd := "git -C " + a + " commit -m a && git -C " + b + " commit -m b"
	shaA, shaB := g(a, "rev-parse", "HEAD"), g(b, "rev-parse", "HEAD")
	lookups := []commitLookup{{shaA, root, a}, {shaB, root, b}}
	if worktrees { // a non-commit -C is no candidate, so cwd wins
		lookups = append(lookups, commitLookup{shaB, b, b})
	}
	return cmd, lookups
}

// Each sha of a chained `git -C A commit && git -C B commit` resolves to the repo (or linked worktree) that made it.
func TestCommitRepoDirTriesEveryDashC(t *testing.T) {
	for _, worktrees := range []bool{false, true} {
		root := t.TempDir()
		cmd, lookups := commitRepoFixture(t, root, worktrees)
		for _, l := range lookups {
			c := cmd
			if l.cwd != root {
				c = "git -C " + filepath.Join(root, "a") + " status && git commit -m b"
			}
			if got := commitRepoDir(context.Background(), l.cwd, c, l.sha); got != l.want {
				t.Errorf("worktrees=%v cwd=%s: commitRepoDir = %q, want %q", worktrees, l.cwd, got, l.want)
			}
		}
	}
}

func dateStamp() string { return time.Now().Format("2006-01-02") }

const planTemplate = "# Plan: Widget Refactor\n\n## Context\n\nwhy\n\n## Phases\n\n### Phase 1 — do thing\n\n" +
	"**Files:** x\n\n## Tasks\n\n- [ ] Phase 1\n"

func TestWriteVerifiedPlanDropsEmptyClaimOnFailure(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "state"), []byte("x"), 0o600); err != nil { // plans.lock cannot be created
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "plans", "2026-11-05-probe.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, nil, 0o600); err != nil { // the claim planDestination makes
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if writeVerifiedPlan(dest, []byte("# Probe\n"), &stderr) {
		t.Fatal("write with an unusable state dir succeeded")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("empty claim left behind after failed write: stat err = %v", err)
	}
}
