package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const reviseLog = `# Decision Log

## 2026-11-13 — Audit executes tests serially <!-- id:b0000002 -->
Supersedes a0000001. Tests now execute serially.

## 2026-11-13 — Names a missing entry <!-- id:c0000003 -->
Supersedes deadbeef. Unrelated gateway note.

## 2026-11-12 — First of a cycle <!-- id:d0000004 -->
Supersedes e0000005. Gateway cache note.

## 2026-11-12 — Second of a cycle <!-- id:e0000005 -->
Supersedes d0000004. Gateway cache note.

## 2026-11-12 — Keep the old shard layout for audit tests <!-- id:a0000001 -->
Shard layout audit tests run concurrently.
`

// TestSettleStrikesSupersededAndRecallFollows: settle strikes what a newer entry supersedes; recall serves the newer.
func TestSettleStrikesSupersededAndRecallFollows(t *testing.T) {
	tempConfig(t)
	path := Path("decisionLog.md")
	write(t, path, reviseLog)
	if _, err := settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, path)
	want := "~~Keep the old shard layout for audit tests~~ <!-- id:a0000001 -->\nReplaced by b0000002 (2026-11-13)."
	if !strings.Contains(got, want) {
		t.Fatalf("old entry not struck with its pointer and id kept; want %q in:\n%s", want, got)
	}
	assertUnmarked(t, got, "c0000003", "d0000004", "e0000005") // a dangling reference and a cycle
	if _, err := settleBank(time.Now()); err != nil || mustRead(t, path) != got {
		t.Fatalf("second settle changed the file (err %v)", err)
	}
	assertRecallFollows(t)
}

// assertRecallFollows: recall serves the new entry for the old one's terms; review reports the two bad refs.
func assertRecallFollows(t *testing.T) {
	t.Helper()
	picks, _ := recallPicks(context.Background(), "", "shard layout audit run concurrently", false, false)
	if len(picks) != 1 || picks[0].Entry.ID != "b0000002" {
		t.Fatalf("recall = %+v; want only the replacing entry b0000002", picks)
	}
	if p := reviseBankProblems(); len(p) != 2 || !strings.Contains(strings.Join(p, "\n"), "deadbeef") {
		t.Fatalf("review problems = %q; want the dangling id and the cycle", p)
	}
}

// assertUnmarked fails when any of ids has a strike or a pointer in log.
func assertUnmarked(t *testing.T, log string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if b := blockOf(t, log, id); strings.Contains(b, "~~") || replacedBy(b) != "" {
			t.Errorf("%s was marked", id)
		}
	}
}

// blockOf is the block of the decision log text carrying id.
func blockOf(t *testing.T, log, id string) string {
	t.Helper()
	_, blocks := splitHeadingBlocks(log)
	for _, b := range blocks {
		if ownerID(b) == id {
			return b
		}
	}
	t.Fatalf("no block %s", id)
	return ""
}

const partlyStruckLog = `# Decision Log

## 2026-11-13 — Count blocked results; ~~embeddings stay off~~ (see 5c07d1a8) <!-- id:9d40e6c1 -->
Replaced by 5c07d1a8 (2026-11-13).
Dream stamps a retry when blocked and nothing edited.

## 2026-11-13 — Turn embeddings on <!-- id:5c07d1a8 -->
Supersedes 9d40e6c1. Embeddings are on.
`

// TestPartlyStruckTitleStaysLive: a partly struck title loses its pointer at settle and stays in recall.
func TestPartlyStruckTitleStaysLive(t *testing.T) {
	tempConfig(t)
	path := Path("decisionLog.md")
	write(t, path, partlyStruckLog)
	if _, err := settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	if b := blockOf(t, mustRead(t, path), "9d40e6c1"); strings.Contains(b, "Replaced by") || isSupersededBlock(b) {
		t.Fatalf("partly struck entry kept a pointer or reads as replaced:\n%s", b)
	}
	picks, _ := recallPicks(context.Background(), "", "dream stamps retry blocked nothing edited", false, false)
	if len(picks) != 1 || picks[0].Entry.ID != "9d40e6c1" {
		t.Fatalf("recall = %+v; want the partly struck entry 9d40e6c1", picks)
	}
}

func TestReviseDeclarationGrammar(t *testing.T) {
	bullet := formatBankMarkdown(markReplaced("- "+strings.Repeat("Convention ", 20)+"<!-- id:bbbbbbbb -->\nBody.", "deadbeef", "2026-11-13"))
	if got := replacedBy(bullet); got != "deadbeef" {
		t.Errorf("formatted bullet pointer = %q in %q", got, bullet)
	}
	for _, c := range []struct{ name, body, ref, pointer string }{
		{"plain", "Supersedes aaaaaaaa.", "aaaaaaaa", ""},
		{"bullet", "- Supersedes aaaaaaaa.", "aaaaaaaa", ""},
		{"wrapped declaration", "Supersedes\n aaaaaaaa.", "aaaaaaaa", ""},
		{"inline sentence", "`Example. Supersedes aaaaaaaa.`", "", ""},
		{"quoted sentence", "\"Example. Supersedes aaaaaaaa.\"", "", ""},
		{"backtick id", "supersedes `aaaaaaaa`.", "aaaaaaaa", ""},
		{"quoted title", "SUPERSEDES \"quoted title\" (aaaaaaaa).", "aaaaaaaa", ""},
		{"inline example", "The example `Supersedes aaaaaaaa.` shows syntax.", "", ""},
		{"quoted example", "\"Supersedes aaaaaaaa.\"", "", ""},
		{"blockquote", "> Supersedes aaaaaaaa.", "", ""},
		{"fenced", "```\nSupersedes aaaaaaaa.\nReplaced by deadbeef.\n```", "", ""},
		{"quoted pointer", "\"Replaced by deadbeef.\"", "", ""},
		{"body pointer", "Example follows.\nReplaced by deadbeef.", "", ""},
		{"wrapped pointer", "  Replaced by\n  deadbeef (2026-11-13).\nBody.", "", "deadbeef"},
		{"slug id", "Supersedes `b5ad9e07-semorder`.", "b5ad9e07-semorder", ""},
		{"sentence", "Changed direction. Supersedes aaaaaaaa.", "aaaaaaaa", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := "## Title <!-- id:bbbbbbbb -->\n" + c.body
			if got := strings.Join(revisedIDs(b, "bbbbbbbb"), ","); got != c.ref {
				t.Errorf("refs = %q, want %q", got, c.ref)
			}
			if got := replacedBy(b); got != c.pointer {
				t.Errorf("pointer = %q, want %q", got, c.pointer)
			}
		})
	}
}

func TestReviseBlockPosition(t *testing.T) {
	for _, name := range []string{"decisionLog.md", "conventions.md"} {
		t.Run(name, func(t *testing.T) {
			real := "## Real <!-- id:aaaaaaaa -->\nOriginal body."
			first := "## Example <!-- id:bbbbbbbb -->\n```\n" + real + "\n```"
			d := newBankDoc(name, "# Bank\n\n"+first+"\n\n"+real+"\n")
			d.setBlock(1, markReplaced(d.blocks[1], "cccccccc", ""))
			_, blocks := entryBlocks(name, d.text)
			if blocks[0] != first || replacedBy(blocks[1]) != "cccccccc" {
				t.Fatalf("wrong block edited:\n%s", d.text)
			}
		})
	}
}

func TestSettleRecoversPendingJournal(t *testing.T) {
	tempConfig(t)
	path := Path("decisionLog.md")
	write(t, path, reviseLog)
	recovered := reviseLog + "\n## Recovered <!-- id:f0000006 -->\nRecovered journal payload.\n"
	rel, err := filepath.Rel(StoreDir(), path)
	if err != nil {
		t.Fatal(err)
	}
	j, err := newJournal(StoreRepo(), "pending fixture", []Change{{Rel: filepath.ToSlash(rel), Data: []byte(recovered)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.save(); err != nil {
		t.Fatal(err)
	}
	if _, err = settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mustRead(t, path), "Recovered journal payload.") {
		t.Fatal("pending journal payload lost")
	}
}

func TestReviseRecallBoundaries(t *testing.T) {
	for _, mode := range []string{"missing", "eight hops", "nine hops", "routed", "all"} {
		t.Run(mode, func(t *testing.T) {
			tempConfig(t)
			if mode == "routed" || mode == "all" {
				assertReviseCorpus(t)
				return
			}
			assertReviseChain(t, mode)
		})
	}
}

func assertReviseCorpus(t *testing.T) {
	t.Helper()
	old := markReplaced("## Old <!-- id:aaaaaaaa -->\nunique quasar shard layout audit.", "bbbbbbbb", "")
	write(t, Path("decisionLog.md"), rejoin("# Log", []string{old, "## New <!-- id:bbbbbbbb -->\nNew policy."}))
	hits := RecallBank("projb unique quasar shard layout audit", 1)
	if len(hits) != 1 || hits[0].ID != "bbbbbbbb" {
		t.Fatalf("recall = %+v, want bbbbbbbb", hits)
	}
}

func assertReviseFallback(t *testing.T, mode string, hits []SearchHit) {
	t.Helper()
	if mode == "eight hops" {
		return
	}
	if !hits[0].UnresolvedReplacement {
		t.Fatal("unresolved original was not flagged")
	}
	if candidates, _ := recallTier(hits, false, 1); len(candidates) != 1 {
		t.Fatal("recall filtered unresolved original")
	}
}

func assertReviseChain(t *testing.T, mode string) {
	t.Helper()
	hops := reviseHopCap
	if mode == "nine hops" {
		hops++
	}
	var blocks []string
	for i := 0; i <= hops; i++ {
		b := fmt.Sprintf("## Entry <!-- id:%08x -->\nterminal payload", i+1)
		if i < hops {
			b = markReplaced(b, fmt.Sprintf("%08x", i+2), "")
		}
		blocks = append(blocks, b)
	}
	path := Path("decisionLog.md")
	write(t, path, rejoin("# Log", blocks))
	files := []corpusFile{{Bank: "local", Path: path, Mode: segEntries}}
	if mode == "missing" {
		files = nil
	}
	got := followReplaced([]SearchHit{{ID: "00000001", Block: blocks[0], Score: 5, Matched: 3}}, files, loadSegments(files, true))
	want := fmt.Sprintf("%08x", hops+1)
	if mode != "eight hops" {
		want = "00000001"
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("follow = %+v, want %s", got, want)
	}
	assertReviseFallback(t, mode, got)
}

// TestReviseResolvesRefsOfLoadedArchives: a reference carried by an archive loaded for another is resolved too.
func TestReviseResolvesRefsOfLoadedArchives(t *testing.T) {
	tempConfig(t)
	write(t, Path("decisionLog.md"), "# Decision Log\n\n## 2026-11-16 — Live <!-- id:a1000001 -->\nSupersedes b2000002. Live.\n")
	write(t, Sub("archive", "decisionLog-2026-11.md"), "## 2026-11-10 — Mid <!-- id:b2000002 -->\nSupersedes c3000003. Mid.\n")
	write(t, Sub("archive", "decisionLog-2026-10.md"), "## 2026-10-01 — Old <!-- id:c3000003 -->\nOld.\n")
	if p := reviseBankProblems(); strings.Contains(strings.Join(p, "\n"), "unknown id") {
		t.Fatalf("review problems = %q; want no unknown id", p)
	}
}

func TestLocateBlocksSlugAlias(t *testing.T) {
	d := &bankDoc{
		blocks: []string{"## Old <!-- id:b5ad9e07-semorder --> <!-- id:23994edb -->\nbody"},
		ids:    []string{"23994edb"},
	}
	loc := locateBlocks([]*bankDoc{d})
	for _, c := range []struct {
		id   string
		want bool
	}{{"b5ad9e07-semorder", true}, {"23994edb", true}, {"b5ad9e07-nope", false}, {"b5ad9e07", false}} {
		if _, ok := loc[c.id]; ok != c.want {
			t.Errorf("resolve %q = %v, want %v", c.id, ok, c.want)
		}
	}
	if got := missingRefs([]*bankDoc{{blocks: d.blocks, ids: d.ids, refs: [][]string{{"b5ad9e07-semorder", "b5ad9e07-nope"}}}}); len(got) != 1 || !strings.Contains(got[0], "nope") {
		t.Errorf("missingRefs = %v", got)
	}
}
