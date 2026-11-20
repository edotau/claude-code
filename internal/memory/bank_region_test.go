package memory

import (
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/paths"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Only a line-start "---" closes front matter, so an indented one inside a block scalar is left alone.
func TestStripFrontmatterBlockScalarDashes(t *testing.T) {
	t.Parallel()
	in := "---\ntitle: x\nbody: |\n  line1\n  ---\n  line3\n---\nrest\n"
	if got := stripFrontmatter(in); got != "rest\n" {
		t.Errorf("stripFrontmatter(block scalar with --- line) = %q, want %q", got, "rest\n")
	}
}

// A body with no leading "---\n" fence passes through unchanged.
func TestStripFrontmatterNoFrontmatter(t *testing.T) {
	t.Parallel()
	in := "# Just a heading\nbody\n"
	if got := stripFrontmatter(in); got != in {
		t.Errorf("stripFrontmatter(no frontmatter) = %q, want unchanged %q", got, in)
	}
}

var indexNow = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

// seedNotes writes n feedback notes f00..: note i is modified i days after 2026-01-01.
func seedNotes(t *testing.T, n int, pinned ...int) {
	t.Helper()
	for i := range n {
		pin := ""
		if slices.Contains(pinned, i) {
			pin = "  pinned: true\n"
		}
		writeNote(t, "feedback", fmt.Sprintf("f%02d.md", i), fmt.Sprintf(
			"---\nname: f%02d\ndescription: hook %d\nmetadata:\n  modified: %s\n%s---\nbody\n", i, i, time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339), pin))
	}
}

func refreshIndex(t *testing.T) (indexReport, string) {
	t.Helper()
	rep, err := refreshNativeIndex(indexNow, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rep, mustRead(t, NativeIndex())
}

func readIfExistsString(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// testDecode reads index.md's records without production code: 4-space lines, closed by the end-of-entry line.
func testDecode(text string) []string {
	var out, cur []string
	for l := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(l, "    "):
			cur = append(cur, l[4:])
		case l == "<!-- end of entry -->" && cur != nil:
			out = append(out, strings.Join(cur, "\n"))
			cur = nil
		default:
			cur = nil
		}
	}
	return out
}

func stored(t *testing.T) []string {
	t.Helper()
	return testDecode(readIfExistsString(filepath.Join(paths.MemoryStoreDir(), catalogFile)))
}

func TestNativeIndexHoldsPinnedPlusNewestTwentyAndAMoreLine(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 50, 0) // the oldest note is pinned and also would never be among the newest
		rep, got := refreshIndex(t)
		if rep.Pinned != 1 || rep.Newest != 20 || rep.More != 29 || !strings.Contains(got, "- 29 more: see "+routerFile) {
			t.Errorf("report %+v:\n%s", rep, got)
		}
		if strings.Count(got, "](feedback/f00.md)") != 1 || !strings.Contains(got, "## Pinned\n- [f00]") {
			t.Errorf("pinned note must render once, under Pinned:\n%s", got)
		}
		if i, j := strings.Index(got, "(feedback/f49.md)"), strings.Index(got, "(feedback/f30.md)"); i < 0 || j < i || strings.Contains(got, "(feedback/f29.md)") {
			t.Errorf("newest 20 must run f49..f30 newest first:\n%s", got)
		}
		if len(bankIndexHint) > 120 || len(blockBegin) > 120 {
			t.Error("the hint and begin lines must stay within 120 characters")
		}
		if again, _ := refreshIndex(t); again.Bytes != rep.Bytes {
			t.Error("a second refresh with the same files and clock must not change the file")
		}
		writeNote(t, "feedback", "f00.md", "---\nname: f00\ndescription: hook 0\n---\n")
		if _, got = refreshIndex(t); strings.Contains(got, "## Pinned") {
			t.Errorf("an unpinned note must leave Pinned on the next refresh:\n%s", got)
		}
	})
}

func TestNativeIndexReportsLostPinUntilNextRefresh(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 3, 0, 1, 2)
		refreshIndex(t)
		writeNote(t, "feedback", "f00.md", "---\nname: f00\n---\n")
		if err := os.Remove(storePath("feedback/f01.md")); err != nil {
			t.Fatal(err)
		}
		want := "1 note(s) lost metadata.pinned: feedback/f00.md"
		rep, _ := refreshIndex(t)
		if rep.LostPins != want || !rep.notable() {
			t.Fatalf("refresh advisory = %q, want %q", rep.LostPins, want)
		}
		if got := indexProblems(indexNow); !slices.Equal(got, []string{want}) {
			t.Fatalf("review advisories = %q, want only %q", got, want)
		}
		refreshIndex(t)
		if got := indexProblems(indexNow); len(got) != 0 {
			t.Fatalf("review after acknowledgement = %q, want none", got)
		}
	})
}

func TestFolderPointersJoinTheNewestListByFileDate(t *testing.T) {
	runBank(t, func(t *testing.T) {
		const handoffs = 120 // past the soft cap: pinned by folder, they would push MEMORY.md over it
		seedNotes(t, 5)
		var lines []string
		for i := range handoffs {
			name := time.Date(2026, 2, 1+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02") + fmt.Sprintf("-h%03d.md", i)
			write(t, paths.MemorySub("handoffs", name), "handoff\n")
			lines = append(lines, fmt.Sprintf("- [h%03d](handoffs/%s) — handoff %d", i, name, i))
		}
		write(t, NativeIndex(), strings.Join(lines, "\n")+"\n")
		rep, got := refreshIndex(t)
		if rep.Pinned != 0 || rep.Newest != bankIndexNewest || rep.More != handoffs+5-bankIndexNewest || strings.Contains(got, "## Pinned") {
			t.Fatalf("folder pointers must not pin: %+v", rep)
		}
		last, cut := lines[handoffs-1], lines[handoffs-bankIndexNewest]
		if i, j := strings.Index(got, last), strings.Index(got, cut); i < 0 || j < i || strings.Contains(got, lines[handoffs-bankIndexNewest-1]) {
			t.Errorf("newest 20 must be the handoffs dated newest first:\n%s", got)
		}
		if !slices.Equal(stored(t), lines) {
			t.Error("every handoff pointer line must be stored in index.md, in order")
		}
	})
}

// hostileEntries are entry shapes that must survive byte-exact; the "lead" ones are text before the first bullet.
func hostileEntries() (bullets, lead []string) {
	bullets = []string{
		"- [a](notes/a.md) — crlf\r", "- [a](notes/a.md) — huge " + strings.Repeat("X", 50<<10),
		"- [a](notes/a.md) — wrapped\n  two\n\ttab\n        eight spaces", "* [a](notes/a.md) — star", "+ [b](notes/b.md) — plus",
		"12. [a](notes/a.md) — numbered", "[b](notes/b.md) — bare", "- [x](notes/a.md) first\n\n  second paragraph",
		"- continued entry\n## Pinned\n  tail", "- [x](notes/b.md) first\n# continuation heading\ncontinued",
		"- [gone](notes/gone.md) — unresolved\n  and wrapped", "- [a](notes/a.md) — dup", "- [a](notes/a.md) — dup",
		"- <!-- end of entry -->\n<!-- end of entry -->\n" + recordPrefix + "<!-- end of entry -->",
		"- " + blockEndPrefix + "0123456789abcdef -->\n## Unresolved\n## Newest 20",
	}
	lead = []string{
		"    - [a](notes/a.md) — indented like a stored line", "\tleading tab", "free text\nwith a lazy continuation",
		blockBegin, "<!-- end of entry -->", "## Pointer entries (appended by claude-code memory index, never rewritten)",
		"\ufeffinner bom",
	}
	return bullets, lead
}

// randomEntry is a bounded pseudo-random entry with a unique token, so each is told apart.
func randomEntry(r *rand.Rand, i int) string {
	marks := []string{"- ", "* ", "+ ", "3. ", ""}
	targets := []string{"notes/a.md", "notes/b.md", "handoffs/2026-03-01-h.md", "notes/missing.md"}
	bits := []string{"<!--", "-->", "## x", "`x`", "|", "é日本", "\t", "[y](z)", `\n`, "    ", "\r"}
	var b strings.Builder
	fmt.Fprintf(&b, "%s[r%d](%s) — s%d %s", marks[r.IntN(len(marks))], i, targets[r.IntN(len(targets))], i, bits[r.IntN(len(bits))])
	for range r.IntN(3) {
		fmt.Fprintf(&b, "\n%s%s", []string{"  cont ", "\n  para ", "# h ", "    code "}[r.IntN(4)], bits[r.IntN(len(bits))])
	}
	return b.String()
}

// appendModelText appends entries the way a model would, a lead entry first, with blank lines between some.
func appendModelText(r *rand.Rand, cur, lead string, entries []string) string {
	var b strings.Builder
	b.WriteString(cur)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		b.WriteString("\n")
	}
	if lead != "" {
		fmt.Fprintf(&b, "%s\n", lead)
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "%s%s\n", strings.Repeat("\n", r.IntN(3)), e)
	}
	return b.String()
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// roundTrip is the 30-session state: every entry written so far and the ones written during the last refresh.
type roundTrip struct {
	r              *rand.Rand
	bullets, lead  []string
	want, pending  map[string]bool
	next           int
	held           *os.File // a handle on MEMORY.md opened before a refresh, written after its rename
	injectedAtStep string
}

func (rt *roundTrip) entries() (lead string, add []string) {
	if len(rt.lead) > 0 && rt.r.IntN(2) == 0 {
		lead, rt.lead = rt.lead[0], rt.lead[1:]
		rt.want[lead] = true
	}
	for range 1 + rt.r.IntN(3) {
		e := randomEntry(rt.r, rt.next)
		if len(rt.bullets) > 0 {
			e, rt.bullets = rt.bullets[0], rt.bullets[1:]
		}
		rt.next++
		add, rt.want[e] = append(add, e), true
	}
	return lead, add
}

// inject writes one more entry at a refresh step: by path at most steps, through the held handle after the rename.
func (rt *roundTrip) inject(t *testing.T, step string) {
	if step != rt.injectedAtStep {
		return
	}
	e := fmt.Sprintf("- [late%d](notes/a.md) — written after %s", rt.next, step)
	rt.next++
	rt.pending[e] = true
	if rt.held != nil {
		if _, err := rt.held.WriteString("\n" + e + "\n"); err != nil {
			t.Fatal(err)
		}
		return
	}
	appendFile(t, NativeIndex(), e+"\n")
}

// sessionEvent is a whole-file edit an editor or the owner makes to MEMORY.md before some sessions.
func sessionEvent(t *testing.T, session int) {
	t.Helper()
	switch cur := readIfExistsString(NativeIndex()); session {
	case 15:
		write(t, NativeIndex(), "\ufeff"+cur)
	case 9:
		write(t, NativeIndex(), strings.ReplaceAll(cur, "\n", "\r\n"))
	case 13:
		write(t, NativeIndex(), "")
	}
}

// importSource is a source store with stored records, a pending MEMORY.md entry and a duplicate of a local entry.
func importSource(t *testing.T, rt *roundTrip, local string) []string {
	t.Helper()
	src := t.TempDir()
	rec, pend := fmt.Sprintf("- [imp%d](notes/a.md) — stored\n\n  remote", rt.next), fmt.Sprintf("* [imp%d](notes/b.md) — pending", rt.next)
	rt.next++
	var cat strings.Builder
	cat.WriteString("# old router\n- [x](notes/x.md) — not a record\n\n")
	for _, e := range []string{rec, local} {
		fmt.Fprintf(&cat, "    %s\n<!-- end of entry -->\n", strings.ReplaceAll(e, "\n", "\n    "))
	}
	write(t, filepath.Join(src, catalogFile), cat.String())
	write(t, filepath.Join(src, "MEMORY.md"), pend+"\n")
	rt.want[rec], rt.want[pend] = true, true
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := &importPlan{}
	p.readPointers(root)
	return p.pointers
}

func TestEntriesRoundTripAcrossSessions(t *testing.T) {
	runBank(t, func(t *testing.T) {
		note := "---\nname: a\ndescription: d\n---\n"
		writeNote(t, "notes", "a.md", note)
		writeNote(t, "notes", "b.md", "---\nname: b\ndescription: e\n---\n")
		write(t, paths.MemorySub("handoffs", "2026-03-01-h.md"), "h\n")
		bullets, lead := hostileEntries()
		rt := &roundTrip{r: rand.New(rand.NewPCG(7, 11)), bullets: bullets, lead: lead, want: map[string]bool{}, pending: map[string]bool{}}
		indexStep = func(step string) { rt.inject(t, step) }
		t.Cleanup(func() { indexStep = func(string) {} })
		for session := range 30 {
			sessionEvent(t, session)
			l, add := rt.entries()
			write(t, NativeIndex(), appendModelText(rt.r, readIfExistsString(NativeIndex()), l, add))
			var extra []string
			if session%10 == 9 {
				extra = importSource(t, rt, add[0])
			}
			rt.injectedAtStep = []string{"", "renamed", "appended", "installed", "held"}[session%5]
			rt.session(t, session, extra)
		}
		if mustRead(t, paths.MemorySub("notes", "a.md")) != note {
			t.Error("a note was edited")
		}
	})
}

// session runs one refresh with its injected write, a follow-up that folds it, and a rerun that must change nothing.
func (rt *roundTrip) session(t *testing.T, session int, extra []string) {
	t.Helper()
	cat := filepath.Join(paths.MemoryStoreDir(), catalogFile)
	if rt.injectedAtStep == "held" {
		f, err := os.OpenFile(NativeIndex(), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		rt.held, rt.injectedAtStep = f, "installed" // after the moved file was read: only the next step 1 sees it
		defer func() { _ = f.Close(); rt.held = nil }()
	}
	before := readIfExistsString(cat)
	if _, err := refreshNativeIndex(indexNow, false, extra); err != nil {
		t.Fatalf("session %d: %v", session, err)
	}
	rt.check(t, session, before, rt.want)
	rt.injectedAtStep = ""
	before = readIfExistsString(cat)
	refreshIndex(t)
	maps.Copy(rt.want, rt.pending) // written during the refresh: the follow-up one folds them
	rt.check(t, session, before, rt.want)
	cur, mem := readIfExistsString(cat), readIfExistsString(NativeIndex())
	if refreshIndex(t); readIfExistsString(cat) != cur || readIfExistsString(NativeIndex()) != mem {
		t.Fatalf("session %d: a refresh with no new input changed a byte", session)
	}
	rt.pending = map[string]bool{}
}

// check asserts after a refresh: each wanted entry stored exactly once, index.md only grew, MEMORY.md in bounds.
func (rt *roundTrip) check(t *testing.T, session int, before string, want map[string]bool) {
	t.Helper()
	after := readIfExistsString(filepath.Join(paths.MemoryStoreDir(), catalogFile))
	if !strings.HasPrefix(after, before) {
		t.Fatalf("session %d: index.md before the refresh is not a prefix of index.md after it", session)
	}
	got := map[string]int{}
	for _, e := range testDecode(after) {
		got[e]++
	}
	for e := range want {
		if got[e] != 1 {
			t.Fatalf("session %d: entry %.80q is stored %d times, want 1", session, e, got[e])
		}
	}
	if mem := readIfExistsString(NativeIndex()); strings.Count(mem, "\n") > bankIndexMaxLines || len(mem) > bankIndexMaxBytes {
		t.Fatalf("session %d: MEMORY.md is %d lines / %d bytes, over its bounds", session, strings.Count(mem, "\n"), len(mem))
	}
}

func TestOwnerEditsToIndexMdAreNeverDeletedOrMoved(t *testing.T) {
	edits := map[string]func(cat, older string) string{
		"delete an entry": func(c, _ string) string {
			return strings.Replace(c, "    - [a](notes/a.md) — one\n<!-- end of entry -->\n", "", 1)
		},
		"add a line inside": func(c, _ string) string {
			return strings.Replace(c, "<!-- end of entry -->\n", "<!-- end of entry -->\nowner\n", 1)
		},
		"add an indented":   func(c, _ string) string { return strings.Replace(c, "<!-- end of entry -->\n", "    owner code\n", 1) },
		"append at the end": func(c, _ string) string { return c + "- owner appended\n    - prefixed too\n" },
		"restore older":     func(_, o string) string { return o },
		"empty":             func(string, string) string { return "" },
		"crlf":              func(c, _ string) string { return strings.ReplaceAll(c, "\n", "\r\n") },
	}
	for name, edit := range edits {
		runBank(t, func(t *testing.T) {
			writeNote(t, "notes", "a.md", "---\nname: a\ndescription: d\n---\n")
			write(t, NativeIndex(), "- [a](notes/a.md) — one\n")
			refreshIndex(t)
			cat := filepath.Join(paths.MemoryStoreDir(), catalogFile)
			older := mustRead(t, cat)
			appendFile(t, NativeIndex(), "- [a](notes/a.md) — two\nfree text\n")
			refreshIndex(t)
			if name == "empty" {
				_ = os.RemoveAll(filepath.Join(paths.MemoryStoreDir(), ".git")) // no history is ever needed
			}
			owner := edit(mustRead(t, cat), older)
			write(t, cat, owner)
			refreshIndex(t)
			if got := mustRead(t, cat); !strings.HasPrefix(got, owner) {
				t.Errorf("%s: the owner's index.md is not a prefix of the refreshed one:\n%q\n%q", name, owner, got)
			}
			if name == "crlf" && !strings.Contains(mustRead(t, NativeIndex()), "— one\n") {
				t.Error("a CRLF-converted index.md must still read back its records")
			}
		})
	}
}

func TestEmptiedMemoryMdIsRegeneratedFromIndexMd(t *testing.T) {
	runBank(t, func(t *testing.T) {
		writeNote(t, "notes", "a.md", "---\nname: a\ndescription: d\n---\n")
		write(t, NativeIndex(), "- [a](notes/a.md) — kept\n")
		_, first := refreshIndex(t)
		write(t, NativeIndex(), "")
		if _, got := refreshIndex(t); got != first || !strings.Contains(got, "- [a](notes/a.md) — kept\n") {
			t.Errorf("an emptied MEMORY.md must come back from index.md:\n%s", got)
		}
	})
}

func TestNativeIndexClipsNewestFirstNeverPinned(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 150, seq(140)...)
		rep, got := refreshIndex(t)
		if !rep.Clipped || rep.Newest != 0 || rep.Pinned != 140 || strings.Count(got, "](feedback/") != 140 || !strings.Contains(rep.String(), "WARNING") {
			t.Errorf("pinned set over the soft cap must keep every pin and drop the newest list: %+v", rep)
		}
		seedNotes(t, 150, seq(60)...)
		if rep, _ = refreshIndex(t); rep.Clipped || rep.Lines > bankIndexMaxLines || rep.Bytes > bankIndexMaxBytes || rep.Pinned != 60 {
			t.Errorf("60 pins plus newest must fit by construction: %+v", rep)
		}
	})
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func TestPinsOverTheLoaderLimitAreClippedOldestFirstAndReported(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 230, seq(230)...)
		line := "- [Local](feedback/f00.md) — keep"
		write(t, NativeIndex(), line+"\n")
		rep, err := refreshNativeIndex(indexNow, false, nil)
		if !errors.Is(err, errIndexProblem) || !strings.Contains(err.Error(), "230 pinned") || !strings.Contains(err.Error(), "unpin") {
			t.Fatalf("err=%v, want the pin overflow with its advice", err)
		}
		mem := mustRead(t, NativeIndex())
		if rep.Lines > loaderMaxLines || len(mem) > loaderMaxBytes || !strings.Contains(mem, "(feedback/f229.md)") || !slices.Contains(stored(t), line) {
			t.Errorf("MEMORY.md must fit the loader with the newest pins, and the entry stay stored: %+v", rep)
		}
	})
}

func TestPointerAddedInsideAnEditedBlockIsFolded(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 30, 0)
		_, got := refreshIndex(t)
		top, pinned, tail := "- [f05](feedback/f05.md) — top", "- [f06](feedback/f06.md) — in pinned", "- [f07](feedback/f07.md) — after"
		got = top + "\n" + strings.Replace(got, "## Pinned\n", "## Pinned\n"+pinned+"\n", 1) + tail + "\n"
		write(t, NativeIndex(), got)
		if rep, _ := refreshIndex(t); rep.Folded != 3 || rep.Unresolved != 0 {
			t.Errorf("report %+v, want the three added lines folded and no generated line", rep)
		}
		if s := stored(t); !slices.Equal(s, []string{top, pinned, tail}) {
			t.Errorf("index.md = %q", s)
		}
	})
}

func TestNotesWithCRLFBOMOrInlineMetadataAreListedNeverEdited(t *testing.T) {
	runBank(t, func(t *testing.T) {
		notes := map[string]string{"crlf.md": "---\r\nname: c\r\ndescription: cd\r\n---\r\nbody\r\n", "bom.md": "\ufeff---\nname: b\n---\n",
			"inline.md": "---\nname: i\ndescription: id\nmetadata: {pinned: true}\n---\n"}
		for f, body := range notes {
			writeNote(t, "notes", f, body)
		}
		write(t, NativeIndex(), "- [c](notes/crlf.md) — kept\n")
		_, got := refreshIndex(t)
		for _, want := range []string{"(notes/crlf.md) — kept", "(notes/bom.md) — bom", "(notes/inline.md) — id"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
		for f, body := range notes {
			if mustRead(t, paths.MemorySub("notes", f)) != body {
				t.Errorf("%s was edited", f)
			}
		}
	})
}

func TestOlderGeneratedFormsAreNotInput(t *testing.T) {
	runBank(t, func(t *testing.T) {
		writeNote(t, "notes", "a.md", "---\nname: a\ndescription: d\n---\n")
		region := "<!-- bank:begin generated by claude-code memory index 2026-01-01 -->\nBank: decisionLog (.md); " +
			"`claude-code memory search <terms>`\nLayout: one bank for every workspace; archive/ (see index.md)\n" +
			"- decisionLog: y\n- unique, written inside the region\n<!-- bank:end -->\n"
		write(t, NativeIndex(), region+"- [a](notes/a.md) — hook\n")
		_, old := refreshIndex(t)
		writeNote(t, "notes", "b.md", "---\nname: b\ndescription: bd\n---\n")
		refreshIndex(t)
		write(t, NativeIndex(), old) // an earlier generation written back whole: its hash still holds
		want := []string{"- unique, written inside the region", "- [a](notes/a.md) — hook"}
		if refreshIndex(t); !slices.Equal(stored(t), want) {
			t.Errorf("only lines the older generator could not write are input, never an earlier block: %q", stored(t))
		}
	})
}

func TestUnresolvedShowsTheNewestTenAndKeepsAllInIndexMd(t *testing.T) {
	runBank(t, func(t *testing.T) {
		var free []string
		for i := range 12 {
			free = append(free, fmt.Sprintf("- [gone%d](notes/gone%d.md) — missing", i, i))
		}
		write(t, NativeIndex(), strings.Join(free, "\n")+"\n")
		rep, got := refreshIndex(t)
		if rep.Unresolved != 12 || !strings.Contains(got, "gone11.md") || strings.Contains(got, "gone1.md") ||
			!strings.Contains(got, "- 2 more unresolved in index.md\n") || len(stored(t)) != 12 {
			t.Errorf("want the newest 10 shown, a count and all 12 stored: %+v\n%s", rep, got)
		}
	})
}

// snapshotIndexFiles is each index file's link target or bytes, so a refusal can be shown to change nothing.
func snapshotIndexFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range []string{catalogFile, nativeIndexFile, routerFile, "notes/a.md", asidePath()} {
		p := n
		if !filepath.IsAbs(n) {
			p = storePath(n)
		}
		if target, err := os.Readlink(p); err == nil {
			out[n] = "link:" + target
		} else {
			out[n] = readIfExistsString(p)
		}
	}
	return out
}

func TestAliasedIndexFilesAreRefusedWithNothingChanged(t *testing.T) {
	aliases := map[string]func() error{
		"index.md to MEMORY.md": func() error { return os.Symlink(nativeIndexFile, catalogPath()) },
		"index.md to a note":    func() error { return os.Symlink("notes/a.md", catalogPath()) },
		"MEMORY.md symlink":     func() error { _ = os.Remove(NativeIndex()); return os.Symlink("notes/a.md", NativeIndex()) },
		"kept copy symlink": func() error {
			_ = os.MkdirAll(paths.MemoryStateDir(), 0o755)
			return os.Symlink("notes/a.md", asidePath())
		},
		"catalog.md symlink": func() error { return os.Symlink("notes/a.md", storePath(routerFile)) },
		"index.md hard link": func() error { return os.Link(NativeIndex(), catalogPath()) },
		"kept copy hard link": func() error {
			_ = os.MkdirAll(paths.MemoryStateDir(), 0o755)
			return os.Link(NativeIndex(), asidePath())
		},
	}
	for name, alias := range aliases {
		runBank(t, func(t *testing.T) {
			writeNote(t, "notes", "a.md", "---\nname: a\ndescription: d\n---\nsynthetic\n")
			write(t, NativeIndex(), "- symlink-token\n")
			if err := alias(); err != nil {
				t.Skip("this filesystem cannot make the alias:", err)
			}
			before := snapshotIndexFiles(t)
			for range 2 {
				rep, err := refreshNativeIndex(indexNow, false, nil)
				if !errors.Is(err, errIndexProblem) || !strings.Contains(err.Error(), "refused") || rep.Left {
					t.Fatalf("%s: err=%v left=%v, want a refusal naming the file", name, err, rep.Left)
				}
			}
			if after := snapshotIndexFiles(t); !maps.Equal(before, after) {
				t.Errorf("%s: a refused refresh changed a file:\n%q\n%q", name, before, after)
			}
		})
	}
}

func TestMemoryMdStaysWhenHardLinksAreUnsupported(t *testing.T) {
	runBank(t, func(t *testing.T) {
		linkFile = func(a, b string) error {
			return &os.LinkError{Op: "link", Old: a, New: b, Err: errors.New("operation not supported")}
		}
		t.Cleanup(func() { linkFile = os.Link })
		for i := range 3 {
			e := fmt.Sprintf("- link-fail %d", i)
			appendFile(t, NativeIndex(), e+"\n")
			_, err := refreshNativeIndex(indexNow, false, nil)
			mem := readIfExistsString(NativeIndex())
			if err != nil || !strings.HasPrefix(mem, blockBegin) || !strings.Contains(mem, e+"\n") || !slices.Contains(stored(t), e) {
				t.Fatalf("refresh %d: err=%v; MEMORY.md must hold the new text and the entry be stored, without hard links", i, err)
			}
		}
	})
}

func TestEntryBytesAreKeptAndBlankSeparatorsReported(t *testing.T) {
	cases := map[string]struct {
		in     string
		stored []string
		blank  int
	}{
		"bom":    {"\ufeff- a", []string{"\ufeff- a"}, 0},
		"crlf":   {"- a\r\n  b\r\n", []string{"- a\r\n  b\r"}, 0},
		"blanks": {"\n\n\n", nil, 3},
		"edges":  {"\n- a\n\n- b\n", []string{"- a", "- b"}, 2},
	}
	for name, c := range cases {
		runBank(t, func(t *testing.T) {
			write(t, NativeIndex(), c.in)
			rep, err := refreshNativeIndex(indexNow, false, nil)
			if err != nil || !slices.Equal(stored(t), c.stored) || rep.Blank != c.blank ||
				c.blank > 0 && !strings.Contains(rep.String(), "blank line(s)") {
				t.Errorf("%s: stored %q, blank %d (%v), want %q and %d reported", name, stored(t), rep.Blank, err, c.stored, c.blank)
			}
		})
	}
}

func TestEditedBlockKeepsEveryLineItDidNotGenerate(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 25)
		_, cur := refreshIndex(t)
		e := "- new-entry\n" + bankIndexHint + "\n  user-tail" // a generated line inside a new entry is the entry's
		write(t, NativeIndex(), strings.Replace(cur, blockEndPrefix, e+"\n"+blockEndPrefix, 1))
		seedNotes(t, 26) // the "N more" count changes, so the old count line is no longer one the generator writes
		refreshIndex(t)
		if s := stored(t); !slices.Equal(s, []string{e}) {
			t.Errorf("index.md = %q, want only the inserted entry, whole, and never our own older lines", s)
		}
	})
}

func TestChangedGeneratedCounterIsNeitherInputNorUnresolved(t *testing.T) {
	runBank(t, func(t *testing.T) {
		seedNotes(t, 25)
		_, cur := refreshIndex(t)
		i := strings.Index(cur, " more: see ")
		j := strings.LastIndex(cur[:i], "\n- ") + len("\n- ")
		write(t, NativeIndex(), cur[:j]+"999"+cur[i:])
		_, next := refreshIndex(t)
		if s := stored(t); len(s) != 0 || strings.Contains(next, "## "+titleUnresolved) {
			t.Errorf("stored %q, unresolved shown %v: a generator counter is not an entry", s, strings.Contains(next, titleUnresolved))
		}
	})
}

func TestPromotedEntryDeletedFromIndexAndKeptCopyStaysGone(t *testing.T) {
	runBank(t, func(t *testing.T) {
		writeNote(t, "notes", "a.md", "---\nname: a\ndescription: d\n---\n")
		e := "- [a](notes/a.md) custom"
		write(t, NativeIndex(), e+"\n")
		refreshIndex(t)
		_ = os.Remove(paths.MemorySub("notes", "a.md")) // the promotion prompt's steps: the note, its record, its kept-copy line
		write(t, catalogPath(), strings.Replace(mustRead(t, catalogPath()), encodeRecord(e), "", 1))
		write(t, asidePath(), strings.Replace(mustRead(t, asidePath()), e+"\n", "", 1))
		if refreshIndex(t); slices.Contains(stored(t), e) {
			t.Error("an entry deleted from index.md and the kept copy came back")
		}
	})
}

func TestOlderBinaryDotfilesAreAdoptedOnceWithNoLoss(t *testing.T) {
	runBank(t, func(t *testing.T) {
		writeNote(t, "notes", "a.md", "---\nname: a\ndescription: d\n---\n")
		_, cur := refreshIndex(t)
		write(t, storePath(legacyGen), cur) // the layout an older binary leaves: proof and kept copy in the root
		write(t, storePath(legacyAside), "- hand-from-old-copy\n")
		_ = os.Remove(asidePath())
		_ = os.Remove(genPath())
		for range 2 {
			refreshIndex(t)
		}
		if s := stored(t); len(s) != 1 || s[0] != "- hand-from-old-copy" {
			t.Errorf("index.md = %q, want the old kept copy's line once", s)
		}
		for _, p := range []string{storePath(legacyAside), storePath(legacyGen)} {
			if _, err := os.Lstat(p); err == nil {
				t.Errorf("%s still in the store root", p)
			}
		}
		if mustRead(t, genPath()) != mustRead(t, NativeIndex()) {
			t.Error("the state folder's proof is not the installed MEMORY.md")
		}
	})
}

func TestRefreshRefusesWhenTheKeptCopyCannotBeRenamedAcrossVolumes(t *testing.T) {
	runBank(t, func(t *testing.T) {
		write(t, NativeIndex(), "- keep-me\n")
		renameFile = func(string, string) error { return &os.LinkError{Op: "rename", Err: errors.New("cross-device link")} }
		t.Cleanup(func() { renameFile = os.Rename })
		_, err := refreshNativeIndex(indexNow, false, nil)
		if !errors.Is(err, errIndexProblem) || !strings.Contains(err.Error(), "one volume") {
			t.Fatalf("err = %v, want a refusal naming the volume", err)
		}
		if mustRead(t, NativeIndex()) != "- keep-me\n" {
			t.Error("MEMORY.md changed on a refused refresh")
		}
		if left, _ := filepath.Glob(NativeIndex() + ".tmp-*"); len(left) != 0 {
			t.Errorf("staged temp files left: %v", left)
		}
	})
}
