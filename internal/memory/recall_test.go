package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/proc"
)

// searchBanks ranks root's local, home and archive banks plus native topics; top k (0 = all).
func searchBanks(query string, k int) []SearchHit {
	ranked, _, _ := searchBankTerms(contentTerms(query), k)
	return ranked
}

func writeBank(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(StoreDir(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p, body)
}

func recallEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
}

// wantRecallBodies requires lines to lead with the 2 strongest matches as bodies, regardless of entry type.
func wantRecallBodies(t *testing.T, got string, lines []string) {
	t.Helper()
	bodies := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "- [") {
			bodies++
		}
	}
	if bodies != 2 || !strings.HasPrefix(lines[0], "- [conventions.md] aaaa0005") || !strings.HasPrefix(lines[1], "- [sessionHistory.md] aaaa0004") {
		t.Errorf("want the 2 strongest matches as bodies regardless of type:\n%s", got)
	}
}

// wantRecallPointers requires every line to be an expandable `id · date · title` pointer, newest decision first.
func wantRecallPointers(t *testing.T, got string, pointers []string) {
	t.Helper()
	pointer := regexp.MustCompile(`^- (\S+) · (\d{4}-\d{2}-\d{2}|undated) · \S`)
	for _, l := range pointers {
		m := pointer.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("not an `id · date · title` pointer: %q", l)
		}
		if _, _, err := ShowBlock(m[1]); err != nil {
			t.Errorf("pointer id %s does not expand: %v", m[1], err)
		}
	}
	if !strings.Contains(pointers[0], "aaaa0002") || !strings.Contains(pointers[1], "aaaa0001") {
		t.Errorf("newest decision must win an exact relevance tie among pointers:\n%s", got)
	}
}

// One pass: struck entries drop, newest decisions lead, two bodies then pointers, archive only when nothing current.
func TestRecallEntriesFilterOrderAndExpand(t *testing.T) {
	recallEnv(t)
	writeBank(t, "decisionLog.md", "# D\n\n"+
		"## 2026-10-01 — Zebra quota limiter older choice <!-- id:aaaa0001 -->\n\nzebra quota limiter fallback older rationale\n\n"+
		"## 2026-10-20 — Zebra quota limiter newer choice <!-- id:aaaa0002 -->\n\nzebra quota limiter fallback newer rationale\n\n"+
		"## ~~2026-10-25 — Zebra quota limiter struck choice~~ — SUPERSEDED by aaaa0002 <!-- id:aaaa0003 -->\n\nzebra quota limiter fallback struck rationale\n")
	writeBank(t, "sessionHistory.md", "# S\n\n## 2026-10-28 — Zebra quota limiter session narrative <!-- id:aaaa0004 -->\n\nzebra quota limiter fallback narrative\n")
	writeBank(t, "conventions.md", "# C\n\n- Zebra quota limiter fallback convention rule <!-- id:aaaa0005 -->\n")
	writeBank(t, "archive/decisionLog-2026-09.md", "# D\n\n## 2026-09-01 — Zebra quota limiter archived choice <!-- id:aaaa0006 -->\n\nzebra quota limiter fallback archived\n\n"+
		"## 2026-09-02 — Quokka burrow tunnel survey archived <!-- id:aaaa0007 -->\n\nquokka burrow tunnel survey results\n")
	got := PromptRecall(context.Background(), "s1", "zebra quota limiter fallback", io.Discard)
	if !strings.HasPrefix(got, "<!-- memory-recall;") {
		t.Fatalf("header missing: %q", got)
	}
	for _, absent := range []string{"struck", "archived choice"} {
		if strings.Contains(got, absent) {
			t.Errorf("%q must not be injected (superseded / archive with current matches):\n%s", absent, got)
		}
	}
	lines := strings.Split(got, "\n")[1:]
	if len(lines) != 4 {
		t.Fatalf("want 4 entries (2 bodies + 2 pointers), got %d:\n%s", len(lines), got)
	}
	wantRecallBodies(t, got, lines)
	wantRecallPointers(t, got, lines[2:])
	if again := PromptRecall(context.Background(), "s1", "zebra quota limiter fallback", io.Discard); strings.Contains(again, "aaaa0005") || strings.Contains(again, "aaaa0004") {
		t.Errorf("bodies already in context must not repeat:\n%s", again)
	}

	if arch := PromptRecall(context.Background(), "s2", "quokka burrow tunnel survey", io.Discard); !strings.Contains(arch, "aaaa0007") && !strings.Contains(arch, "Quokka burrow") {
		t.Errorf("archive must answer when nothing current matches: %q", arch)
	}
}

// vecServer embeds a text as [count(gamma|zeta), count(delta), 0.1]; slow > 0 stalls every reply.
func slowVecServer(t *testing.T, slow time.Duration) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow > 0 {
			select {
			case <-time.After(slow):
			case <-release:
				return
			}
		}
		var in struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		var data []map[string]any
		for i, s := range in.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(strings.Count(s, "gamma") + strings.Count(s, "zeta")), float32(strings.Count(s, "delta")), 0.1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close so a stalled handler lets go
	return srv
}

func useEmbedServer(t *testing.T, srv *httptest.Server) *EmbedClient {
	t.Helper()
	t.Setenv("CLAUDE_PROFILE", "fake") // embedQuery reads the session profile's cache
	c := &EmbedClient{Model: "fake-gte", EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		return postEmbeddings(ctx, srv.URL, texts, func(http.Header) error { return nil })
	}}
	prev := openEmbedClient
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return c, nil }
	t.Cleanup(func() { openEmbedClient = prev })
	return c
}

// seven keyword-equal decisions, five slots: the two closest by vector win a slot only when semantic rank is on.
func seedBlendBank(t *testing.T) {
	var b strings.Builder
	b.WriteString("# D\n\n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&b, "## 2026-10-%02d — Plain walrus harbour choice <!-- id:bbbb000%d -->\n\nwalrus harbour tide ledger plain\n\n", i, i)
	}
	for i := 6; i <= 7; i++ {
		fmt.Fprintf(&b, "## 2026-09-%02d — Gamma walrus harbour choice <!-- id:bbbb000%d -->\n\nwalrus harbour tide ledger gamma gamma gamma pad pad pad pad pad pad pad pad pad pad pad pad pad pad pad\n\n", i, i)
	}
	writeBank(t, "decisionLog.md", b.String())
}

func TestRecallBlendsCosineWithKeyword(t *testing.T) {
	recallEnv(t)
	seedBlendBank(t)
	cwd := t.TempDir()
	const prompt = "walrus harbour tide ledger zeta"

	base := PromptRecall(context.Background(), "a", prompt, io.Discard)
	if strings.Contains(base, "bbbb0006") || strings.Contains(base, "bbbb0007") {
		t.Fatalf("fixture invalid: keyword-only already surfaces the gamma entries:\n%s", base)
	}
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	blended := PromptRecall(context.Background(), "b", prompt, io.Discard)
	if !strings.Contains(blended, "bbbb0006") || !strings.Contains(blended, "bbbb0007") {
		t.Errorf("cosine must lift the gamma entries into the five slots:\n%s", blended)
	}
	if len(strings.Split(blended, "\n")) != 6 {
		t.Errorf("filters must still cap at 5 entries:\n%s", blended)
	}
}

// recallPicks reads the session profile for its semantic query from the ctx it is handed, not the process env.
func TestRecallPicksSemanticQueryUsesTheCtxProfile(t *testing.T) {
	recallEnv(t)
	seedBlendBank(t)
	cwd := t.TempDir()
	const prompt = "walrus harbour tide ledger zeta"
	if _, _, err := embedChanged(context.Background(), cwd, useEmbedServer(t, slowVecServer(t, 0))); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROFILE", "") // the process names no profile: only the snapshot does
	snap := proc.WithValues(context.Background(), proc.Values{"CLAUDE_PROFILE": "fake"})
	if _, tr := recallPicks(snap, "", prompt, true, true); tr.Embeddings != "ran" {
		t.Errorf("snapshot ctx: embeddings = %q, want the semantic query to run on the snapshot's profile", tr.Embeddings)
	}
	if _, tr := recallPicks(context.Background(), "", prompt, true, true); !strings.HasPrefix(tr.Embeddings, "keyword-only") {
		t.Errorf("no snapshot: embeddings = %q, want keyword-only", tr.Embeddings)
	}
}

// A stalled endpoint costs at most the 1.2s budget and the output is exactly the keyword-only result.
func TestRecallFallsBackToKeywordOnTimeout(t *testing.T) {
	recallEnv(t)
	seedBlendBank(t)
	cwd := t.TempDir()
	const prompt = "walrus harbour tide ledger zeta"
	if _, _, err := embedChanged(context.Background(), cwd, useEmbedServer(t, slowVecServer(t, 0))); err != nil {
		t.Fatal(err)
	}
	want := PromptRecall(context.Background(), "kw", prompt, io.Discard) // semantic on, fast endpoint
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "0")
	keyword := PromptRecall(context.Background(), "kw0", prompt, io.Discard)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	if want == keyword {
		t.Fatalf("fixture invalid: blend and keyword results are identical:\n%s", keyword)
	}

	prevWait := embedRecallWait
	embedRecallWait = 100 * time.Millisecond
	t.Cleanup(func() { embedRecallWait = prevWait })
	useEmbedServer(t, slowVecServer(t, 10*time.Second))
	start := time.Now()
	got := PromptRecall(context.Background(), "slow", prompt, io.Discard)
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Errorf("recall took %s with a stalled endpoint, budget is ~100ms", el)
	}
	if got != keyword {
		t.Errorf("timeout must fall back to the keyword result:\n got: %s\nwant: %s", got, keyword)
	}
}

// Once the current decision is in context, the same prompt must not fall back to an older archived one.
func TestRecallNoArchiveFallbackWhenCurrentIsSeen(t *testing.T) {
	recallEnv(t)
	writeBank(t, "decisionLog.md", "# D\n\n## 2026-10-20 — Zebra quota limiter current <!-- id:aaaa0002 -->\n\nzebra quota limiter fallback current\n")
	writeBank(t, "archive/decisionLog-2026-09.md", "# D\n\n## 2026-09-01 — Zebra quota limiter outdated <!-- id:aaaa0006 -->\n\nzebra quota limiter fallback outdated\n")
	if first := PromptRecall(context.Background(), "s1", "zebra quota limiter fallback", io.Discard); !strings.Contains(first, "aaaa0002") {
		t.Fatalf("setup: %q", first)
	}
	if again := PromptRecall(context.Background(), "s1", "zebra quota limiter fallback", io.Discard); again != "" {
		t.Errorf("seen current entry must not be replaced by the archive:\n%s", again)
	}
}

// A prompt sharing no word with the entry it means still recalls it when the vectors are close.
func TestRecallSemanticReachesZeroOverlapEntry(t *testing.T) {
	recallEnv(t)
	writeBank(t, "decisionLog.md", "# D\n\n## 2026-10-20 — Quokka burrow gamma choice <!-- id:cccc0001 -->\n\nquokka burrow gamma gamma\n\n"+
		"## 2026-10-19 — Walrus harbour plain choice <!-- id:cccc0002 -->\n\nwalrus harbour plain\n")
	cwd := t.TempDir()
	const prompt = "hello world foo bar zeta"
	if got := PromptRecall(context.Background(), "k", prompt, io.Discard); got != "" {
		t.Fatalf("fixture invalid: keyword-only already answers: %q", got)
	}
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	got := PromptRecall(context.Background(), "s", prompt, io.Discard)
	if !strings.Contains(got, "cccc0001") || strings.Contains(got, "cccc0002") {
		t.Errorf("want only the semantically close entry:\n%s", got)
	}
}

// Seen entries must not occupy the semantic top-k: with five of them closest, a sixth still surfaces.
func TestRecallSemanticSkipsSeenBeforeTopK(t *testing.T) {
	recallEnv(t)
	var b strings.Builder
	b.WriteString("# D\n\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&b, "## 2026-10-%02d — Quokka note %d <!-- id:dddd000%d -->\n\nquokka burrow %s\n\n", i, i, i, strings.Repeat("gamma ", i+1))
	}
	writeBank(t, "decisionLog.md", b.String())
	cwd := t.TempDir()
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	var seen []SearchHit
	for _, h := range searchBanks("quokka burrow", 0) {
		if !strings.Contains(h.Block, "dddd0006") {
			seen = append(seen, h)
		}
	}
	markSeen("sess", seen, false) // the five closest are already in context
	got := PromptRecall(context.Background(), "sess", "hello world foo bar zeta", io.Discard)
	if !strings.Contains(got, "dddd0006") {
		t.Errorf("the sixth entry never surfaced:\n%s", got)
	}
}

// A current semantic-only match already in context keeps the archive out, same as a keyword one.
func TestRecallNoArchiveFallbackWhenSemanticMatchSeen(t *testing.T) {
	recallEnv(t)
	writeBank(t, "decisionLog.md", "# D\n\n## 2026-10-20 — Quokka burrow choice <!-- id:eeee0001 -->\n\nquokka burrow gamma gamma\n")
	writeBank(t, "archive/decisionLog-2026-09.md", "# D\n\n## 2026-09-01 — Hello world outdated <!-- id:eeee0009 -->\n\nhello world foo bar older\n")
	cwd := t.TempDir()
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	const prompt = "hello world foo bar zeta"
	if first := PromptRecall(context.Background(), "sm", prompt, io.Discard); !strings.Contains(first, "eeee0001") {
		t.Fatalf("setup: semantic match not recalled: %q", first)
	}
	if again := PromptRecall(context.Background(), "sm", prompt, io.Discard); again != "" {
		t.Errorf("seen semantic match must not let the archive answer:\n%s", again)
	}
}

// An edited entry keeps its full keyword score, so a weaker semantic-only match can fall below the floor.
func TestRecallUncachedEntryKeepsFullKeywordScore(t *testing.T) {
	recallEnv(t)
	plain := func(tail string) string {
		return "# D\n\n## 2026-10-20 — Walrus harbour choice <!-- id:ffff0001 -->\n\nwalrus harbour tide ledger " + tail + "\n\n" +
			"## 2026-10-19 — Quokka burrow choice <!-- id:ffff0002 -->\n\nquokka burrow gamma delta\n"
	}
	writeBank(t, "decisionLog.md", plain("original"))
	cwd := t.TempDir()
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	writeBank(t, "decisionLog.md", plain("edited after the index")) // ffff0001 now has no valid vector
	got := PromptRecall(context.Background(), "u", "walrus harbour tide ledger zeta", io.Discard)
	if !strings.Contains(got, "ffff0001") || strings.Contains(got, "ffff0002") {
		t.Errorf("the uncached keyword match must retain its full score and floor out the weaker semantic match:\n%s", got)
	}
}

// TestCapRecallLinesDropsWholeTrailingLinesToFitByteBudget: whole trailing lines drop until the block fits.
func TestCapRecallLinesDropsWholeTrailingLinesToFitByteBudget(t *testing.T) {
	long := "\n- " + strings.Repeat("x", 350)
	block, n := capRecallLines([]string{long, long, long})
	if n != 2 || strings.Count(block, long) != 2 {
		t.Fatalf("kept n=%d, want 2 whole lines: %q", n, block)
	}
	if len(block) > agentRecallBlockCapBytes {
		t.Errorf("block is %d bytes, want <= %d", len(block), agentRecallBlockCapBytes)
	}
	if strings.Contains(block, "x…") || strings.Contains(block, "…") {
		t.Errorf("a line was cut mid-string instead of dropped whole: %q", block)
	}
	if block, n := capRecallLines(nil); block != "" || n != 0 {
		t.Errorf("capRecallLines(nil) = (%q, %d), want (\"\", 0)", block, n)
	}
	if _, n := capRecallLines([]string{strings.Repeat("y", 2000)}); n != 0 {
		t.Errorf("a single line too big alone must drop to empty, got n=%d", n)
	}
}

const recallTask = "stage the deploy pipeline staging signoff rollout"

// seedRecallBank writes n id-marked, strongly matching home-bank blocks so AgentRecall has real hits to rank.
func seedRecallBank(t *testing.T, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Conventions\n\n")
	for i := range n {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d %s<!-- id:%08x -->\n", i, strings.Repeat("padding ", 40), i+1)
	}
	writeBank(t, "conventions.md", b.String())
}

// AgentRecall cites at most 3 ids and its block (whole lines only) never exceeds the byte budget.
func TestAgentRecallCappedAtThreeHitsAndByteBudget(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedRecallBank(t, 5) // more candidates than the k=3 cap
	block, ids := AgentRecall(context.Background(), "", recallTask)
	if len(ids) == 0 || len(ids) > 3 || strings.Count(block, "\n- ") != len(ids) {
		t.Errorf("ids=%v, want 1-3 matching the block's lines: %q", ids, block)
	}
	if len(block) > agentRecallBlockCapBytes || !strings.HasPrefix(block, "\n\n"+agentRecallMarker) {
		t.Errorf("block is %d bytes, want <= %d and marker-led: %q", len(block), agentRecallBlockCapBytes, block)
	}
}

// A home-bank hit comes back, and a marked task returns nothing.
func TestAgentRecallHitsTheBankAndSkipsMarkedTasks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedRecallBank(t, 1)
	block, ids := AgentRecall(context.Background(), "test-session", recallTask)
	if len(ids) != 1 || !strings.Contains(block, agentRecallMarker) {
		t.Errorf("ids=%v block=%q, want the one home-bank hit", ids, block)
	}
	if b, i := AgentRecall(context.Background(), "test-session", recallTask+" "+agentRecallMarker); b != "" || i != nil {
		t.Errorf("a task already carrying the marker must return nothing, got %q %v", b, i)
	}
}

// Access boosts break ties without admitting entries that fail relevance gates.
func TestPickTierAccessBoost(t *testing.T) {
	hit := func(id string, score float64, matched int) SearchHit {
		return SearchHit{Score: score, Matched: matched, Bank: "home", File: "decisionLog.md",
			Block: "## 2026-10-01 — Zebra quota " + id + " <!-- id:" + id + " -->\n\nzebra quota limiter fallback"}
	}
	ranked := []SearchHit{hit("aa000001", 10, 3), hit("bb000002", 10, 3)}
	picks, passed := pickTier(ranked, false, 3, nil, nil, map[string]int{"bb000002": 3}, nil)
	if !passed || len(picks) != 2 || picks[0].Entry.ID != "bb000002" {
		t.Fatalf("the returned-to entry must rank first: %+v (passed %v)", picks, passed)
	}
	picks, _ = pickTier(ranked, false, 3, nil, nil, nil, nil)
	if len(picks) != 2 || picks[0].Entry.ID != "aa000001" {
		t.Fatalf("without returns the stable keyword order must hold: %+v", picks)
	}

	weak := []SearchHit{hit("aa000001", 10, 3), hit("cc000003", 2, 1)}
	picks, _ = pickTier(weak, false, 3, nil, nil, map[string]int{"cc000003": recallAccessCap}, nil)
	if len(picks) != 1 || picks[0].Entry.ID != "aa000001" {
		t.Fatalf("a returned-to entry failing the gate must not be admitted: %+v", picks)
	}

	// A boosted top hit must not raise the relative floor: a hit at half the raw top still survives.
	half := []SearchHit{hit("aa000001", 10, 3), hit("cc000003", 5, 3)}
	picks, _ = pickTier(half, false, 3, nil, nil, map[string]int{"aa000001": recallAccessCap}, nil)
	if len(picks) != 2 {
		t.Fatalf("the boost lifted the floor over a qualifying hit: %+v", picks)
	}
}

// Session-root files age out with their session, but survive while the session's count- file is fresh.
func TestPruneSeenSessionRoots(t *testing.T) {
	isolatedConfig(t)
	state := StateDir()
	old := time.Now().Add(-2 * seenMaxAge)
	const stale, live = "stale-session", "live-session"
	for _, name := range []string{"root-" + stale, "root-" + stale + ".lock", "root-" + live, "root-" + live + ".lock", "count-" + stale, "count-" + live} {
		write(t, filepath.Join(state, name), "x")
		if name != "count-"+live {
			if err := os.Chtimes(filepath.Join(state, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	pruneSeen(filepath.Join(state, "recall-seen"))
	for name, wantGone := range map[string]bool{
		"root-" + stale: true, "root-" + stale + ".lock": true, "count-" + stale: true,
		"root-" + live: false, "root-" + live + ".lock": false,
	} {
		_, err := os.Stat(filepath.Join(state, name))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s gone=%v, want %v", name, gone, wantGone)
		}
	}
}

// Synthesized agent transcripts age out after seenMaxAge; a fresh one stays.
func TestPruneSeenAgentTranscripts(t *testing.T) {
	isolatedConfig(t)
	dir := filepath.Join(StateDir(), "agent-transcripts")
	oldF, newF := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "new.jsonl")
	write(t, oldF, "x")
	write(t, newF, "x")
	old := time.Now().Add(-seenMaxAge - 24*time.Hour)
	if err := os.Chtimes(oldF, old, old); err != nil {
		t.Fatal(err)
	}
	pruneSeen(filepath.Join(StateDir(), "recall-seen"))
	if _, err := os.Stat(oldF); !os.IsNotExist(err) {
		t.Errorf("8-day-old agent transcript survived the prune: %v", err)
	}
	if _, err := os.Stat(newF); err != nil {
		t.Errorf("fresh agent transcript was removed: %v", err)
	}
}

// Given a bank file session start cannot read, When resetSeen runs, Then its blocks leave the seen set (not shown).
func TestResetSeenDropsBlocksFromAnUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for this user")
	}
	tempConfig(t)
	const sid = "41111111-2222-4333-8444-555555555555"
	bank := filepath.Join(StoreDir(), "conventions.md")
	write(t, bank, "# C\n\n- focus rule shown at start\n")
	resetSeen(sid, "- focus rule shown at start\n")
	if len(seenBlocks(sid)) != 1 {
		t.Fatalf("readable file: seen %v, want its one block", seenBlocks(sid))
	}
	if err := os.Chmod(bank, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bank, 0o600) })
	resetSeen(sid, "")
	if seen := seenBlocks(sid); len(seen) != 0 {
		t.Errorf("unreadable file: seen %v, want empty (the surface could not show it)", seen)
	}
}

// --load cuts an over-budget block at a rune boundary within recallLoadCap and loads nothing once the budget is spent.
func TestRecallLoadTruncatesOnRuneBoundary(t *testing.T) {
	tempConfig(t)
	const id = "00000001"
	writeBank(t, "conventions.md", "# Conventions\n\n- Feedback: "+strings.Repeat("—", recallLoadCap)+" <!-- id:"+id+" -->\n")
	_, block, err := ShowBlock(id)
	if err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, []SearchHit{{ID: id}, {ID: id}}, 2)
	loaded := strings.Split(out.String(), "\n<!-- "+id+" -->\n")
	if len(loaded) < 2 || errw.Len() != 0 {
		t.Fatalf("want loaded blocks and no errors, got stdout %q stderr %q", out.String(), errw.String())
	}
	body := strings.TrimSuffix(loaded[1], "\n")
	room := recallLoadCap - len("\n<!-- "+id+" -->\n\n") // the cap counts each block's id wrapper too
	if out.Len() > recallLoadCap || !strings.HasPrefix(strings.TrimSpace(block), body) || !utf8.ValidString(body) ||
		len(body) <= room-utf8.UTFMax {
		t.Errorf("loaded %d bytes, wrote %d, want a valid-UTF-8 block prefix cut just under %d", len(body), out.Len(), room)
	}
}

// TestRecallLoadStopsAfterCut: a rune-boundary cut leaves 1-2 bytes of budget, which must not print the next hit.
func TestRecallLoadStopsAfterCut(t *testing.T) {
	tempConfig(t)
	const long, short = "00000001", "00000002"
	writeBank(t, "conventions.md", "# Conventions\n\n- Feedback: xx"+strings.Repeat("—", recallLoadCap)+" <!-- id:"+long+" -->\n"+
		"- Feedback: short follow-up fact <!-- id:"+short+" -->\n")
	_, block, err := ShowBlock(long)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneStart(strings.TrimSpace(block)[recallLoadCap]) {
		t.Fatal("fixture: the cap must fall inside a rune so the cut leaves budget over")
	}
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, []SearchHit{{ID: long}, {ID: short}}, 2)
	if strings.Contains(out.String(), "<!-- "+short+" -->") || errw.Len() != 0 {
		t.Errorf("a hit after the cut block was loaded as a fragment:\n%q\nstderr %q", out.String()[out.Len()-60:], errw.String())
	}
}

// TestRecallLoadSkipsWhatDoesNotFit: a too-big block is skipped whole, a smaller later one loads; id wrappers count.
func TestRecallLoadSkipsWhatDoesNotFit(t *testing.T) {
	tempConfig(t)
	facts := []string{strings.Repeat("m", 3000), strings.Repeat("b", 2000), "small fact"}
	for i := range 5 { // five blocks whose text fits the cap but whose wrappers do not
		facts = append(facts, strings.Repeat(string(rune('c'+i)), 780))
	}
	var bank strings.Builder
	var hits []SearchHit
	bank.WriteString("# Conventions\n\n")
	for i, fact := range facts {
		id := fmt.Sprintf("%08x", i+1)
		fmt.Fprintf(&bank, "- Feedback: %s <!-- id:%s -->\n", fact, id)
		hits = append(hits, SearchHit{ID: id})
	}
	writeBank(t, "conventions.md", bank.String())
	hits, even := hits[:3], hits[3:]
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, hits, len(hits))
	if got := out.String(); strings.Contains(got, hits[1].ID) || !strings.Contains(got, "small fact") {
		t.Errorf("want the 2000-byte block skipped and the small one loaded after it, got %d bytes:\n%.200s", len(got), got)
	}
	out.Reset()
	if recallLoad(&out, &errw, even, len(even)); out.Len() > recallLoadCap || errw.Len() != 0 {
		t.Errorf("wrote %d bytes, want at most %d; stderr %q", out.Len(), recallLoadCap, errw.String())
	}
}

// The expand header shows once per session and returns after resetSeen; a native topic hit skips its label.
func TestRecallExpandHeaderOncePerSessionAndNativeLabel(t *testing.T) {
	recallEnv(t)
	writeBank(t, "conventions.md", "# C\n\n- Zebra quota limiter fallback convention rule <!-- id:aaaa0005 -->\n\n"+
		"- Quokka burrow tunnel survey convention rule <!-- id:aaaa0006 -->\n")
	write(t, filepath.Join(StoreDir(), "notes", "zebra-topic.md"),
		"---\nname: zebra-topic\ndescription: zebra quota limiter fallback guidance\n---\nbody\n")
	const sid = "51111111-2222-4333-8444-555555555555"
	first := PromptRecall(context.Background(), sid, "zebra quota limiter fallback", io.Discard)
	second := PromptRecall(context.Background(), sid, "quokka burrow tunnel survey", io.Discard)
	if !strings.HasPrefix(first, "<!-- memory-recall; expand:") || !strings.HasPrefix(second, "<!-- memory-recall -->") {
		t.Fatalf("expand header must lead the first block only:\n%s\n---\n%s", first, second)
	}
	if strings.Contains(first, "[zebra-topic.md]") || !strings.Contains(first, "zebra-topic —") {
		t.Errorf("native topic line must drop its repeated label:\n%s", first)
	}
	if !strings.Contains(first, "[conventions.md]") {
		t.Errorf("typed-bank hit must keep its label:\n%s", first)
	}
	resetSeen(sid, "")
	if got := PromptRecall(context.Background(), sid, "quokka burrow tunnel survey", io.Discard); !strings.HasPrefix(got, "<!-- memory-recall; expand:") {
		t.Errorf("a restarted seen set must show the expand header again: %q", got)
	}
}

func TestRecallTraceSelectedUsesTheRealHeader(t *testing.T) {
	picksFor := func(titleLen int) []recallPick {
		body := strings.Repeat("alpha ", 60)
		mk := func(id uint64, title string) recallPick {
			return recallPick{Entry: Entry{ID: fmt.Sprintf("id%d", id), File: "f.md", Type: "convention", Body: body, Title: title},
				Hit: SearchHit{sum: id}}
		}
		return []recallPick{mk(1, "t"), mk(2, "t"), mk(3, strings.Repeat("p", titleLen))}
	}
	titleLen := -1 // a pointer that fits under the short header but not under the expand header
	for n := 1; n < 400 && titleLen < 0; n++ {
		short, _ := recallContext(picksFor(n), false)
		long, _ := recallContext(picksFor(n), true)
		if strings.Contains(short, "\n- "+picksFor(n)[2].Entry.pointer()) && !strings.Contains(long, "\n- "+picksFor(n)[2].Entry.pointer()) {
			titleLen = n
		}
	}
	if titleLen < 0 {
		t.Fatal("no pointer length separates the two headers")
	}
	picks := picksFor(titleLen)
	for _, tc := range []struct {
		expand bool
		want   string
	}{{false, "pointer"}, {true, "over cap"}} {
		tr := &recallTrace{Rows: []recallCandidate{{key: picks[2].Hit.key()}}}
		tr.selected(picks, tc.expand)
		if got := tr.Rows[0].Outcome; got != tc.want {
			t.Errorf("expand=%v: outcome %q, want %q", tc.expand, got, tc.want)
		}
	}
}
