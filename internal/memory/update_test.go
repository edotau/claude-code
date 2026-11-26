package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	write(t, path, strings.Join(lines, "\n"))
	return path
}

func TestTagBlock_EmptyTranscriptNoBlock(t *testing.T) {
	t.Parallel()
	path := writeTranscript(t, "")
	if got := tagBlock(path, "", false); got != "" {
		t.Fatalf("tagBlock on an empty transcript = %q, want empty", got)
	}
}

func TestTagBlock_MissingTranscriptNoBlock(t *testing.T) {
	t.Parallel()
	if got := tagBlock(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), "", false); got != "" {
		t.Fatalf("tagBlock on a missing transcript = %q, want empty", got)
	}
}

func TestAgentUpdatePrompt_UsesReference(t *testing.T) {
	transcript := writeTranscript(t, "")
	got, err := agentUpdatePrompt(transcript, "", "", false)
	if err != nil {
		t.Fatalf("agentUpdatePrompt: %v", err)
	}
	ref := assets.MemoryUpdateProcedure()
	if ref == "" || !strings.Contains(got, ref) {
		t.Fatalf("update prompt missing its embedded procedure: %q", got)
	}
	if !strings.Contains(got, transcript) {
		t.Fatalf("update prompt missing transcript path: %q", got)
	}
}

func TestAgentUpdatePrompt_WithCWDAppendsGitActivity(t *testing.T) {
	cwd := t.TempDir()
	hosttest.In(t, cwd, "init", "-q")
	write(t, filepath.Join(cwd, "f.txt"), "x")
	hosttest.In(t, cwd, "add", "f.txt")
	hosttest.In(t, cwd, "commit", "-q", "-m", "seed commit")

	ts := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	path := writeTranscript(t, `{"timestamp":"`+ts+`"}`)
	got, err := agentUpdatePrompt(path, "", cwd, false)
	if err != nil {
		t.Fatalf("agentUpdatePrompt: %v", err)
	}
	if !strings.Contains(got, "git activity") {
		t.Errorf("prompt missing git activity block:\n%s", got)
	}
	if !strings.Contains(got, "seed commit") {
		t.Errorf("prompt missing the seeded commit:\n%s", got)
	}
}

func TestInitNoOverwrite(t *testing.T) {
	tempConfig(t)
	_, err := Init()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range BankFiles() {
		if _, err := os.Stat(Path(name)); err != nil {
			t.Errorf("missing %s", name)
		}
	}
	write(t, Path("activeContext.md"), "mine\n")
	_ = os.Remove(Path("progress.md"))
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Path("activeContext.md")); string(b) != "mine\n" {
		t.Error("Init overwrote an existing file")
	}
	if !bankExists() || !fileExists(Path("progress.md")) {
		t.Error("Init did not re-seed a missing file")
	}
}

func TestSplitBlocksFenceAware(t *testing.T) {
	t.Parallel()
	body := "# Conventions\n> note\n\n- first rule\n  - sub\n```\n- not a block\n## nor this\n```\n- second rule\n## Heading\ntext"
	header, blocks := entryBlocks("conventions.md", body)
	if header != "# Conventions\n> note" || len(blocks) != 3 || !strings.Contains(blocks[0], "- not a block") {
		t.Fatalf("header %q blocks %q", header, blocks)
	}
	_, hb := entryBlocks("decisionLog.md", "# D\n## a\n- bullet stays\n~~~\n## fenced\n~~~\n## b")
	if len(hb) != 2 || !strings.Contains(hb[0], "## fenced") {
		t.Errorf("heading blocks %q", hb)
	}
	if got := rejoin(header, blocks[:1]); !strings.HasPrefix(got, "# Conventions\n> note\n\n- first rule") {
		t.Errorf("rejoin %q", got)
	}
}

func TestSearchBanksRanking(t *testing.T) {
	tempConfig(t)
	write(t, Path("decisionLog.md"), "# D\n\n## 2026-01-01 — router retries on 429\n\nwhy: rate limits\n\n## 2026-01-02 — unrelated choice\n\nthe router is mentioned once\n")
	write(t, filepath.Join(paths.MemoryStoreDir(), "conventions.md"), "# C\n\n- keep commits small\n")
	write(t, paths.MemorySub("archive", "sessionHistory-2025-12.md"), "# old\n\n## 2025-12-01 — 429 storm\n")
	hits := searchBanks("the router 429", 0)
	if len(hits) != 3 || !strings.Contains(hits[0].Block, "retries on 429") || hits[0].Bank != "home" {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].Title() != "2026-01-01 — router retries on 429" {
		t.Errorf("title %q", hits[0].Title())
	}
	if g := searchBanks("commits", 5); len(g) != 1 || g[0].Bank != "home" {
		t.Errorf("global hit %+v", g)
	}
	if searchBanks("the and of", 5) != nil {
		t.Error("all-stopword query must return nothing")
	}
}

func TestRotateAndSettle(t *testing.T) {
	tempConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("# Session History\n\n> newest first\n")
	for i := 35; i >= 1; i-- {
		fmt.Fprintf(&sb, "\n## entry %02d\n\n- did %d\n", i, i)
	}
	write(t, Path("sessionHistory.md"), sb.String())
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	rotated, err := settleBank(now)
	if err != nil || rotated != 5 {
		t.Fatalf("rotated %d err %v", rotated, err)
	}
	assertRotated(t)
	if n, _ := rotateSessionHistory(now); n != 0 {
		t.Error("rotation under cap must be a no-op")
	}
}

// assertRotated checks the history kept its newest entries and the oldest went to the archive.
func assertRotated(t *testing.T) {
	t.Helper()
	_, kept := splitHeadingBlocks(mustRead(t, Path("sessionHistory.md")))
	arch := mustRead(t, paths.MemorySub("archive", "sessionHistory-2026-09.md"))
	if len(kept) != sessionHistoryMaxEntries || !strings.HasPrefix(kept[0], "## entry 35") || !strings.Contains(arch, "## entry 01") || strings.Contains(arch, "## entry 06") {
		t.Errorf("kept %d first %q archive %q", len(kept), kept[0], arch)
	}
}

func TestCapCharsBelowMarkerYieldsEmpty(t *testing.T) {
	if got := capChars("aaaa\nbbbb", 3); got != "" {
		t.Errorf("a cap below the marker length yields empty, got %q", got)
	}
}

func TestUpdateRecordsOutcome(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	_, _ = Init()
	ok := func(ctx context.Context, _, _ string) (string, error) {
		if !slices.Contains(ChildEnv(ctx), UpdateChildEnv+"=1") {
			return "fake", errors.New("update child env not passed")
		}
		time.Sleep(10 * time.Millisecond)
		f, err := os.OpenFile(Path("sessionHistory.md"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return "fake", err
		}
		defer f.Close()
		_, err = f.WriteString("\n## 2026-09-16 — updated-by-child\n")
		return "fake", err
	}
	_ = writeUpdateResult("11111111-1111-1111-1111-111111111111", UpdateResult{Status: updatePending, Time: time.Now(), Log: "/l.log"})

	if err := update(updateOptions{Run: ok, CWD: root, Session: "11111111-1111-1111-1111-111111111111"}); err != nil {
		t.Fatal(err)
	}
	r, _ := readUpdateResult("11111111-1111-1111-1111-111111111111")
	if r.Status != updateOK || !r.BankUpdated || r.Log != "/l.log" || r.needsAttention(time.Now()) {
		t.Errorf("success result %+v", r)
	}
	if st, err := StoreRepo().Status("*"); err != nil || len(st) != 0 {
		t.Errorf("update did not settle the bank: %+v, %v", st, err)
	}
	assertUpdateFailure(t, root)
	stale := UpdateResult{Status: updatePending, Time: time.Now().Add(-updateGrace - time.Minute)}
	if !stale.needsAttention(time.Now()) {
		t.Error("a pending result past the grace window needs attention")
	}
}

// assertUpdateFailure: a failing agent errors and records a failed result.
func assertUpdateFailure(t *testing.T, root string) {
	t.Helper()
	bad := func(context.Context, string, string) (string, error) { return "fake", errors.New("exit status 1") }
	if err := update(updateOptions{Run: bad, CWD: root, Session: "s2"}); err == nil {
		t.Error("failing agent must return an error")
	}
	if r, _ := readUpdateResult("s2"); r.Status != updateFailed || r.BankUpdated || !r.needsAttention(time.Now()) {
		t.Errorf("failure result %+v", r)
	}
}

// TestUpdateReceiptRoundTripsClassAndFix checks typed receipts, runner reports and owner-only retry gating.
func TestUpdateReceiptRoundTripsClassAndFix(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	id := "22222222-2222-2222-2222-222222222222"
	now := time.Now()
	assertReceiptFields(t, id, now)
	assertOwnerRetry(t, id, now, JobClassAuth)
	if err := writeUpdateResult(id, UpdateResult{Status: updateFailed, Time: now}); err != nil {
		t.Fatal(err)
	}
	got, ok := readUpdateResult(id)
	if !ok || got.Class != "" || got.Fix != "" || got.Ran != "" || got.Local {
		t.Fatalf("legacy receipt: %+v, ok=%t", got, ok)
	}
	assertReportedUpdate(t, id, JobClassBudget)
	b := UpdateResult{Class: JobClassBudget, Fix: "claude-code memory update --session 'x'"}
	if got := b.fixNote(); !strings.HasSuffix(got, "("+budgetFixGuidance+")") {
		t.Fatalf("budget fix note: %q", got)
	}
}

// assertReceiptFields checks that all typed keys survive a write and read.
func assertReceiptFields(t *testing.T, id string, now time.Time) {
	t.Helper()
	want := UpdateResult{Status: updateFailed, Time: now, Log: "/tmp/update.log", Class: JobClassAuth,
		Fix: "claude-code databricks login --profile dev", Ran: "claude·dev·sonnet", Local: true}
	if err := writeUpdateResult(id, want); err != nil {
		t.Fatal(err)
	}
	got, ok := readUpdateResult(id)
	if !ok || got.Class != want.Class || got.Fix != want.Fix || got.Ran != want.Ran || !got.Local {
		t.Fatalf("typed receipt: %+v, ok=%t", got, ok)
	}
}

// assertOwnerRetry checks that only new edits can rearm an owner-fix failure.
func assertOwnerRetry(t *testing.T, id string, now time.Time, class string) {
	t.Helper()
	c := sweepCand{id: id, path: "transcript", mtime: now.Add(-time.Nanosecond)}
	count := func(string, string) int { return updateMinEdits }
	if why := sweepWhy(&c, now, count); why != "" {
		t.Errorf("owner failure without stamp retried: %q", why)
	}
	if err := writeStampAttempts(stampPath(id), now, "catchup", updateMinEdits, 1); err != nil {
		t.Fatal(err)
	}
	if why := sweepWhy(&c, now, count); why != "" {
		t.Errorf("owner failure retried: %q", why)
	}
	if !ownerFixRequired(UpdateResult{Status: updateFailed, Class: class}) {
		t.Error("owner failure not classified")
	}
	if why := sweepWhy(&c, now.Add(2*catchupRetry), count); why != "" {
		t.Errorf("second retry of owner failure: %q", why)
	}
	count = func(string, string) int { return updateMinEdits + 1 }
	c.mtime = now.Add(time.Second)
	if why := sweepWhy(&c, now, count); !strings.Contains(why, "edits") {
		t.Errorf("fresh edits should save: %q", why)
	}
}

// assertReportedUpdate checks an injected runner's failure report reaches the persisted receipt.
func assertReportedUpdate(t *testing.T, id, class string) {
	t.Helper()
	run := func(ctx context.Context, _, _ string) (string, error) {
		ReportJob(ctx, JobResult{Agent: "claude", Profile: "dev", Model: "sonnet", Class: class,
			Fix: "claude-code memory update --catchup", Local: true})
		return "claude", errors.New("dispatch tree budget spent")
	}
	if err := update(updateOptions{Run: run, Session: id}); err == nil {
		t.Fatal("expected failing update")
	}
	got, ok := readUpdateResult(id)
	if !ok || got.Status != updateFailed || got.Class != class || got.Fix != "claude-code memory update --catchup" || got.Ran != "claude·dev·sonnet" || !got.Local {
		t.Fatalf("runner report: %+v, ok=%t", got, ok)
	}
}

func TestNativeTopicsJoinSearch(t *testing.T) {
	tempConfig(t)
	if got := paths.ProjectSlug("/Users/a/.claude"); got != "-Users-a--claude" {
		t.Errorf("ProjectSlug = %q", got)
	}
	write(t, Path("gw.md"), "---\nname: gateway\ndescription: home gateway over ssh\n---\n\nRun the status script.\n")
	write(t, NativeIndex(), "- [gateway](gw.md) — home gateway over ssh status\n")
	write(t, Path("loose.md"), "gateway ssh status without frontmatter\n")
	hits := searchBanks("gateway ssh status", 0)
	if len(hits) != 1 || hits[0].Bank != "native" || hits[0].File != "gw.md" || hits[0].Matched != 3 {
		t.Fatalf("want only the frontmatter topic, matching 3 terms; got %+v", hits)
	}
	if !strings.HasPrefix(hits[0].Block, "gateway — home gateway over ssh\n") {
		t.Errorf("topic block %q", hits[0].Block)
	}
}

func TestSeenSetTracksInjectedBlocks(t *testing.T) {
	tempConfig(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "conventions.md"), "# C\n\n- injected rule\n  continues here\n- other rule\n")
	resetSeen("11111111-1111-1111-1111-111111111111", "<!-- surface -->\n- injected   rule\ncontinues here\n")
	seen := seenBlocks("11111111-1111-1111-1111-111111111111")
	if !seen[blockKey("- injected rule\n  continues here")] || seen[blockKey("- other rule")] || len(seen) != 1 {
		t.Fatalf("seen after reset: %v", seen)
	}
	markSeen("11111111-1111-1111-1111-111111111111", []SearchHit{{Block: "- other rule"}}, false)
	if !seenBlocks("11111111-1111-1111-1111-111111111111")[blockKey("- other rule")] {
		t.Error("markSeen did not record the hit")
	}
	resetSeen("11111111-1111-1111-1111-111111111111", "")
	if len(seenBlocks("11111111-1111-1111-1111-111111111111")) != 0 {
		t.Error("a SessionStart (e.g. after compact) must clear what recall injected")
	}
	for _, bad := range []string{"", ".."} {
		markSeen(bad, []SearchHit{{Block: "x"}}, false)
		if len(seenBlocks(bad)) != 0 {
			t.Errorf("session id %q must be a no-op", bad)
		}
	}
}

// hitContaining returns the first hit whose block contains sub (the last when several do, as the old scan did).
func hitContaining(hits []SearchHit, sub string) (found SearchHit, ok bool) {
	for _, h := range hits {
		if strings.Contains(h.Block, sub) {
			found, ok = h, true
		}
	}
	return found, ok
}

// An exact "agent" outranks the substring-only match in "subagent"; hyphenated terms match both parts.
func TestCompoundMatchFindsHyphenatedAndSubstringTerms(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	err := EnsureBank("")
	if err != nil {
		t.Fatal(err)
	}
	write(t, Path("decisionLog.md"), "# Decisions\n\n"+
		"## compound candidate\nSubagent routing stays on the local classifier\n\n"+
		"## exact candidate\nAgent routing stays on the local classifier\n\n"+
		"## unrelated entry\nnothing to do with either term\n")

	hits := searchBanks("agent routing", 0)
	compound, compoundSeen := hitContaining(hits, "Subagent routing")
	exact, exactSeen := hitContaining(hits, "Agent routing")
	compoundScore, compoundMatched := compound.Score, compound.Matched
	exactScore, exactMatched := exact.Score, exact.Matched
	if !compoundSeen {
		t.Fatalf("\"agent routing\" missed the compound (subagent) block: %#v", hits)
	}
	if !exactSeen {
		t.Fatalf("\"agent routing\" missed the exact block: %#v", hits)
	}
	// Matched must credit both terms, "agent" only via the "subagent" substring.
	if compoundMatched != 2 {
		t.Errorf("compound block matched %d terms, want 2 (agent via substring, routing exact): %#v", compoundMatched, hits)
	}
	if exactMatched != 2 {
		t.Errorf("exact block matched %d terms, want 2: %#v", exactMatched, hits)
	}
	if exactScore <= compoundScore {
		t.Errorf("exact match score %v did not outrank the compound match score %v", exactScore, compoundScore)
	}

	assertHyphenatedMatchesBoth(t)
}

// assertHyphenatedMatchesBoth: the hyphenated query "agent-routing" matches both the compound and the exact block.
func assertHyphenatedMatchesBoth(t *testing.T) {
	t.Helper()
	hyphen := searchBanks("agent-routing", 0)
	var hyphenHitsCompound, hyphenHitsExact bool
	for _, h := range hyphen {
		if strings.Contains(h.Block, "Subagent routing") {
			hyphenHitsCompound = true
		}
		if strings.Contains(h.Block, "Agent routing") {
			hyphenHitsExact = true
		}
	}
	if !hyphenHitsCompound || !hyphenHitsExact {
		t.Fatalf("hyphenated query \"agent-routing\" did not match both parts: %#v", hyphen)
	}
}

// LiveTranscript prefers projects/<slug(cwd)>/<id>.jsonl, falls back to any project, and is "" otherwise.
func TestLiveTranscript(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	cwd := t.TempDir()
	write := func(slug, id string) string {
		p := filepath.Join(cfg, "projects", slug, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exact := write(paths.ProjectSlug(cwd), "s1")
	write("-other-project", "s1")
	other := write("-other-project", "s2")
	if got := LiveTranscript(cwd, "s1"); got != exact {
		t.Errorf("exact: got %q, want %q", got, exact)
	}
	if got := LiveTranscript(cwd, "s1.jsonl"); got != exact {
		t.Errorf("exact with .jsonl: got %q, want %q", got, exact)
	}
	if got := LiveTranscript(cwd, "s2"); got != other {
		t.Errorf("glob fallback: got %q, want %q", got, other)
	}
	for _, id := range []string{"", "missing"} {
		if got := LiveTranscript(cwd, id); got != "" {
			t.Errorf("id %q: got %q, want \"\"", id, got)
		}
	}
}

var embeddedProcedures = []func() string{assets.MemoryInitProcedure, assets.MemoryReviewProcedure,
	assets.MemoryPromoteProcedure, assets.MemoryUpdateProcedure, assets.MemoryDreamProcedure}

func TestReferenceEmbeddedVerbsNonEmpty(t *testing.T) {
	for i, read := range embeddedProcedures {
		got := read()
		if strings.TrimSpace(got) == "" {
			t.Errorf("procedure %d = empty, want a non-empty procedure", i)
		}
	}
}

func TestProcedureDispatchIsTyped(t *testing.T) {
	src, err := os.ReadFile("verbs.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "func Reference(") {
		t.Fatal("generic procedure dispatch remains")
	}
}

func TestProcedureReadersPreserveReferenceWhitespace(t *testing.T) {
	for i, read := range embeddedProcedures {
		if got := read(); got == "" || strings.TrimSpace(got) != got {
			t.Errorf("procedure %d lost Reference's trimmed, nonempty contract", i)
		}
	}
}

// TestAgentDreamPrompt_UsesReference: the embedded dream procedure lands in the built prompt.
func TestAgentDreamPrompt_UsesReference(t *testing.T) {
	got, err := agentDreamPrompt([]string{"/m/a.md"})
	if err != nil {
		t.Fatalf("agentDreamPrompt: %v", err)
	}
	ref := dreamProcedure(dreamTyped())
	if !strings.Contains(got, ref) {
		t.Errorf("prompt missing the embedded dream reference:\n%s", got)
	}
	if !strings.Contains(got, "- /m/a.md") {
		t.Errorf("prompt missing the changed-file list:\n%s", got)
	}
}

// Regression: on a fresh store Dream re-took its own bank lock inside settleBank and hung.
func TestDreamFreshStoreRunsWithoutDeadlock(t *testing.T) {
	clearChildEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ran := false
	run := func(ctx context.Context, _, _ string) (string, error) {
		ran = true
		AnswerJob(ctx, `{"ops":[]}`)
		return "stub", nil
	}
	done := make(chan error, 1)
	go func() { done <- dream(dreamOptions{CWD: t.TempDir(), Run: run}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Dream error = %v, want nil", err)
		}
		if !ran {
			t.Error("agent did not run despite an embedded reference")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Dream deadlocked on a fresh store")
	}
}

func TestMergeFileListsKeepsUnstagedPaths(t *testing.T) {
	got := mergeFileLists("", " M internal/foo.go\n D bar.go\n?? new.go\nM  staged.go\n")
	want := []string{"internal/foo.go", "bar.go", "new.go", "staged.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mergeFileLists = %v, want %v", got, want)
	}
}

func TestDreamFailedRunRetriesAfterBackoff(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, string, string) (string, error) { return "x", errors.New("boom") }
	if err := dream(dreamOptions{CWD: root, Run: fail}); err == nil {
		t.Fatal("failed agent run must return an error")
	}
	now := time.Now()
	if dreamDue(now) {
		t.Error("a failed dream is due again at once, so every session start would rerun it")
	}
	if !dreamDue(now.Add(dreamRetry + time.Minute)) {
		t.Error("a failed dream is not due after its backoff")
	}
}

// TestHistoricalUpdatePromptAndReport: an older session's prompt forbids current state, and an edit to it is reported.
func TestHistoricalUpdatePromptAndReport(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	write(t, Path("activeContext.md"), "live focus\n")
	var prompt string
	run := func(_ context.Context, p, _ string) (string, error) {
		prompt = p
		write(t, Path("activeContext.md"), "(NOW, newest) stale\n")
		return "fake", nil
	}
	var out bytes.Buffer
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, tr, "{}\n")
	if err := update(updateOptions{Run: run, Transcript: tr, CWD: root, Session: "h1", Historical: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "OLDER session") || !strings.Contains(prompt, "never write NOW") {
		t.Errorf("historical prompt lacks the rule:\n%s", prompt)
	}
	if !strings.Contains(out.String(), "changed activeContext.md") {
		t.Errorf("edit to activeContext.md not reported: %q", out.String())
	}
	if err := update(updateOptions{Run: run, Transcript: tr, CWD: root, Session: "h2", Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "OLDER session") {
		t.Error("a normal update must not carry the historical rule")
	}
}

// TestMergeFileListsRenameKeepsNewPath: a porcelain rename line contributes only the new path.
func TestMergeFileListsRenameKeepsNewPath(t *testing.T) {
	t.Parallel()
	got := mergeFileLists("", "R  old.go -> new.go\n")
	if len(got) != 1 || got[0] != "new.go" {
		t.Fatalf("got %q, want [new.go]", got)
	}
}

// Dream hands its child markers to the runner through ctx and never mutates the parent process env.
func TestDreamPassesChildEnvWithoutMutatingParent(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	var got []string
	run := func(ctx context.Context, _, _ string) (string, error) {
		got = ChildEnv(ctx)
		AnswerJob(ctx, `{"ops":[]}`)
		return "stub", nil
	}
	if err := dream(dreamOptions{CWD: root, Run: run}); err != nil {
		t.Fatal(err)
	}
	want := []string{UpdateChildEnv + "=1", dreamChildEnv + "=1"}
	if !slices.Equal(got, want) {
		t.Errorf("child env = %q, want %q", got, want)
	}
	if IsUpdateChild() || isDreamChild() {
		t.Errorf("parent env mutated: update=%v dream=%v", IsUpdateChild(), isDreamChild())
	}
}

// assistantLine builds one JSONL record shaped like a real Claude Code transcript assistant turn.
func assistantLine(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` +
		jsonQuote(text) + `}]}}`
}

func userLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"text","text":` + jsonQuote(text) + `}]}}`
}

func toolResultLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","text":` +
		jsonQuote(text) + `}]}}`
}

func systemLine(text string) string {
	return `{"type":"system","message":{"role":"system","content":[{"type":"text","text":` + jsonQuote(text) + `}]}}`
}

// jsonQuote escapes text into a JSON string literal without pulling in encoding/json for test fixtures.
func jsonQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestExtractTags_AssistantAllFourKinds(t *testing.T) {
	t.Parallel()
	transcript := strings.Join([]string{
		assistantLine("DECISION: use bbolt — rejected sqlite because no cgo\nBUG: update hangs → missing timeout\nNEXT: add retry\nNOTE: vault key lives in keyring"),
	}, "\n")
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	wantKinds := []string{"DECISION", "BUG", "NEXT", "NOTE"}
	if len(tags) != 4 {
		t.Fatalf("got %d tags, want 4: %+v", len(tags), tags)
	}
	for i, want := range wantKinds {
		if tags[i].Kind != want {
			t.Errorf("tag %d kind = %q, want %q", i, tags[i].Kind, want)
		}
	}
}

func TestExtractTags_UserToolSystemExcluded(t *testing.T) {
	t.Parallel()
	transcript := strings.Join([]string{
		userLine("DECISION: this came from the user and must not be filed"),
		toolResultLine("DECISION: this came from a tool result and must not be filed"),
		systemLine("DECISION: this came from a system message and must not be filed"),
	}, "\n")
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("got %d tags from non-assistant lines, want 0: %+v", len(tags), tags)
	}
}

func TestExtractTags_OptionalBulletPrefix(t *testing.T) {
	t.Parallel()
	transcript := assistantLine("- DECISION: bulleted tag still counts")
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Kind != "DECISION" || tags[0].Text != "bulleted tag still counts" {
		t.Fatalf("got %+v", tags)
	}
}

func TestExtractTags_ExactRepeatsDeduped(t *testing.T) {
	t.Parallel()
	transcript := strings.Join([]string{
		assistantLine("NOTE: same fact twice"),
		assistantLine("NOTE: same fact twice"),
	}, "\n")
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("got %d tags, want 1 after dedup: %+v", len(tags), tags)
	}
}

func TestExtractTags_OverLongCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	transcript := assistantLine("NOTE: " + long)
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("got %d tags, want 1: %+v", len(tags), tags)
	}
	if len(tags[0].Text) != tagMaxLen {
		t.Fatalf("tag text len = %d, want %d", len(tags[0].Text), tagMaxLen)
	}
}

func TestExtractTags_OverCountKeepsNewest(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range tagMaxCount + 5 {
		lines = append(lines, assistantLine("NOTE: fact "+string(rune('a'+i))))
	}
	transcript := strings.Join(lines, "\n")
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != tagMaxCount {
		t.Fatalf("got %d tags, want %d", len(tags), tagMaxCount)
	}
	// the oldest 5 ("fact a".."fact e") must have been dropped, keeping the newest run.
	if tags[0].Text != "fact "+string(rune('a'+5)) {
		t.Fatalf("first kept tag = %q, want the 6th written", tags[0].Text)
	}
}

func TestExtractTags_MalformedLineSkipped(t *testing.T) {
	t.Parallel()
	transcript := strings.Join([]string{
		"{not valid json",
		assistantLine("NOTE: survives the bad line before it"),
	}, "\n")
	tags, err := extractTagsExcept(strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Text != "survives the bad line before it" {
		t.Fatalf("got %+v", tags)
	}
}

func TestExtractTags_EmptyTranscript(t *testing.T) {
	t.Parallel()
	tags, err := extractTagsExcept(strings.NewReader(""), nil)
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("got %d tags from an empty transcript, want 0", len(tags))
	}
}

// warnChanged lists every changed file in sorted order, every run, so map iteration order never leaks out.
func TestWarnChangedSortedOrder(t *testing.T) {
	runBank(t, func(t *testing.T) { testWarnChangedSortedOrder(t) })
}

func testWarnChangedSortedOrder(t *testing.T) {
	names := []string{"progress.md", "activeContext.md", "projectContext.md", "conventions.md", "b.md", "a.md"}
	snap := snapshotFiles(names)
	for _, n := range names {
		write(t, Path(n), "changed\n")
	}
	var want strings.Builder
	for _, n := range []string{"a.md", "activeContext.md", "b.md", "conventions.md", "progress.md", "projectContext.md"} {
		fmt.Fprintf(&want, "memory update: WARNING: historical run changed %s (not restored)\n", n)
	}
	for range 20 {
		var out bytes.Buffer
		warnChanged(&out, snap)
		if out.String() != want.String() {
			t.Fatalf("warnChanged order:\n%s\nwant:\n%s", out.String(), want.String())
		}
	}
}

// Porcelain's C-quoted paths (space, quote, backslash, octal UTF-8 bytes) come back as the real file names.
func TestMergeFileListsUnquotesPorcelain(t *testing.T) {
	porcelain := " M \"a b.md\"\n?? \"q\\\"x.md\"\nM  \"back\\\\slash.md\"\n?? \"caf\\303\\251.md\"\n" +
		"R  \"old name.md\" -> \"new name.md\"\n"
	got := mergeFileLists("\"caf\\303\\251.md\"\n", porcelain)
	want := []string{"café.md", "a b.md", `q"x.md`, `back\slash.md`, "new name.md"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mergeFileLists = %q, want %q", got, want)
	}
}

func TestUpdatePromptNamesEverySixPathsOfBothBanks(t *testing.T) {
	runRoot(t, func(t *testing.T, root string) {
		if _, err := Init(); err != nil {
			t.Fatal(err)
		}
		p, err := agentUpdatePrompt("", "", root, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range bankFiles {
			if !strings.Contains(p, Path(name)) {
				t.Errorf("prompt lacks %s", Path(name))
			}
		}
	})
}

// TestTagBlockTypedEventBeatsLegacyText: an event the Stop hook recorded and the same transcript line file once.
func TestTagBlockTypedEventBeatsLegacyText(t *testing.T) {
	tempConfig(t)
	path := filepath.Join(t.TempDir(), "s9.jsonl")
	block := `{"type":"text","text":"DECISION: use A\nNOTE: b"}`
	write(t, path, `{"type":"assistant","message":{"role":"assistant","content":[`+block+`]}}`)
	write(t, TagEventsPath("s9"), `{"event":"decision","text":"use A","at":"x"}`+"\n")
	got := tagBlock(path, "other", false)
	if strings.Count(got, "use A") != 1 || !strings.Contains(got, "NOTE: b") {
		t.Fatalf("tagBlock with another session id = %q, want only the transcript tags once", got)
	}
	got = tagBlock(path, "s9", false)
	if n := strings.Count(got, "use A"); n != 1 || !strings.Contains(got, "NOTE: b") {
		t.Fatalf("tagBlock = %q, want use A once and the legacy-only NOTE", got)
	}
}

// TestMergeTagsCapDropsLegacyBeforeTyped: over the cap the typed-only events stay.
func TestMergeTagsCapDropsLegacyBeforeTyped(t *testing.T) {
	typed := []tag{{Kind: "DECISION", Text: "typed"}}
	var legacy []tag
	for i := range tagMaxCount + 5 {
		legacy = append(legacy, tag{Kind: "NOTE", Text: strconv.Itoa(i)})
	}
	got := mergeTags(typed, legacy)
	if len(got) != tagMaxCount || got[0] != typed[0] {
		t.Fatalf("mergeTags len %d first %v, want %d and the typed event", len(got), got[0], tagMaxCount)
	}
}

// TestMergeTagsKeepsLegacyOnlyTagWhenTypedFull: a decision the recorder missed survives a full typed log.
func TestMergeTagsKeepsLegacyOnlyTagWhenTypedFull(t *testing.T) {
	var typed []tag
	for i := range tagMaxCount + 1 {
		typed = append(typed, tag{Kind: "NOTE", Text: strconv.Itoa(i)})
	}
	missed := tag{Kind: "DECISION", Text: "missed"}
	got := mergeTags(typed, []tag{missed})
	if len(got) != tagMaxCount || got[len(got)-1] != missed {
		t.Fatalf("mergeTags len %d, want %d ending in the legacy-only decision", len(got), tagMaxCount)
	}
}

// TestTagBlockHistoricalFiltersBeforeTheCap: legacy notes never take slots from decisions in a historical update.
func TestTagBlockHistoricalFiltersBeforeTheCap(t *testing.T) {
	tempConfig(t)
	var ev, notes strings.Builder
	for i := range tagMaxCount {
		ev.WriteString(`{"event":"decision","text":"d` + strconv.Itoa(i) + `","at":"x"}` + "\n")
	}
	for i := range 8 {
		notes.WriteString("NOTE: n" + strconv.Itoa(i) + `\n`)
	}
	path := filepath.Join(t.TempDir(), "s8.jsonl")
	write(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+notes.String()+`"}]}}`)
	write(t, TagEventsPath("s8"), ev.String())
	if got := tagBlock(path, "s8", true); !strings.Contains(got, "DECISION: d0") || strings.Contains(got, "NOTE") {
		t.Fatalf("historical tagBlock = %q, want all decisions and no notes", got)
	}
}

// TestTagOpsTargetBankFiles: every Go-filed tag op names a file the store routes, so a tag cannot void the batch.
func TestTagOpsTargetBankFiles(t *testing.T) {
	tempConfig(t)
	write(t, TagEventsPath("s9"), `{"event":"next","text":"n","at":"x"}`+"\n"+
		`{"event":"bug","text":"b","at":"x"}`+"\n"+`{"event":"note","text":"keep","at":"x"}`+"\n")
	ops := tagOps("s9", false)
	if len(ops) != 3 {
		t.Fatalf("tagOps = %+v, want one op per tag", ops)
	}
	for _, o := range ops {
		if name, res := routeBatchOp(nil, o); name == "" {
			t.Errorf("add to %q is not routed: %s", o.Target, res.Reason)
		}
	}
}

// TestFileDecisionEventsGoWritesTheEntry: a recorded DECISION lands in decisionLog.md once and leaves the tag block.
func TestFileDecisionEventsGoWritesTheEntry(t *testing.T) {
	tempConfig(t)
	write(t, TagEventsPath("s7"), `{"event":"decision","text":"use A","at":"2026-11-20T10:00:00Z"}`+"\n"+
		`{"event":"note","text":"n1","at":"x"}`+"\n")
	for range 2 {
		if err := fileDecisionEvents("s7", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := readFile(Path("decisionLog.md"))
	if n := strings.Count(string(b), "— use A"); n != 1 || !strings.Contains(string(b), "## 2026-11-20 — use A") {
		t.Fatalf("decisionLog.md = %q, want one dated entry for use A", b)
	}
	if got := tagBlock("", "s7", false); strings.Contains(got, "use A") || !strings.Contains(got, "NOTE: n1") {
		t.Fatalf("tagBlock = %q, want the note and no filed decision", got)
	}
}

// TestFiledDecisionsMatchesWholeTitleAndStruckEntries: "use AB" does not file "use A"; a struck title still counts.
func TestFiledDecisionsMatchesWholeTitleAndStruckEntries(t *testing.T) {
	tempConfig(t)
	write(t, Path("decisionLog.md"), "# Decision Log\n\n## 2026-11-20 — use AB <!-- id:12345678 -->\n\n## 2026-11-19 — ~~use C~~ <!-- id:87654321 -->\n")
	filed := filedDecisions()
	if filed("use A") || !filed("use AB") || !filed("use C") {
		t.Fatalf("filed(use A, use AB, use C) = %v %v %v, want false true true", filed("use A"), filed("use AB"), filed("use C"))
	}
}

// TestTagBlockFiledDecisionsNeverEvictAPendingBug: decisions already in the log leave before the legacy cap.
func TestTagBlockFiledDecisionsNeverEvictAPendingBug(t *testing.T) {
	tempConfig(t)
	var log, text strings.Builder
	log.WriteString("# Decision Log\n\n")
	text.WriteString(`BUG: pending\n`)
	for i := range tagMaxCount + 1 {
		log.WriteString("## 2026-11-20 — d" + strconv.Itoa(i) + "\n\n")
		text.WriteString("DECISION: d" + strconv.Itoa(i) + `\n`)
	}
	write(t, Path("decisionLog.md"), log.String())
	path := filepath.Join(t.TempDir(), "s6.jsonl")
	write(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+text.String()+`"}]}}`)
	if got := tagBlock(path, "s6", false); !strings.Contains(got, "BUG: pending") || strings.Contains(got, "DECISION") {
		t.Fatalf("tagBlock = %q, want the pending BUG and no filed decision", got)
	}
}

// TestFiledByGoBlockListsEntryAndBareTagMatchesRationaleTitle: date, title and id listed; "x" matches "x — why".
func TestFiledByGoBlockListsEntryAndBareTagMatchesRationaleTitle(t *testing.T) {
	tempConfig(t)
	write(t, Path("decisionLog.md"), "# Decision Log\n\n## 2026-11-20 — use X — because Y <!-- id:12345678 -->\n")
	write(t, TagEventsPath("s5"), `{"event":"decision","text":"use X","at":"x"}`+"\n")
	if !filedDecisions()("use X") {
		t.Fatal("bare tag did not match its rationale title")
	}
	if got := filedByGoBlock("s5"); !strings.Contains(got, "- 2026-11-20 — use X — because Y (id 12345678)") {
		t.Fatalf("filedByGoBlock = %q", got)
	}
}

// typedStore stands in for the schema stamp so the typed path runs on a fresh test store.
func typedStore(t *testing.T, ready bool) {
	t.Helper()
	old := typedStoreReady
	typedStoreReady = func() bool { return ready }
	t.Cleanup(func() { typedStoreReady = old })
}

func TestUpdateAppliesTypedBatch(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	typedStore(t, true)
	good := `{"ops":[{"op":"add","target":"sessionHistory","title":"typed arc","text":"- shipped typed update"},` +
		`{"op":"add","target":"activeContext","section":"Next Steps","text":"verify the typed path"}]}`
	calls := 0
	run := func(ctx context.Context, _, _ string) (string, error) {
		calls++
		AnswerJob(ctx, good)
		return "stub", nil
	}
	if err := update(updateOptions{Run: run, CWD: root, Session: "t1"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := readUpdateResult("t1"); r.Status != updateOK || r.Typed != "ok" || calls != 1 {
		t.Errorf("typed result %+v after %d runs", r, calls)
	}
	hist, _ := os.ReadFile(Path("sessionHistory.md"))
	active, _ := os.ReadFile(Path("activeContext.md"))
	if !strings.Contains(string(hist), "typed arc") || !strings.Contains(string(active), "verify the typed path") {
		t.Errorf("ops not written:\n%s\n%s", hist, active)
	}
	assertTypedFallback(t, root)
}

// assertTypedFallback: an invalid answer is retried once with the message, then the tools-on run takes over.
func assertTypedFallback(t *testing.T, root string) {
	t.Helper()
	calls := 0
	var prompts []string
	bad := func(ctx context.Context, p, _ string) (string, error) {
		calls++
		prompts = append(prompts, p)
		AnswerJob(ctx, `{"ops":[{"op":"bogus"}]}`)
		return "stub", nil
	}
	if err := update(updateOptions{Run: bad, CWD: root, Session: "t2"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := readUpdateResult("t2"); r.Typed != "fallback" || calls != 3 || !strings.Contains(prompts[1], "previous answer was rejected") {
		t.Errorf("fallback result %+v after %d runs", r, calls)
	}
}

func TestUpdateOlderStoreKeepsUntypedPath(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	typedStore(t, false)
	calls := 0
	run := func(ctx context.Context, _, _ string) (string, error) {
		calls++
		if JobAnswerSchema(ctx) != "" {
			t.Error("a v3 store must not ask for a typed answer")
		}
		return "stub", nil
	}
	var out bytes.Buffer
	if err := update(updateOptions{Run: run, CWD: root, Session: "t3", Out: &out}); err != nil {
		t.Fatal(err)
	}
	if r, _ := readUpdateResult("t3"); r.Typed != "" || calls != 1 || !strings.Contains(out.String(), "memory upgrade") {
		t.Errorf("old-store result %+v after %d runs, out %q", r, calls, out.String())
	}
}

func TestUpdateApplyFailureFallsBackOnce(t *testing.T) {
	stale := `{"ops":[{"op":"close","id":"z00000000","reason":"done"}]}`
	good := `{"ops":[{"op":"add","target":"sessionHistory","title":"fallback arc","text":"- placed by the fallback"}]}`
	for name, answer := range map[string]string{"corrected": good, "still bad": stale} {
		t.Run(name, func(t *testing.T) {
			clearChildEnv(t)
			tempConfig(t)
			root := repoRoot(t, "proj")
			if _, err := Init(); err != nil {
				t.Fatal(err)
			}
			typedStore(t, true)
			calls := 0
			standInStoreCopy(t)
			old := CodeModeRun
			CodeModeRun = func(_ context.Context, prompt, _, _, _ string) (string, string, error) {
				calls++
				if !strings.Contains(prompt, "op 0 (close): unknown id") {
					t.Errorf("fallback prompt lacks the failed op's reason")
				}
				return answer, "fake-pi", nil
			}
			t.Cleanup(func() { CodeModeRun = old })
			run := func(ctx context.Context, _, _ string) (string, error) {
				AnswerJob(ctx, stale)
				return "stub", nil
			}
			err := update(updateOptions{Run: run, CWD: root, Session: "ta"})
			hist, _ := os.ReadFile(Path("sessionHistory.md"))
			wrote := strings.Contains(string(hist), "fallback arc")
			if calls != 1 || wrote != (name == "corrected") || (err != nil) != (name == "still bad") ||
				err != nil && !strings.Contains(err.Error(), "fake-pi") {
				t.Errorf("dispatches %d, written %v, err %v", calls, wrote, err)
			}
		})
	}
}

func TestUpdateExhaustedValidationUsesTheFallbackOnce(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	typedStore(t, true)
	standInStoreCopy(t)
	calls, runs := 0, 0
	old := CodeModeRun
	CodeModeRun = func(context.Context, string, string, string, string) (string, string, error) {
		calls++
		return `{"ops":[{"op":"add","target":"sessionHistory","title":"exhausted arc","text":"- via fallback"}]}`, "fake-pi", nil
	}
	t.Cleanup(func() { CodeModeRun = old })
	run := func(ctx context.Context, _, _ string) (string, error) {
		runs++
		AnswerJob(ctx, `{"ops":[{"op":"bogus"}]}`)
		return "stub", nil
	}
	if err := update(updateOptions{Run: run, CWD: root, Session: "tx"}); err != nil {
		t.Fatal(err)
	}
	hist, _ := os.ReadFile(Path("sessionHistory.md"))
	if r, _ := readUpdateResult("tx"); calls != 1 || runs != 2 || r.Typed != "ok" || !strings.Contains(string(hist), "exhausted arc") {
		t.Errorf("fallback calls %d, runs %d, result %+v", calls, runs, r)
	}
}

func TestUpdateDecisionEventsNotFiledOnFailure(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	t.Setenv("HARNESS_KEYRING", "off")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	typedStore(t, true)
	write(t, TagEventsPath("failed"), `{"event":"decision","text":"must not appear","at":"2026-11-20T10:00:00Z"}`+"\n")
	before := mustRead(t, Path("decisionLog.md"))
	calls := 0
	run := func(context.Context, string, string) (string, error) {
		calls++
		return "stub", errors.New("runner failed")
	}
	if err := update(updateOptions{Run: run, CWD: root, Session: "failed"}); err == nil {
		t.Fatal("expected runner failure")
	}
	if calls != 2 || mustRead(t, Path("decisionLog.md")) != before {
		t.Fatalf("calls %d; decision log changed: %s", calls, mustRead(t, Path("decisionLog.md")))
	}
}

func TestUpdateFallbackErrorNamesBothModels(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	t.Setenv("HARNESS_KEYRING", "off")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	typedStore(t, true)
	standInStoreCopy(t)
	old := CodeModeRun
	CodeModeRun = func(context.Context, string, string, string, string) (string, string, error) {
		return `{"ops":[{"op":"close","id":"z00000000","reason":"done"}]}`, "pi fallback-model", nil
	}
	t.Cleanup(func() { CodeModeRun = old })
	run := func(ctx context.Context, _, _ string) (string, error) {
		answer, _, err := JobCodeFallback(ctx).Run(ctx, "prompt", root, BatchSchema())
		AnswerJob(ctx, answer)
		ReportJob(ctx, JobResult{Agent: "pi", Model: "fallback-model", Primary: "primary-model"})
		return "pi", err
	}
	err := update(updateOptions{Run: run, CWD: root, Session: "names"})
	if err == nil || !strings.Contains(err.Error(), "primary-model then fallback-model") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdateNamedKeepsModelsOnAfterCommit(t *testing.T) {
	j := JobResult{Agent: "pi", Model: "fallback-model", Primary: "primary-model"}
	after := &AfterCommitError{errors.New("settle failed")}
	got := j.named(after)
	if !strings.Contains(got.Error(), "primary-model then fallback-model") || refused(got) {
		t.Errorf("after-commit error lost the models or its type: %v", got)
	}
	if plain := errors.New("refused"); j.named(plain) != plain {
		t.Error("a refusal is not wrapped: its retry names the models")
	}
}
