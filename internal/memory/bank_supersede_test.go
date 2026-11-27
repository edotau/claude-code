package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
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

// Settle's strike rewrites only the replaced block; unformatted blocks and the header keep their bytes.
func TestSettleSupersedePreservesOtherBlocks(t *testing.T) {
	tempConfig(t)
	long := strings.Repeat("unrelated text with irregular   spaces ", 5)
	keep := []string{"# Log\n>  keep   header\n\n", "## New <!-- id:bbbbbbbb -->\nSupersedes aaaaaaaa.\n\n\n",
		"## Other <!-- id:cccccccc -->\n" + long + "\n\n\n"}
	path := Path("decisionLog.md")
	write(t, path, strings.Join(keep, "")+"## Old <!-- id:aaaaaaaa -->\nOld body.\n")
	if _, err := settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	after := mustRead(t, path)
	if !strings.HasPrefix(after, strings.Join(keep, "")) {
		t.Errorf("untouched bytes changed:\n%s", after)
	}
	if !strings.Contains(after, "~~Old~~ <!-- id:aaaaaaaa -->\nReplaced by bbbbbbbb.") {
		t.Errorf("old entry was not marked: %s", after)
	}
}

// A struck conventions title that wraps keeps its id on line 0 and its pointer readable; neighbours keep their bytes.
func TestSettleSupersedeWrappedConventionKeepsPointer(t *testing.T) {
	tempConfig(t)
	old := "- User: " + strings.TrimSpace(strings.Repeat("abcd ", 18)) + " <!-- id:aaaa1111 -->\n" // fits until struck
	other := "- User:   odd " + strings.Repeat("legacy  text ", 14) + "<!-- id:cccc3333 -->\n"
	path := Path("conventions.md")
	write(t, path, "# C\n\n"+old+"\n"+other+"\n- User: new rule <!-- id:bbbb2222 -->\n  Supersedes aaaa1111.\n")
	if _, err := settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	after := mustRead(t, path)
	if !strings.Contains(after, "\n"+other+"\n- User: new rule") {
		t.Errorf("untouched entry changed:\n%s", after)
	}
	d := ParseDoc("conventions.md", []byte(after))
	i := slices.IndexFunc(d.Entries, func(e DocEntry) bool { return e.ID == "aaaa1111" })
	if i < 0 {
		t.Fatalf("struck entry lost its id:\n%s", after)
	}
	block := strings.TrimRight(d.Entries[i].block(), "\n")
	if replacedBy(block) != "bbbb2222" || len(d.Entries[i].Lines) < 3 {
		t.Errorf("pointer unreadable or title not wrapped: %q", block)
	}
	assertLintClean(t, block)
	if _, err := settleBank(time.Now()); err != nil || mustRead(t, path) != after {
		t.Errorf("second settle changed the file (err %v)", err)
	}
}

// setBlock splices at the loaded offsets: fenced look-alikes, CRLF, blank runs and a final unterminated block survive.
func TestSetBlockSplicesInPlace(t *testing.T) {
	head := "# C\r\n\n```\n- not an entry\n```\n\n"
	blocks := []string{"- one\r\n  more", "- two", "- three"}
	text := head + blocks[0] + "\n\n\n" + blocks[1] + "\n" + blocks[2]
	d := newBankDoc(Path("conventions.md"), text)
	if len(d.blocks) != 3 || d.blocks[0] != blocks[0] {
		t.Fatalf("blocks = %q", d.blocks)
	}
	d.setBlock(0, "- ONE, longer now")
	d.setBlock(2, "- THREE")
	d.setBlock(1, "- SECOND, longer")
	d.setBlock(0, "- FIRST")
	want := head + "- FIRST\n\n\n- SECOND, longer\n- THREE"
	if d.text != want || !d.dirty {
		t.Errorf("text = %q\nwant  %q", d.text, want)
	}
}

func TestSupersedePreservesInlineIDExample(t *testing.T) {
	text := "- before ``<!-- id:11111111 -->`` after <!-- id:12345678 -->\n"
	d := ParseDoc("conventions.md", []byte(text))
	out, rs := Apply(d, Batch{Ops: []Op{{Op: "supersede", ID: "12345678", Read: contentRef(d.Entries[0].block()), Text: "new"}}}, testEnv())
	old := out.Entries[0].block()
	if rs[0].Outcome != DreamOK || replacedBy(old) != rs[0].ID || !strings.Contains(old, "~~before ``<!-- id:11111111 -->`` after~~") {
		t.Fatalf("inline example confused strike or pointer: %+v\n%s", rs, out.Render())
	}
}

func TestWriteOffsetsSkipFrontmatter(t *testing.T) {
	head := "---\nname: example\n## fake <!-- id:11111111 -->\n---\n\n# Log\n\n"
	entry := "## Real <!-- id:12345678 -->\nbody"
	d := newBankDoc("decisionLog.md", head+entry)
	if len(d.blocks) != 1 || insertionOffset("decisionLog.md", head+entry) != len(head) {
		t.Fatalf("frontmatter comment became an entry: %q, %v", d.blocks, d.starts)
	}
	d.setBlock(0, "## Changed <!-- id:12345678 -->\nbody")
	if !strings.HasPrefix(d.text, head) || strings.HasSuffix(d.text, "\n") {
		t.Errorf("header changed or trailing newline added: %q", d.text)
	}
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
	hits := SearchBank("projb unique quasar shard layout audit", 1)
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
	got := followReplaced([]SearchHit{{ID: "00000001", Block: blocks[0], Score: 5, Matched: 3}}, files, loadSegments(files, true, nil))
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
	write(t, paths.MemorySub("archive", "decisionLog-2026-11.md"), "## 2026-11-10 — Mid <!-- id:b2000002 -->\nSupersedes c3000003. Mid.\n")
	write(t, paths.MemorySub("archive", "decisionLog-2026-10.md"), "## 2026-10-01 — Old <!-- id:c3000003 -->\nOld.\n")
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

func oldFindBlockByID(id string, files []corpusFile, segs []segView) (SearchHit, bool) {
	marker := "<!-- id:" + id + " -->"
	for i, seg := range segs {
		if seg == nil {
			continue
		}
		for d := range seg.Docs() {
			if block, _ := seg.Doc(d); strings.Contains(block, marker) && ownerID(block) == id {
				f := files[i]
				return SearchHit{Bank: f.Bank, File: filepath.Base(f.Path), Block: block, ID: id}, true
			}
		}
	}
	return SearchHit{}, false
}

// TestBlockFinderMatchesScan: the lookup answers as the per-id scan did, duplicates and a missing id included.
func TestBlockFinderMatchesScan(t *testing.T) {
	tempConfig(t)
	write(t, Path("decisionLog.md"), "# Log\n\n## A <!-- id:a1000001 -->\nfirst\n\n## B <!-- id:b2000002 -->\nb\n\n## A again <!-- id:a1000001 -->\nsecond\n")
	write(t, Path("progress.md"), "# P\n\n## C <!-- id:a1000001 -->\nthird\n\n## D <!-- id:c3000003 -->\nd\n")
	files := []corpusFile{
		{Bank: "local", Path: Path("decisionLog.md"), Mode: segEntries},
		{Bank: "local", Path: Path("progress.md"), Mode: segEntries},
		{Bank: "local", Path: Path("gone.md"), Mode: segEntries},
	}
	segs := loadSegments(files, true, nil)
	find := blockFinder(files, segs)
	for _, id := range []string{"a1000001", "b2000002", "c3000003", "deadbeef", ""} {
		want, wok := oldFindBlockByID(id, files, segs)
		got, ok := find(id)
		if ok != wok || got != want {
			t.Errorf("id %q: got %+v %v, want %+v %v", id, got, ok, want, wok)
		}
	}
}

// pinPointerForms checks replacedBy and dropPointer take tok as a pointer ref exactly when want is non-empty.
func pinPointerForms(t *testing.T, tok, want string) {
	t.Helper()
	for _, date := range []string{"", " (2026-11-25)"} {
		if got := replacedBy("## T <!-- id:11111111 -->\nReplaced by " + tok + date + "."); got != want {
			t.Errorf("replacedBy(%q%s) = %q, want %q", tok, date, got, want)
		}
	}
	if dropped := dropPointer("a\n  Replaced by "+tok+".") == "a"; dropped != (want != "") {
		t.Errorf("dropPointer(%q) dropped = %v, want %v", tok, dropped, want != "")
	}
}

// TestRefTokenSitesPinned pins which id tokens each supersede matcher takes; the sites differ on purpose.
func TestRefTokenSitesPinned(t *testing.T) {
	const head = "## T <!-- id:11111111 -->\n"
	for _, c := range []struct {
		tok         string
		revises     string // revisedIDs: Supersedes <tok>.
		replaced    string // replacedBy: Replaced by <tok>.
		slugKey     bool   // locateBlocks: <!-- id:<tok> --> on the title line
		partlyKeeps bool   // partlyStruck: a (see <tok>) note is not live text
	}{
		{"abcdef12", "abcdef12", "abcdef12", false, true},
		{"zabcdef12", "zabcdef12", "zabcdef12", false, true},
		{"abcdef12-foo", "abcdef12-foo", "abcdef12-foo", true, true},
		{"abcdef12-foo-bar", "abcdef12-foo", "", false, false},
		{"zabcdef12-foo", "zabcdef12-foo", "zabcdef12-foo", false, true},
		{"ABCDEF12", "ABCDEF12", "", false, false},
		{"abcdef12-Foo", "abcdef12-Foo", "", false, false},
		{"abcdef1", "", "", false, false},
		{"abcdef123", "", "", false, false},
		{"abcdef12_x", "", "", false, false},
		{"abcdef12-", "abcdef12", "", false, false},
		{"abcdef12-foo_bar", "abcdef12", "", false, false},
	} {
		got := revisedIDs(head+"Supersedes "+c.tok+".", "")
		if want := c.revises; (want == "") != (len(got) == 0) || len(got) > 0 && got[0] != want {
			t.Errorf("revisedIDs(%q) = %v, want %q", c.tok, got, want)
		}
		pinPointerForms(t, c.tok, c.replaced)
		d := &bankDoc{blocks: []string{"## T <!-- id:" + c.tok + " -->\nbody"}, ids: []string{""}}
		if _, ok := locateBlocks([]*bankDoc{d})[c.tok]; ok != c.slugKey {
			t.Errorf("locateBlocks key %q = %v, want %v", c.tok, ok, c.slugKey)
		}
		if got := !partlyStruck("- ~~Old~~ (see " + c.tok + ") <!-- id:11111111 -->"); got != c.partlyKeeps {
			t.Errorf("partlyStruck (see %q) live-only = %v, want %v", c.tok, got, c.partlyKeeps)
		}
	}
	if got := replacedBy(head + "Replaced by abcdef12 now."); got != "" {
		t.Errorf("replacedBy with trailing text = %q, want none", got)
	}
}
