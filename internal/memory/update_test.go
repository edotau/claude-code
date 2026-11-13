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
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	write(t, path, strings.Join(lines, "\n"))
	return path
}

func TestTagBlock_EmptyTranscriptNoBlock(t *testing.T) {
	t.Parallel()
	path := writeTranscript(t, "")
	if got := tagBlock(path, false); got != "" {
		t.Fatalf("tagBlock on an empty transcript = %q, want empty", got)
	}
}

func TestTagBlock_MissingTranscriptNoBlock(t *testing.T) {
	t.Parallel()
	if got := tagBlock(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), false); got != "" {
		t.Fatalf("tagBlock on a missing transcript = %q, want empty", got)
	}
}

func TestAgentUpdatePrompt_UsesReference(t *testing.T) {
	transcript := writeTranscript(t, "")
	got, err := agentUpdatePrompt(transcript, "", false)
	if err != nil {
		t.Fatalf("agentUpdatePrompt: %v", err)
	}
	ref, err := Reference("update")
	if err != nil {
		t.Fatalf("Reference(update): %v", err)
	}
	if ref == "" || !strings.Contains(got, ref) {
		t.Fatalf("update prompt missing its embedded procedure: %q", got)
	}
	if !strings.Contains(got, transcript) {
		t.Fatalf("update prompt missing transcript path: %q", got)
	}
}

// TestAgentUpdatePrompt_WithCWDAppendsGitActivity: a transcript with a parseable timestamp and a
// real git cwd gets a "git activity" block with the session's commits/touched files appended.
func TestAgentUpdatePrompt_WithCWDAppendsGitActivity(t *testing.T) {
	cwd := t.TempDir()
	hosttest.In(t, cwd, "init", "-q")
	write(t, filepath.Join(cwd, "f.txt"), "x")
	hosttest.In(t, cwd, "add", "f.txt")
	hosttest.In(t, cwd, "commit", "-q", "-m", "seed commit")

	ts := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	path := writeTranscript(t, `{"timestamp":"`+ts+`"}`)
	got, err := agentUpdatePrompt(path, cwd, false)
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

func TestDirSlug(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "My Repo.v2")
	if got := Dir(root); filepath.Dir(got) != StoreDir() || !strings.HasPrefix(filepath.Base(got), "my-repo.v2") {
		t.Errorf("Dir = %s want a my-repo.v2 store under %s", got, StoreDir())
	}
	if isHomeBank(root) {
		t.Error("a repo's bank must not be the home bank")
	}
	if plain := t.TempDir(); !isHomeBank(plain) {
		t.Errorf("a non-repo root must collapse to the global bank, got %s", Dir(plain))
	}
}

func TestInitNoOverwrite(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "My Repo.v2")
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range BankFiles() {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s", name)
		}
	}
	write(t, filepath.Join(dir, "activeContext.md"), "mine\n")
	_ = os.Remove(filepath.Join(dir, "progress.md"))
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "activeContext.md")); string(b) != "mine\n" {
		t.Error("Init overwrote an existing file")
	}
	if !bankExists(root) || !fileExists(filepath.Join(dir, "progress.md")) {
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
	root := repoRoot(t, "proj")
	dir := Dir(root)
	write(t, filepath.Join(dir, "decisionLog.md"), "# D\n\n## 2026-01-01 — router retries on 429\n\nwhy: rate limits\n\n## 2026-01-02 — unrelated choice\n\nthe router is mentioned once\n")
	write(t, filepath.Join(HomeBankDir(), "conventions.md"), "# C\n\n- keep commits small\n")
	write(t, filepath.Join(dir, "archive", "sessionHistory-2025-12.md"), "# old\n\n## 2025-12-01 — 429 storm\n")
	hits := searchBanks(root, "the router 429", 0)
	if len(hits) != 3 || !strings.Contains(hits[0].Block, "retries on 429") || hits[0].Bank != "local" {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].Title() != "2026-01-01 — router retries on 429" {
		t.Errorf("title %q", hits[0].Title())
	}
	if g := searchBanks(root, "commits", 5); len(g) != 1 || g[0].Bank != "home" {
		t.Errorf("global hit %+v", g)
	}
	if searchBanks(root, "the and of", 5) != nil {
		t.Error("all-stopword query must return nothing")
	}
}

func TestRotateAndSettle(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	var b strings.Builder
	b.WriteString("# Session History\n\n> newest first\n")
	for i := 35; i >= 1; i-- {
		fmt.Fprintf(&b, "\n## entry %02d\n\n- did %d\n", i, i)
	}
	write(t, filepath.Join(dir, "sessionHistory.md"), b.String())
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	rotated, err := settleBank(root, now)
	if err != nil || rotated != 5 {
		t.Fatalf("rotated %d err %v", rotated, err)
	}
	assertRotated(t, dir)
	if n, _ := rotateSessionHistory(dir, now); n != 0 {
		t.Error("rotation under cap must be a no-op")
	}
}

// assertRotated checks the history kept its newest entries, the oldest went to the archive, and the index follows.
func assertRotated(t *testing.T, dir string) {
	t.Helper()
	_, kept := splitHeadingBlocks(mustRead(t, filepath.Join(dir, "sessionHistory.md")))
	arch := mustRead(t, filepath.Join(dir, "archive", "sessionHistory-2026-09.md"))
	if len(kept) != sessionHistoryMaxEntries || !strings.HasPrefix(kept[0], "## entry 35") || !strings.Contains(arch, "## entry 01") || strings.Contains(arch, "## entry 06") {
		t.Errorf("kept %d first %q archive %q", len(kept), kept[0], arch)
	}
	idx := mustRead(t, filepath.Join(dir, nativeIndexFile))
	if !strings.Contains(idx, "- sessionHistory: entry 35") || strings.Contains(idx, "entry 01") {
		t.Errorf("index %q", idx)
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
	dir, _ := Init(root)
	ok := func(ctx context.Context, _, _ string) (string, error) {
		if !slices.Contains(ChildEnv(ctx), UpdateChildEnv+"=1") {
			return "fake", errors.New("update child env not passed")
		}
		time.Sleep(10 * time.Millisecond)
		f, err := os.OpenFile(filepath.Join(dir, "sessionHistory.md"), os.O_APPEND|os.O_WRONLY, 0o644)
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
	if !strings.Contains(mustRead(t, filepath.Join(dir, nativeIndexFile)), "updated-by-child") {
		t.Error("update did not settle/index the bank")
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

func TestNativeTopicsJoinSearch(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if got := paths.ProjectSlug("/Users/a/.claude"); got != "-Users-a--claude" {
		t.Errorf("ProjectSlug = %q", got)
	}
	native := Dir(root)
	write(t, filepath.Join(native, "gw.md"), "---\nname: gateway\ndescription: home gateway over ssh\n---\n\nRun the status script.\n")
	write(t, filepath.Join(native, "MEMORY.md"), "- [gateway](gw.md) — home gateway over ssh status\n")
	write(t, filepath.Join(native, "loose.md"), "gateway ssh status without frontmatter\n")
	hits := searchBanks(root, "gateway ssh status", 0)
	if len(hits) != 1 || hits[0].Bank != "native" || hits[0].File != "gw.md" || hits[0].Matched != 3 {
		t.Fatalf("want only the frontmatter topic, matching 3 terms; got %+v", hits)
	}
	if !strings.HasPrefix(hits[0].Block, "gateway — home gateway over ssh\n") {
		t.Errorf("topic block %q", hits[0].Block)
	}
}

func TestSeenSetTracksInjectedBlocks(t *testing.T) {
	tempConfig(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "conventions.md"), "# C\n\n- injected rule\n  continues here\n- other rule\n")
	resetSeen("11111111-1111-1111-1111-111111111111", root, "<!-- surface -->\n- injected   rule\ncontinues here\n")
	seen := seenBlocks("11111111-1111-1111-1111-111111111111")
	if !seen[blockKey("- injected rule\n  continues here")] || seen[blockKey("- other rule")] || len(seen) != 1 {
		t.Fatalf("seen after reset: %v", seen)
	}
	markSeen("11111111-1111-1111-1111-111111111111", []SearchHit{{Block: "- other rule"}})
	if !seenBlocks("11111111-1111-1111-1111-111111111111")[blockKey("- other rule")] {
		t.Error("markSeen did not record the hit")
	}
	resetSeen("11111111-1111-1111-1111-111111111111", root, "")
	if len(seenBlocks("11111111-1111-1111-1111-111111111111")) != 0 {
		t.Error("a SessionStart (e.g. after compact) must clear what recall injected")
	}
	for _, bad := range []string{"", ".."} {
		markSeen(bad, []SearchHit{{Block: "x"}})
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

// TestCompoundMatchFindsHyphenatedAndSubstringTerms: "agent routing" finds a block that only carries
// "subagent" (agent is a substring of subagent) and "routing" exactly; a block with an exact "agent"
// outranks the substring-only match; a hyphenated query term ("agent-routing") matches both parts.
func TestCompoundMatchFindsHyphenatedAndSubstringTerms(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "routing")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "decisionLog.md"), "# Decisions\n\n"+
		"## compound candidate\nSubagent routing stays on the local classifier\n\n"+
		"## exact candidate\nAgent routing stays on the local classifier\n\n"+
		"## unrelated entry\nnothing to do with either term\n")

	hits := searchBanks(repo, "agent routing", 0)
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
	// Matched must credit BOTH query terms on the compound block — "agent" only via the subagent
	// substring — not just the one term ("routing") that was already an exact token.
	if compoundMatched != 2 {
		t.Errorf("compound block matched %d terms, want 2 (agent via substring, routing exact): %#v", compoundMatched, hits)
	}
	if exactMatched != 2 {
		t.Errorf("exact block matched %d terms, want 2: %#v", exactMatched, hits)
	}
	if exactScore <= compoundScore {
		t.Errorf("exact match score %v did not outrank the compound match score %v", exactScore, compoundScore)
	}

	assertHyphenatedMatchesBoth(t, repo)
}

// assertHyphenatedMatchesBoth: the hyphenated query "agent-routing" matches both the compound and the exact block.
func assertHyphenatedMatchesBoth(t *testing.T, repo string) {
	t.Helper()
	hyphen := searchBanks(repo, "agent-routing", 0)
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

// embeddedVerbs are every verb Go actually calls memory.Reference with — grepped by hand from
// update.go, dream.go, promote.go — each must resolve to a non-empty embedded prompt.
var embeddedVerbs = []string{"init", "review", "promote", "update", "dream"}

// TestReferenceEmbeddedVerbsNonEmpty: every verb Go calls Reference(verb) with resolves to a
// non-empty embedded prompt — no disk path left to go missing at runtime.
func TestReferenceEmbeddedVerbsNonEmpty(t *testing.T) {
	for _, verb := range embeddedVerbs {
		got, err := Reference(verb)
		if err != nil {
			t.Errorf("Reference(%q): %v", verb, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("Reference(%q) = empty, want a non-empty procedure", verb)
		}
	}
}

// TestReferenceUnknownVerbErrors: a verb with no embedded prompts/<verb>.md is an error, not an
// empty string — a caller must never run a background job on a silently empty prompt.
func TestReferenceUnknownVerbErrors(t *testing.T) {
	if _, err := Reference("does-not-exist"); err == nil {
		t.Fatal("Reference(\"does-not-exist\") = nil error, want one")
	}
}

// TestReferenceEmptyVerbErrors: the empty string is also an unknown verb.
func TestReferenceEmptyVerbErrors(t *testing.T) {
	if _, err := Reference(""); err == nil {
		t.Fatal("Reference(\"\") = nil error, want one")
	}
}

// TestAgentDreamPrompt_UsesReference: the embedded dream procedure lands in the built prompt.
func TestAgentDreamPrompt_UsesReference(t *testing.T) {
	got, err := agentDreamPrompt([]string{"/m/a.md"})
	if err != nil {
		t.Fatalf("agentDreamPrompt: %v", err)
	}
	ref, err := Reference("dream")
	if err != nil {
		t.Fatalf("Reference(\"dream\"): %v", err)
	}
	if !strings.Contains(got, ref) {
		t.Errorf("prompt missing the embedded dream reference:\n%s", got)
	}
	if !strings.Contains(got, "- /m/a.md") {
		t.Errorf("prompt missing the changed-file list:\n%s", got)
	}
}

// TestDreamFreshStoreRunsWithoutDeadlock: on a fresh store Dream used to re-take its
// own bank lock inside settleBank and hang. Reference("dream") is now embedded and always resolves
// (the old "missing reference" repro is unreachable), so this instead confirms Dream still returns
// promptly and the agent still runs, proving the lock-then-settle-outside-the-lock fix holds.
func TestDreamFreshStoreRunsWithoutDeadlock(t *testing.T) {
	clearChildEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ran := false
	run := func(context.Context, string, string) (string, error) { ran = true; return "stub", nil }
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
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, string, string) (string, error) { return "x", errors.New("boom") }
	if err := dream(dreamOptions{CWD: root, Run: fail}); err == nil {
		t.Fatal("failed agent run must return an error")
	}
	now := time.Now()
	if dreamDue(root, now) {
		t.Error("a failed dream is due again at once, so every session start would rerun it")
	}
	if !dreamDue(root, now.Add(dreamRetry+time.Minute)) {
		t.Error("a failed dream is not due after its backoff")
	}
}

// TestHistoricalUpdatePromptAndReport: an older session's prompt forbids current state, and an edit to it is reported.
func TestHistoricalUpdatePromptAndReport(t *testing.T) {
	clearChildEnv(t)
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	write(t, filepath.Join(dir, "activeContext.md"), "live focus\n")
	var prompt string
	run := func(_ context.Context, p, _ string) (string, error) {
		prompt = p
		write(t, filepath.Join(dir, "activeContext.md"), "(NOW, newest) stale\n")
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
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	var got []string
	run := func(ctx context.Context, _, _ string) (string, error) { got = ChildEnv(ctx); return "stub", nil }
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
	tags, err := extractTags(strings.NewReader(transcript))
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
	tags, err := extractTags(strings.NewReader(transcript))
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
	tags, err := extractTags(strings.NewReader(transcript))
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
	tags, err := extractTags(strings.NewReader(transcript))
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
	tags, err := extractTags(strings.NewReader(transcript))
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
	tags, err := extractTags(strings.NewReader(transcript))
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
	tags, err := extractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Text != "survives the bad line before it" {
		t.Fatalf("got %+v", tags)
	}
}

func TestExtractTags_EmptyTranscript(t *testing.T) {
	t.Parallel()
	tags, err := extractTags(strings.NewReader(""))
	if err != nil {
		t.Fatalf("extractTags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("got %d tags from an empty transcript, want 0", len(tags))
	}
}

// warnChanged lists every changed file in sorted order, every run, so map iteration order never leaks out.
func TestWarnChangedSortedOrder(t *testing.T) {
	dir := t.TempDir()
	names := []string{"progress.md", "activeContext.md", "projectContext.md", "conventions.md", "b.md", "a.md"}
	snap := snapshotFiles(dir, names)
	for _, n := range names {
		write(t, filepath.Join(dir, n), "changed\n")
	}
	var want strings.Builder
	for _, n := range []string{"a.md", "activeContext.md", "b.md", "conventions.md", "progress.md", "projectContext.md"} {
		fmt.Fprintf(&want, "memory update: WARNING: historical run changed %s (not restored)\n", n)
	}
	for range 20 {
		var out bytes.Buffer
		warnChanged(&out, dir, snap)
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
