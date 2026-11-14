package docs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

func TestReminderDoesNotCreateSessionLocks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, session := range []string{"empty", "partial"} {
		if session == "partial" {
			recordPartials(session, "plan", "", []string{"task"}, &bytes.Buffer{})
		}
		Reminder(&core.Input{SessionID: session})
		if _, err := os.Stat(partialFile(session) + ".lock"); !os.IsNotExist(err) {
			t.Errorf("%s left per-session lock: %v", session, err)
		}
	}
}

func TestMirrorConcurrentFirstWriteUsesOneDestination(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "concurrent.md", planTemplate)
	in := core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, planTemplate)))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			<-start
			Mirror(in, &bytes.Buffer{})
		})
	}
	close(start)
	wg.Wait()
	files, err := filepath.Glob(filepath.Join(cfg, "plans", dateStamp()+"-*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("destinations = %v, %v; want exactly one", files, err)
	}
}

func TestMirrorRecordFailureDropsClaim(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "unrecordable.md", planTemplate)
	if err := os.MkdirAll(plans.DestRecordPath(filepath.Base(draft)), 0o700); err != nil {
		t.Fatal(err)
	}
	in := core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, planTemplate)))
	var stderr bytes.Buffer
	Mirror(in, &stderr)
	files, err := filepath.Glob(filepath.Join(cfg, "plans", dateStamp()+"-*.md"))
	if err != nil || len(files) != 0 || !strings.Contains(stderr.String(), "plan-mirror:") {
		t.Fatalf("failed record left mirrors %v, err %v, stderr %q", files, err, stderr.String())
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

// TestPlanTitleSkipsFencedHeading: a "#" line inside a fenced code block is not an AST heading, so planTitle
// correctly falls through to the real heading after the fence (fixed from a raw line scan).
func TestPlanTitleSkipsFencedHeading(t *testing.T) {
	plan := "```\n# Not A Title\n```\n\n# Real Title\n\nbody\n"
	if got, want := planTitle([]byte(plan)), "real-title"; got != want {
		t.Errorf("planTitle(fenced) = %q, want %q", got, want)
	}
}

// TestPlanTemplateRejectsFencedOnlyHeading: Gate now parses the mdsmith AST, so a heading that
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
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, content))), &stderr)
	if code != core.ExitBlock {
		t.Fatalf("fenced-only heading = %d, want ExitBlock (fence content must not satisfy the template check)", code)
	}
	if !strings.Contains(stderr.String(), "## Tasks") {
		t.Fatalf("denial message = %q, want it to name the missing heading", &stderr)
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

// TestSavePlanFromNestedConfigRepoFilesUntagged: a cwd in a git repo nested in the config dir (memory/<repo>) is
// the harness itself, so the plan lands untagged under plans/, not under plans/memory/.
func TestSavePlanFromNestedConfigRepoFilesUntagged(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	nested := hosttest.RepoAt(t, filepath.Join(cfg, "memory"))
	cwd := filepath.Join(nested, "claude-code")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := Save(core.ParseInput(strings.NewReader(planPayload(t, cwd, "# Plan: Nested\n\nsteps\n", ""))), &bytes.Buffer{}); code != core.ExitProceed {
		t.Fatalf("Save must never block, got %d", code)
	}
	want := []string{time.Now().Format("2006-01-02") + "-nested.md"}
	if got := savedPlans(t, cfg); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("saved = %v, want %v", got, want)
	}
}

// TestPlanMirrorUntaggedWhenCWDIsMemoryStoreRepo: a cwd in memory/<repo> is the harness, so the copy lands in plans/.
func TestPlanMirrorUntaggedWhenCWDIsMemoryStoreRepo(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	store := hosttest.RepoAt(t, filepath.Join(cfg, "memory", "claude-code"))
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "store-words.md", planTemplate)
	mirrorRun(t, "Mirror", "Write", store, draft)
	wantMirrorFile(t, filepath.Join(cfg, "plans", dateStamp()+"-widget-refactor.md"), planTemplate, "mirrored file at")
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

// A draft written through a symlink to the config dir is still an undated draft in <config>/plans.
func TestIsUndatedDraftThroughSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", real)
	if !isUndatedDraft(filepath.Join(link, "plans", "draft.md")) {
		t.Error("symlink-spelled draft not recognized")
	}
}

// holdLock holds path's file lock until the test ends and returns once it is held.
func holdLock(t *testing.T, path string) {
	t.Helper()
	held, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = paths.WithFileLock(path, func() error { close(held); <-stop; return nil })
	}()
	<-held
	t.Cleanup(func() { close(stop); <-done })
}

// A hook that finds its lock held gives up after hookLockWait and degrades as on any lock error, not after 2 minutes.
func TestHooksDegradeWhenLockHeld(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	old := hookLockWait
	t.Cleanup(func() { hookLockWait = old })
	recordPartials("held", "plan", "", []string{"task"}, &bytes.Buffer{})
	holdLock(t, partialsLock())
	draft := writeMirrorDraft(t, filepath.Join(cfg, "plans"), "held.md", planTemplate)
	holdLock(t, filepath.Join(paths.StateDir(), "plan-mirror"))
	hookLockWait = 20 * time.Millisecond

	res := Reminder(&core.Input{SessionID: "held"})
	if !strings.Contains(res.SystemMessage, "kept for the next Stop") {
		t.Errorf("Reminder SystemMessage = %q, want the kept-record notice", res.SystemMessage)
	}
	if _, err := os.Stat(partialFile("held")); err != nil {
		t.Errorf("Reminder dropped the partial record: %v", err)
	}
	var tick bytes.Buffer
	recordPartials("held", "plan", "", []string{"task2"}, &tick)
	if !strings.Contains(tick.String(), "plan-tick:") || !strings.Contains(tick.String(), paths.ErrLockTimeout.Error()) {
		t.Errorf("recordPartials stderr = %q, want a plan-tick lock-timeout line", tick.String())
	}
	var mirror bytes.Buffer
	in := core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, planTemplate)))
	if code := Mirror(in, &mirror); code != core.ExitProceed || !strings.Contains(mirror.String(), "plan-mirror:") {
		t.Errorf("Mirror = %d, stderr %q, want proceed with a plan-mirror line", code, mirror.String())
	}
}
