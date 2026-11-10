package docs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/plans"
)

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

// TestPlanMirrorUntaggedWhenCWDIsMemoryStoreRepo: a cwd in memory/<repo> is the harness, so the copy lands in plans/.
func TestPlanMirrorUntaggedWhenCWDIsMemoryStoreRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	store := gittest.RepoAt(t, filepath.Join(cfg, "memory", "claude-code"))
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "store-words.md", planTemplate)
	mirrorRun(t, "Mirror", "Write", store, draft)
	wantMirrorFile(t, filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md"), planTemplate, "mirrored file at")
}

func TestPlanMirrorRepoTaggedWhenCWDIsAnotherRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := gittest.Repo(t)
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

// TestPlanMirrorNeverAdoptsAnIdenticalDatedFile: two drafts share the same H1 and the same first-mirrored
// content on the same day. The second draft must never adopt the first's dated file as its own dest — even
// though the bytes are identical — or a later edit to draft B would silently overwrite draft A's file.
func TestPlanMirrorNeverAdoptsAnIdenticalDatedFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansDir := filepath.Join(cfg, "plans")
	draftA := writeMirrorDraft(t, plansDir, "aaa-words.md", planTemplate)
	draftB := writeMirrorDraft(t, plansDir, "bbb-words.md", planTemplate)
	mirrorRun(t, "mirror A", "Write", cfg, draftA)
	mirrorRun(t, "mirror B", "Write", cfg, draftB)
	destA := filepath.Join(plansDir, dateStamp()+"-widget-refactor.md")
	destB := filepath.Join(plansDir, dateStamp()+"-widget-refactor-2.md")
	wantMirrorFile(t, destA, planTemplate, "destA")
	wantMirrorFile(t, destB, planTemplate, "destB")

	edited := strings.Replace(planTemplate, "why\n", "draft B's own edit\n", 1)
	writeMirrorDraft(t, plansDir, "bbb-words.md", edited)
	mirrorRun(t, "edit mirror B", "Edit", cfg, draftB)
	wantMirrorFile(t, destA, planTemplate, "editing draft B must never change destA")
	wantMirrorFile(t, destB, edited, "destB should hold draft B's edit")
}

// TestSavePlanReusesMirroredDestAndCleansUpDraft: approval with EMPTY tool_input and tool_response{plan,
// filePath} reuses the dest plan-mirror already recorded, verifies the dated file, then removes the draft
// and its .dest record.
func TestSavePlanReusesMirroredDestAndCleansUpDraft(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := gittest.Repo(t)
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

// TestSavePlanKeepsDraftEditedAfterApproval: the draft on disk no longer matches the approved plan text
// (e.g. a concurrent agent kept editing it) — Save must file the approved plan but leave the draft alone.
func TestSavePlanKeepsDraftEditedAfterApproval(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	edited := strings.Replace(planTemplate, "why\n", "why, edited after approval\n", 1)
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "funny-random-words.md", edited)
	stderr := approvePlan(t, approvalPayload(t, cfg, draft, map[string]any{}))
	wantMirrorFile(t, filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md"), planTemplate, "approved plan not saved at")
	wantMirrorFile(t, draft, edited, "draft edited after approval must be kept untouched:")
	if !strings.Contains(stderr, "save-plan: ") || !strings.Contains(stderr, draft) {
		t.Errorf("missing kept-draft diagnostic: %s", stderr)
	}
}

func TestSavePlanFailedRecordRemovalCannotOverwriteApproval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory deletion permissions")
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansDir := filepath.Join(cfg, "plans")
	draft := writeMirrorDraft(t, plansDir, "pending-words.md", planTemplate)
	mirrorRun(t, "initial mirror", "Write", cfg, draft)
	dest := filepath.Join(plansDir, dateStamp()+"-widget-refactor.md")
	edited := strings.Replace(planTemplate, "why\n", "unapproved edit\n", 1)
	writeMirrorDraft(t, plansDir, filepath.Base(draft), edited)
	scratch := plans.ScratchDir()
	t.Cleanup(func() {
		if err := os.Chmod(scratch, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(scratch, 0o500); err != nil {
		t.Fatal(err)
	}
	stderr := approvePlan(t, approvalPayload(t, cfg, draft, nil))
	if !strings.Contains(stderr, plans.DestRecordPath(filepath.Base(draft))) {
		t.Fatalf("expected failed record removal diagnostic, got %q", stderr)
	}
	wantMirrorFile(t, plans.DestRecordPath(filepath.Base(draft)), "", "surviving record must be inert")
	wantMirrorFile(t, draft, edited, "edited draft must survive approval")
	wantMirrorFile(t, dest, planTemplate, "approved plan")
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	mirrorRun(t, "post-approval edit", "Edit", cfg, draft)
	wantMirrorFile(t, dest, planTemplate, "later mirror must not overwrite the approved plan")
	wantMirrorFile(t, filepath.Join(plansDir, dateStamp()+"-widget-refactor-2.md"), edited, "new draft mirror")
}

func TestSavePlanPreservesRecordWhenDraftRemovalFails(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := filepath.Join(cfg, "plans", "draft.md")
	child := writeMirrorDraft(t, draft, "keep", "keep") // the draft is a non-empty directory, so removal fails
	dest := filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md")
	if err := plans.RecordDest(filepath.Base(draft), dest); err != nil {
		t.Fatal(err)
	}
	stderr := approvePlan(t, approvalPayload(t, cfg, draft, nil))
	if !strings.HasPrefix(stderr, "save-plan: ") || !strings.Contains(stderr, draft) {
		t.Errorf("missing draft removal diagnostic: %s", stderr)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != planTemplate {
		t.Errorf("saved plan: err=%v content=%q", err, got)
	}
	if got, ok := plans.ReadRecordedDest(filepath.Base(draft)); !ok || got != dest {
		t.Errorf("retry destination = %q, ok=%v; want %q", got, ok, dest)
	}
	if got, err := os.ReadFile(child); err != nil || string(got) != "keep" {
		t.Errorf("draft directory contents changed: err=%v content=%q", err, got)
	}
}

// TestPlanMirrorFailedWriteDropsEmptyClaim: a first mirror whose write fails leaves no 0-byte dated placeholder.
func TestPlanMirrorFailedWriteDropsEmptyClaim(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "state"), []byte("x"), 0o600); err != nil { // plans.lock cannot be created
		t.Fatal(err)
	}
	plansDir := filepath.Join(cfg, "plans")
	draft := writeMirrorDraft(t, plansDir, "funny-random-words.md", planTemplate)
	mirrorRun(t, "failing mirror", "Write", cfg, draft)
	dest := filepath.Join(plansDir, dateStamp()+"-widget-refactor.md")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("empty claim left behind after failed mirror: stat err = %v", err)
	}
}

// TestPlanMirrorFailedRecordStillWritesCopy: when the dest cannot be recorded, the dated copy is still written.
func TestPlanMirrorFailedRecordStillWritesCopy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	writeMirrorDraft(t, filepath.Join(cfg, "state", "harness"), "plans-scratch", "x") // the scratch dir cannot be created
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "funny-random-words.md", planTemplate)
	mirrorRun(t, "unrecordable mirror", "Write", cfg, draft)
	dated, err := filepath.Glob(filepath.Join(cfg, "plans", "2*.md"))
	if err != nil || len(dated) != 1 {
		t.Fatalf("dated copies = %v (err %v), want exactly one", dated, err)
	}
	wantMirrorFile(t, dated[0], planTemplate, "dated copy after an unrecordable mirror")
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
