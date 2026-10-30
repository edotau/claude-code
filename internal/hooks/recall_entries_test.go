package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

func writeBank(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(memory.HomeBankDir(), rel)
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
	t.Setenv(memory.UpdateChildEnv, "")
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
}

// One spanning pass over a fixture bank: struck entries drop, decisions lead newest first, two bodies then
// expandable pointers, and archive answers only when nothing current matches.
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

	got := recallAs(t, "s1", "zebra quota limiter fallback", cwd)
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
	bodies := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "- [home/") {
			bodies++
		}
	}
	if bodies != 2 || !strings.HasPrefix(lines[0], "- [home/decisionLog.md] aaaa0002") || !strings.HasPrefix(lines[1], "- [home/decisionLog.md] aaaa0001") {
		t.Errorf("want 2 bodies, newest decision first:\n%s", got)
	}
	pointer := regexp.MustCompile(`^- (\S+) · (\d{4}-\d{2}-\d{2}|undated) · \S`)
	for _, l := range lines[2:] {
		m := pointer.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("not an `id · date · title` pointer: %q", l)
		}
		if _, _, err := memory.ShowBlock(m[1]); err != nil {
			t.Errorf("pointer id %s does not expand: %v", m[1], err)
		}
	}
	if !strings.Contains(lines[2], "aaaa0005") || !strings.Contains(lines[3], "aaaa0004") {
		t.Errorf("convention must outrank session narrative among pointers:\n%s", got)
	}
	if again := recallAs(t, "s1", "zebra quota limiter fallback", cwd); strings.Contains(again, "aaaa0002") || strings.Contains(again, "aaaa0001") {
		t.Errorf("bodies already in context must not repeat:\n%s", again)
	}

	if arch := recallAs(t, "s2", "quokka burrow tunnel survey", cwd); !strings.Contains(arch, "aaaa0007") && !strings.Contains(arch, "Quokka burrow") {
		t.Errorf("archive must answer when nothing current matches: %q", arch)
	}
}

// vecServer embeds a text as [count(gamma|zeta), count(delta), 0.1]; slow > 0 stalls every reply.
func vecServer(t *testing.T, slow time.Duration) *httptest.Server {
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

func useEmbedServer(t *testing.T, srv *httptest.Server) *memory.EmbedClient {
	t.Helper()
	t.Setenv("CLAUDE_PROFILE", "fake") // NewSemantic reads the session profile's cache
	c := &memory.EmbedClient{Profile: "fake", Model: "fake-gte", Namespace: "fake|h|fake-gte", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return memory.PostEmbeddings(ctx, srv.URL, texts, func(http.Header) error { return nil })
	}}
	prev := memory.OpenEmbedClient
	memory.OpenEmbedClient = func(context.Context, string, string) (*memory.EmbedClient, error) { return c, nil }
	t.Cleanup(func() { memory.OpenEmbedClient = prev })
	return c
}

// seven keyword-equal decisions, five slots: the two the endpoint says are closest to the prompt must win a slot
// only when semantic rank is on.
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

	base := recallAs(t, "a", prompt, cwd)
	if strings.Contains(base, "bbbb0006") || strings.Contains(base, "bbbb0007") {
		t.Fatalf("fixture invalid: keyword-only already surfaces the gamma entries:\n%s", base)
	}
	c := useEmbedServer(t, vecServer(t, 0))
	if _, _, err := memory.EmbedEntries(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	blended := recallAs(t, "b", prompt, cwd)
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
	if _, _, err := memory.EmbedEntries(context.Background(), cwd, useEmbedServer(t, vecServer(t, 0))); err != nil {
		t.Fatal(err)
	}
	want := recallAs(t, "kw", prompt, cwd) // semantic on, fast endpoint
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "0")
	keyword := recallAs(t, "kw0", prompt, cwd)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	if want == keyword {
		t.Fatalf("fixture invalid: blend and keyword results are identical:\n%s", keyword)
	}

	useEmbedServer(t, vecServer(t, 10*time.Second))
	start := time.Now()
	got := recallAs(t, "slow", prompt, cwd)
	if el := time.Since(start); el > 1800*time.Millisecond {
		t.Errorf("recall took %s with a stalled endpoint, budget is ~1.2s", el)
	}
	if got != keyword {
		t.Errorf("timeout must fall back to the keyword result:\n got: %s\nwant: %s", got, keyword)
	}
}

// The session surface carries no all-repos catalog rows, however many repos the catalog lists.
func TestSessionStartCarriesNoCatalogRows(t *testing.T) {
	recallEnv(t)
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n## Repos\n\n")
	for i := range 120 {
		fmt.Fprintf(&cat, "- repo-%03d — some description of the repo · last 2026-10-29 → repo-%03d/MEMORY.md\n", i, i)
	}
	if err := os.MkdirAll(memory.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory.StoreDir(), memory.CatalogFile), []byte(cat.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"cwd": t.TempDir(), "session_id": "probe-x"})
	var out bytes.Buffer
	if SessionStart(bytes.NewReader(raw), &out) != ExitProceed {
		t.Fatal("SessionStart failed")
	}
	if strings.Contains(out.String(), "repo-000") || strings.Contains(out.String(), "repo-119") {
		t.Errorf("catalog rows injected:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "other repos:") || out.Len() > 1500 {
		t.Errorf("want the one-line pointer and a small surface, got %d bytes:\n%s", out.Len(), out.String())
	}
}

// Once the current decision is in context, the same prompt must not fall back to an older archived one.
func TestRecallNoArchiveFallbackWhenCurrentIsSeen(t *testing.T) {
	recallEnv(t)
	writeBank(t, "decisionLog.md", "# D\n\n## 2026-10-20 — Zebra quota limiter current <!-- id:aaaa0002 -->\n\nzebra quota limiter fallback current\n")
	writeBank(t, "archive/decisionLog-2026-09.md", "# D\n\n## 2026-09-01 — Zebra quota limiter outdated <!-- id:aaaa0006 -->\n\nzebra quota limiter fallback outdated\n")
	cwd := t.TempDir()
	if first := recallAs(t, "s1", "zebra quota limiter fallback", cwd); !strings.Contains(first, "aaaa0002") {
		t.Fatalf("setup: %q", first)
	}
	if again := recallAs(t, "s1", "zebra quota limiter fallback", cwd); again != "" {
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
	if got := recallAs(t, "k", prompt, cwd); got != "" {
		t.Fatalf("fixture invalid: keyword-only already answers: %q", got)
	}
	c := useEmbedServer(t, vecServer(t, 0))
	if _, _, err := memory.EmbedEntries(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	got := recallAs(t, "s", prompt, cwd)
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
	c := useEmbedServer(t, vecServer(t, 0))
	if _, _, err := memory.EmbedEntries(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	var seen []memory.SearchHit
	for _, h := range memory.SearchBanks(cwd, "quokka burrow", 0) {
		if !strings.Contains(h.Block, "dddd0006") {
			seen = append(seen, h)
		}
	}
	memory.MarkSeen("sess", seen) // the five closest are already in context
	got := recallAs(t, "sess", "hello world foo bar zeta", cwd)
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
	c := useEmbedServer(t, vecServer(t, 0))
	if _, _, err := memory.EmbedEntries(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	const prompt = "hello world foo bar zeta"
	if first := recallAs(t, "sm", prompt, cwd); !strings.Contains(first, "eeee0001") {
		t.Fatalf("setup: semantic match not recalled: %q", first)
	}
	if again := recallAs(t, "sm", prompt, cwd); again != "" {
		t.Errorf("seen semantic match must not let the archive answer:\n%s", again)
	}
}

// An entry edited since the index has no vector; it must not outscore and so floor out a strong semantic match.
func TestRecallUncachedEntryDoesNotFloorOutSemanticMatch(t *testing.T) {
	recallEnv(t)
	plain := func(tail string) string {
		return "# D\n\n## 2026-10-20 — Walrus harbour choice <!-- id:ffff0001 -->\n\nwalrus harbour tide ledger " + tail + "\n\n" +
			"## 2026-10-19 — Quokka burrow choice <!-- id:ffff0002 -->\n\nquokka burrow gamma delta\n"
	}
	writeBank(t, "decisionLog.md", plain("original"))
	cwd := t.TempDir()
	c := useEmbedServer(t, vecServer(t, 0))
	if _, _, err := memory.EmbedEntries(context.Background(), cwd, c); err != nil {
		t.Fatal(err)
	}
	writeBank(t, "decisionLog.md", plain("edited after the index")) // ffff0001 now has no valid vector
	got := recallAs(t, "u", "walrus harbour tide ledger zeta", cwd)
	if !strings.Contains(got, "ffff0001") || !strings.Contains(got, "ffff0002") {
		t.Errorf("both the uncached keyword match and the semantic match must surface:\n%s", got)
	}
}
