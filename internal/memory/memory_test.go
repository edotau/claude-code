package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempConfig(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return cfg
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repoRoot makes a git work-tree root (a .git dir is enough for the stat-first check).
func repoRoot(t *testing.T, name string) string {
	t.Helper()
	root := gitRepo(t, canonTemp(t, t.TempDir()), name)
	if _, err := EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDirSlugAndInitNoOverwrite(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "My Repo.v2")
	if got := Dir(root); filepath.Dir(got) != StoreDir() || !strings.HasPrefix(filepath.Base(got), "my-repo.v2") {
		t.Errorf("Dir = %s want a my-repo.v2 store under %s", got, StoreDir())
	}
	if IsHomeBank(root) {
		t.Error("a repo's bank must not be the home bank")
	}
	if plain := t.TempDir(); !IsHomeBank(plain) {
		t.Errorf("a non-repo root must collapse to the global bank, got %s", Dir(plain))
	}
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range bankFiles {
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
	if !HasBank(root) || func() bool { _, err := os.Stat(filepath.Join(dir, "progress.md")); return err != nil }() {
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
	hits := SearchBanks(root, "the router 429", 0)
	if len(hits) != 3 || !strings.Contains(hits[0].Block, "retries on 429") || hits[0].Bank != "local" {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].Title() != "2026-01-01 — router retries on 429" {
		t.Errorf("title %q", hits[0].Title())
	}
	if g := SearchBanks(root, "commits", 5); len(g) != 1 || g[0].Bank != "home" {
		t.Errorf("global hit %+v", g)
	}
	if SearchBanks(root, "the and of", 5) != nil {
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
	s, err := SettleBank(root, now)
	if err != nil || s.Rotated != 5 {
		t.Fatalf("rotated %d err %v", s.Rotated, err)
	}
	_, kept := splitHeadingBlocks(mustRead(t, filepath.Join(dir, "sessionHistory.md")))
	arch := mustRead(t, filepath.Join(dir, "archive", "sessionHistory-2026-09.md"))
	if len(kept) != SessionHistoryMaxEntries || !strings.HasPrefix(kept[0], "## entry 35") || !strings.Contains(arch, "## entry 01") || strings.Contains(arch, "## entry 06") {
		t.Errorf("kept %d first %q archive %q", len(kept), kept[0], arch)
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- sessionHistory: entry 35") || strings.Contains(idx, "entry 01") {
		t.Errorf("index %q", idx)
	}
	if n, _ := RotateSessionHistory(dir, now); n != 0 {
		t.Error("rotation under cap must be a no-op")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCapCharsBelowMarkerYieldsEmpty(t *testing.T) {
	if got := CapChars("aaaa\nbbbb", 3); got != "" {
		t.Errorf("a cap below the marker length yields empty, got %q", got)
	}
}

func TestUpdateRecordsOutcome(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir, _ := Init(root)
	bin := t.TempDir()
	ok := filepath.Join(bin, "claude-ok")
	write(t, ok, "#!/bin/sh\n[ \"$"+UpdateChildEnv+"\" = 1 ] || exit 3\nsleep 0.01\nprintf '\\n## 2026-09-16 — updated-by-child\\n' >> "+filepath.Join(dir, "sessionHistory.md")+"\n")
	bad := filepath.Join(bin, "claude-bad")
	write(t, bad, "#!/bin/sh\nexit 1\n")
	_ = os.Chmod(ok, 0o755)
	_ = os.Chmod(bad, 0o755)
	_ = WriteUpdateResult("11111111-1111-1111-1111-111111111111", UpdateResult{Status: UpdatePending, Time: time.Now(), Log: "/l.log"})

	if err := Update(UpdateOptions{Claude: ok, CWD: root, Session: "11111111-1111-1111-1111-111111111111"}); err != nil {
		t.Fatal(err)
	}
	r, _ := ReadUpdateResult("11111111-1111-1111-1111-111111111111")
	if r.Status != UpdateOK || !r.BankUpdated || r.Log != "/l.log" || r.NeedsAttention(time.Now()) {
		t.Errorf("success result %+v", r)
	}
	if !strings.Contains(mustRead(t, filepath.Join(dir, NativeIndexFile)), "updated-by-child") {
		t.Error("update did not settle/index the bank")
	}
	if err := Update(UpdateOptions{Claude: bad, CWD: root, Session: "s2"}); err == nil {
		t.Error("failing claude must return an error")
	}
	if r, _ := ReadUpdateResult("s2"); r.Status != UpdateFailed || r.BankUpdated || !r.NeedsAttention(time.Now()) {
		t.Errorf("failure result %+v", r)
	}
	if err := Update(UpdateOptions{CWD: root, Session: "s3"}); err == nil {
		t.Error("missing claude must fail")
	}
	stale := UpdateResult{Status: UpdatePending, Time: time.Now().Add(-20 * time.Minute)}
	if !stale.NeedsAttention(time.Now()) {
		t.Error("a pending result past the grace window needs attention")
	}
}

func TestNativeTopicsJoinSearch(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if got := ProjectSlug("/Users/a/.claude"); got != "-Users-a--claude" {
		t.Errorf("ProjectSlug = %q", got)
	}
	native := NativeDir(root)
	write(t, filepath.Join(native, "gw.md"), "---\nname: gateway\ndescription: home gateway over ssh\n---\n\nRun the status script.\n")
	write(t, filepath.Join(native, "MEMORY.md"), "- [gateway](gw.md) — home gateway over ssh status\n")
	write(t, filepath.Join(native, "loose.md"), "gateway ssh status without frontmatter\n")
	hits := SearchBanks(root, "gateway ssh status", 0)
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
	ResetSeen("11111111-1111-1111-1111-111111111111", root, "<!-- surface -->\n- injected   rule\ncontinues here\n")
	seen := SeenBlocks("11111111-1111-1111-1111-111111111111")
	if !seen[BlockKey("- injected rule\n  continues here")] || seen[BlockKey("- other rule")] || len(seen) != 1 {
		t.Fatalf("seen after reset: %v", seen)
	}
	MarkSeen("11111111-1111-1111-1111-111111111111", []SearchHit{{Block: "- other rule"}})
	if !SeenBlocks("11111111-1111-1111-1111-111111111111")[BlockKey("- other rule")] {
		t.Error("MarkSeen did not record the hit")
	}
	ResetSeen("11111111-1111-1111-1111-111111111111", root, "")
	if len(SeenBlocks("11111111-1111-1111-1111-111111111111")) != 0 {
		t.Error("a SessionStart (e.g. after compact) must clear what recall injected")
	}
	for _, bad := range []string{"", ".."} {
		MarkSeen(bad, []SearchHit{{Block: "x"}})
		if len(SeenBlocks(bad)) != 0 {
			t.Errorf("session id %q must be a no-op", bad)
		}
	}
}

// credentialPattern is the coarse memory tripwire: ghs_ with 10+ body characters is caught, a short stub is not.
func TestCredentialPatternGitHubShapes(t *testing.T) {
	if !credentialPattern.MatchString("token ghs_abcdefghij12 leaked") {
		t.Error("ghs_ + 12 chars must match")
	}
	if credentialPattern.MatchString("see ghs_abc for the format") {
		t.Error("ghs_abc is too short to be a token")
	}
}

// TestSearchHitPlainStripsMarkup: a preview drops heading/bullet marks and id markers but keeps every word.
func TestSearchHitPlainStripsMarkup(t *testing.T) {
	t.Parallel()
	h := SearchHit{Block: "## 2026-10-23 — Routing stays local <!-- id:7e92d61b -->\n\n- Rejected the remote router.\n"}
	want := "2026-10-23 — Routing stays local Rejected the remote router."
	if got := h.Plain(); got != want {
		t.Errorf("Plain() = %q, want %q", got, want)
	}
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

	hits := SearchBanks(repo, "agent routing", 0)
	var compoundScore, exactScore float64
	var compoundMatched, exactMatched int
	var compoundSeen, exactSeen bool
	for _, h := range hits {
		switch {
		case strings.Contains(h.Block, "Subagent routing"):
			compoundSeen, compoundScore, compoundMatched = true, h.Score, h.Matched
		case strings.Contains(h.Block, "Agent routing"):
			exactSeen, exactScore, exactMatched = true, h.Score, h.Matched
		}
	}
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

	hyphen := SearchBanks(repo, "agent-routing", 0)
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
	exact := write(ProjectSlug(cwd), "s1")
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
	got, err := agentDreamPrompt()
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
}

// TestDreamFreshStoreRunsWithoutDeadlock: on a fresh store Dream used to re-take its
// own bank lock inside SettleBank and hang. Reference("dream") is now embedded and always resolves
// (the old "missing reference" repro is unreachable), so this instead confirms Dream still returns
// promptly and the agent still runs, proving the lock-then-settle-outside-the-lock fix holds.
func TestDreamFreshStoreRunsWithoutDeadlock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ran := false
	run := func(context.Context, string, string) (string, error) { ran = true; return "stub", nil }
	done := make(chan error, 1)
	go func() { done <- Dream(DreamOptions{CWD: t.TempDir(), Run: run}) }()
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
