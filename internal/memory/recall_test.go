package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestSearchBanksSurfacesRoutedReposOnlyWhenNamed: another repo's blocks only enter a search's results
// when the query names that repo.
func TestSearchBanksSurfacesRoutedReposOnlyWhenNamed(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	a := gitRepo(t, t.TempDir(), "proja")
	b := gitRepo(t, t.TempDir(), "projb")
	if _, err := EnsureRepoDir(a); err != nil {
		t.Fatal(err)
	}
	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(db, "decisionLog.md"), "# D\n\n## 2026-01-01 — deploy pipeline retries three times\n\nbecause rate limits\n")

	if hits := searchBanks(a, "deploy pipeline retries", 0); len(hits) != 0 {
		t.Errorf("unnamed repo leaked into search: %+v", hits)
	}
	hits := searchBanks(a, "projb deploy pipeline retries", 0)
	found := false
	for _, h := range hits {
		if h.Bank == "projb" {
			found = true
		}
	}
	if !found {
		t.Errorf("naming projb did not surface its blocks: %+v", hits)
	}
}

// TestSearchAllCoversEveryRepoStore: searchAll ranks blocks across every repo store, not just one root.
func TestSearchAllCoversEveryRepoStore(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	a := gitRepo(t, t.TempDir(), "proja")
	b := gitRepo(t, t.TempDir(), "projb")
	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(da, "decisionLog.md"), "# D\n\n## 2026-01-01 — proja retries the deploy pipeline\n\nbody\n")
	write(t, filepath.Join(db, "decisionLog.md"), "# D\n\n## 2026-01-01 — projb retries the deploy pipeline\n\nbody\n")

	hits := searchAll("retries the deploy pipeline", 0)
	banks := map[string]bool{}
	for _, h := range hits {
		banks[h.Bank] = true
	}
	if !banks["proja"] || !banks["projb"] {
		t.Errorf("searchAll missed a store, banks seen = %v", banks)
	}
}

// TestMemoryRecallHandlesCwdGracefully: an empty cwd falls back to os.Getwd and still recalls the home bank.
func TestMemoryRecallHandlesCwdGracefully(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	writeBank(t, "conventions.md", "# C\n\n- alpha bravo charlie delta strong\n")
	if got := PromptRecall("", t.TempDir(), "alpha bravo charlie delta", io.Discard); !strings.Contains(got, "strong") {
		t.Fatalf("PromptRecall with explicit cwd = %q, want the seeded hit", got)
	}
	if got := PromptRecall("", "", "alpha bravo charlie delta", io.Discard); !strings.Contains(got, "strong") {
		t.Fatalf("PromptRecall with empty cwd = %q, want the seeded hit", got)
	}
}

func writeBank(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(HomeBankDir(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
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
		if strings.HasPrefix(l, "- [home/") {
			bodies++
		}
	}
	if bodies != 2 || !strings.HasPrefix(lines[0], "- [home/conventions.md] aaaa0005") || !strings.HasPrefix(lines[1], "- [home/sessionHistory.md] aaaa0004") {
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
	cwd := t.TempDir()

	got := PromptRecall("s1", cwd, "zebra quota limiter fallback", io.Discard)
	if !strings.HasPrefix(got, "<!-- memory-recall:") {
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
	if again := PromptRecall("s1", cwd, "zebra quota limiter fallback", io.Discard); strings.Contains(again, "aaaa0005") || strings.Contains(again, "aaaa0004") {
		t.Errorf("bodies already in context must not repeat:\n%s", again)
	}

	if arch := PromptRecall("s2", cwd, "quokka burrow tunnel survey", io.Discard); !strings.Contains(arch, "aaaa0007") && !strings.Contains(arch, "Quokka burrow") {
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
	t.Setenv("CLAUDE_PROFILE", "fake") // NewSemantic reads the session profile's cache
	c := &EmbedClient{Profile: "fake", Model: "fake-gte", Namespace: "fake|h|fake-gte", EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
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

	base := PromptRecall("a", cwd, prompt, io.Discard)
	if strings.Contains(base, "bbbb0006") || strings.Contains(base, "bbbb0007") {
		t.Fatalf("fixture invalid: keyword-only already surfaces the gamma entries:\n%s", base)
	}
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	blended := PromptRecall("b", cwd, prompt, io.Discard)
	if !strings.Contains(blended, "bbbb0006") || !strings.Contains(blended, "bbbb0007") {
		t.Errorf("cosine must lift the gamma entries into the five slots:\n%s", blended)
	}
	if len(strings.Split(blended, "\n")) != 6 {
		t.Errorf("filters must still cap at 5 entries:\n%s", blended)
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
	want := PromptRecall("kw", cwd, prompt, io.Discard) // semantic on, fast endpoint
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "0")
	keyword := PromptRecall("kw0", cwd, prompt, io.Discard)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	if want == keyword {
		t.Fatalf("fixture invalid: blend and keyword results are identical:\n%s", keyword)
	}

	prevWait := embedRecallWait
	embedRecallWait = 100 * time.Millisecond
	t.Cleanup(func() { embedRecallWait = prevWait })
	useEmbedServer(t, slowVecServer(t, 10*time.Second))
	start := time.Now()
	got := PromptRecall("slow", cwd, prompt, io.Discard)
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
	cwd := t.TempDir()
	if first := PromptRecall("s1", cwd, "zebra quota limiter fallback", io.Discard); !strings.Contains(first, "aaaa0002") {
		t.Fatalf("setup: %q", first)
	}
	if again := PromptRecall("s1", cwd, "zebra quota limiter fallback", io.Discard); again != "" {
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
	if got := PromptRecall("k", cwd, prompt, io.Discard); got != "" {
		t.Fatalf("fixture invalid: keyword-only already answers: %q", got)
	}
	c := useEmbedServer(t, slowVecServer(t, 0))
	if _, _, err := embedChanged(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	got := PromptRecall("s", cwd, prompt, io.Discard)
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
	for _, h := range searchBanks(cwd, "quokka burrow", 0) {
		if !strings.Contains(h.Block, "dddd0006") {
			seen = append(seen, h)
		}
	}
	markSeen("sess", seen) // the five closest are already in context
	got := PromptRecall("sess", cwd, "hello world foo bar zeta", io.Discard)
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
	if first := PromptRecall("sm", cwd, prompt, io.Discard); !strings.Contains(first, "eeee0001") {
		t.Fatalf("setup: semantic match not recalled: %q", first)
	}
	if again := PromptRecall("sm", cwd, prompt, io.Discard); again != "" {
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
	got := PromptRecall("u", cwd, "walrus harbour tide ledger zeta", io.Discard)
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
	block, ids := AgentRecall("", t.TempDir(), recallTask)
	if len(ids) == 0 || len(ids) > 3 || strings.Count(block, "\n- ") != len(ids) {
		t.Errorf("ids=%v, want 1-3 matching the block's lines: %q", ids, block)
	}
	if len(block) > agentRecallBlockCapBytes || !strings.HasPrefix(block, "\n\n"+agentRecallMarker) {
		t.Errorf("block is %d bytes, want <= %d and marker-led: %q", len(block), agentRecallBlockCapBytes, block)
	}
}

// An empty cwd falls back to the process cwd: a home-bank hit still comes back, and a marked task returns nothing.
func TestAgentRecallHandlesEmptyCwd(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedRecallBank(t, 1)
	block, ids := AgentRecall("test-session", "", recallTask)
	if len(ids) != 1 || !strings.Contains(block, agentRecallMarker) {
		t.Errorf("empty cwd: ids=%v block=%q, want the one home-bank hit", ids, block)
	}
	if b, i := AgentRecall("test-session", "", recallTask+" "+agentRecallMarker); b != "" || i != nil {
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
