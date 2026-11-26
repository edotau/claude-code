package memory

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

var docFixtures = map[string]string{
	"decisionLog.md":         "# Decisions\n\n> contract\n\n## 2026-11-25 — a <!-- id:0a1b2c3d -->\nbody\n\n## 2026-11-24 — b\n- x\n",
	"conventions.md":         "# Conventions\n\n> c\n\n- one <!-- id:0a1b2c3d -->\n  wrapped\n\n- two\n",
	"progress.md":            "# Progress\n\n<!-- hidden -->\n## Now\n- a\n- b\n## Later\n- c\n",
	"note.md":                "---\nname: n\ndescription: \"d\"\nmetadata:\n  type: feedback\n---\n\n- body\n",
	"crlf.md":                "# T\r\n\r\n- a\r\n- b\r\n",
	"empty.md":               "",
	"fenced.md":              "# T\n\n```\n## not a heading\n- not a bullet\n```\n- real\n",
	"unclosed.md":            "---\nname: x\n\n- a",
	"decisionLog-2026-10.md": "## 2026-10-01 — old\n",
}

func TestDocRoundTripFixtures(t *testing.T) {
	for name, in := range docFixtures {
		if got := string(ParseDoc(name, []byte(in)).Render()); got != in {
			t.Errorf("%s: render differs\n got %q\nwant %q", name, got, in)
		}
	}
}

func TestDocRoundTripStore(t *testing.T) {
	n := 0
	for name, b := range storeFixtures(t) {
		if out := ParseDoc(name, b).Render(); !bytes.Equal(out, b) {
			t.Errorf("%s: first differing byte %d", name, firstDiff(out, b))
		}
		n++
	}
	t.Logf("round-trip: %d fixture files, 0 differ", n)
}

// storeFixtures reads every Markdown file under testdata/store, keyed by its slash path.
func storeFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	root := os.DirFS("testdata/store")
	files := map[string][]byte{}
	err := fs.WalkDir(root, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := fs.ReadFile(root, p)
		files[p] = b
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures: %d files, err %v", len(files), err)
	}
	return files
}

func firstDiff(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

func TestDocWrappedBulletIsOneEntry(t *testing.T) {
	in := "# C\n\n- first <!-- id:0a1b2c3d -->\n  wrapped one\n  wrapped two\n  - nested\n\n- second\n"
	d := ParseDoc("conventions.md", []byte(in))
	if len(d.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(d.Entries))
	}
	if e := d.Entries[0]; e.ID != "0a1b2c3d" || len(e.Lines) != 5 || e.Kind != KindBullet {
		t.Errorf("first entry = %+v", e)
	}
	if d.Entries[1].ID != "" || d.Entries[1].Seq != 1 {
		t.Errorf("second entry = %+v", d.Entries[1])
	}
}

func TestDocGrammarAndStructure(t *testing.T) {
	d := ParseDoc("decisionLog.md", []byte(docFixtures["decisionLog.md"]))
	if d.Grammar != GrammarDated || len(d.Entries) != 2 || d.Entries[0].Date != "2026-11-25" {
		t.Fatalf("dated doc = %+v", d)
	}
	if d.Entries[0].ID != "0a1b2c3d" || d.Header != "# Decisions" {
		t.Errorf("id/header = %q %q", d.Entries[0].ID, d.Header)
	}
	if got := d.OldestFirst(0, 2); got[0] != 1 {
		t.Errorf("dated OldestFirst = %v", got)
	}
}

func TestDocSectionsAndFences(t *testing.T) {
	p := ParseDoc("progress.md", []byte(docFixtures["progress.md"]))
	if lo, hi := p.Section("Now"); lo != 1 || hi != 3 {
		t.Errorf("Section(Now) = %d,%d", lo, hi)
	}
	if lo, _ := p.Section("Nope"); lo != -1 {
		t.Errorf("absent section lo = %d", lo)
	}
	if got := p.OldestFirst(1, 3); got[0] != 2 {
		t.Errorf("sections OldestFirst = %v (higher Seq is older)", got)
	}
	c := ParseDoc("conventions-2026-10.md", []byte(docFixtures["conventions.md"]))
	if c.Grammar != GrammarBullets || c.OldestFirst(0, 2)[0] != 0 {
		t.Errorf("conventions archive = %+v", c)
	}
	f := ParseDoc("fenced.md", []byte(docFixtures["fenced.md"]))
	if len(f.Entries) != 1 {
		t.Errorf("fenced entries = %d, want 1", len(f.Entries))
	}
}

func TestFrontNeedsYAMLShape(t *testing.T) {
	for _, in := range []string{"---\n\nA paragraph.\n---\nbody\n", "---\n- a\n---\n", "---\nname: x\n\n- a"} {
		if d := ParseDoc("n.md", []byte(in)); d.Front != nil || string(d.Render()) != in {
			t.Errorf("%q parsed as front %q", in, d.Front)
		}
	}
	in := "---\n# c\nname: n\nmetadata:\n  type: t\n\n---\n- a\n"
	if d := ParseDoc("n.md", []byte(in)); len(d.Front) != 7 || string(d.Render()) != in {
		t.Errorf("real front lost: %q", d.Front)
	}
}

func TestDocUnclosedFence(t *testing.T) {
	in := "# T\n\n- a\n```\n- b\n## c\n- d\n"
	d := ParseDoc("progress.md", []byte(in))
	if len(d.Entries) != 4 || string(d.Render()) != in {
		t.Errorf("entries = %d, want 4 (a, b, c, d)", len(d.Entries))
	}
	in = "- a\n````\n```\n- b\n````\n- c\n"
	if d := ParseDoc("progress.md", []byte(in)); len(d.Entries) != 2 {
		t.Errorf("closed fence entries = %d, want 2", len(d.Entries))
	}
}

func TestDocIDFromFirstLine(t *testing.T) {
	in := "- real <!-- id:0a1b2c3d -->\n  quoted <!-- id:11111111 -->\n- no id\n  <!-- id:22222222 -->\n"
	d := ParseDoc("conventions.md", []byte(in))
	if d.Entries[0].ID != "0a1b2c3d" || d.Entries[1].ID != "" {
		t.Errorf("ids = %q %q", d.Entries[0].ID, d.Entries[1].ID)
	}
}

func TestMarkerLinear(t *testing.T) {
	line := strings.Repeat("<!-- id:", 130000)
	start := time.Now()
	if _, _, _, ok := marker(line); ok {
		t.Error("marker found in repeats")
	}
	if _, lo, _, ok := marker(line + "0a1b2c3d -->"); !ok || lo != len(line)-len(markerOpen) {
		t.Errorf("last marker lo = %d ok = %v", lo, ok)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("marker on a %d byte line took %v", len(line), time.Since(start))
	}
}

func TestParseRefSlugEdges(t *testing.T) {
	for _, in := range []string{"0a1b2c3d--", "0a1b2c3d---", "0a1b2c3d--a", "0a1b2c3d-a-"} {
		if _, ok := ParseRef(in); ok {
			t.Errorf("ParseRef(%q) accepted", in)
		}
	}
	if r, ok := ParseRef("0a1b2c3d-a-b"); !ok || r.Alias != "a-b" {
		t.Errorf("ParseRef inner hyphen = %+v %v", r, ok)
	}
}

func TestParseRef(t *testing.T) {
	ok := map[string]Ref{
		"0a1b2c3d":      {Hex: "0a1b2c3d"},
		"z0a1b2c3d":     {Hex: "0a1b2c3d", Derived: true},
		"0a1b2c3d-slug": {Hex: "0a1b2c3d", Alias: "slug"},
	}
	for in, want := range ok {
		got, good := ParseRef(in)
		if !good || got != want || got.String() != in {
			t.Errorf("ParseRef(%q) = %+v %v", in, got, good)
		}
	}
	for _, in := range []string{"", "0a1b2c3", "0A1B2C3D", "zzzzzzzz", "0a1b2c3d-", "0a1b2c3d slug", "z-slug", "0a1b2c3dd"} {
		if _, good := ParseRef(in); good {
			t.Errorf("ParseRef(%q) accepted", in)
		}
	}
}

func TestRefMarker(t *testing.T) {
	line := "a `<!-- id:11111111 -->` b <!-- id:0a1b2c3d --> c"
	r, lo, hi, ok := marker(line)
	if !ok || r.Hex != "0a1b2c3d" || line[lo:hi] != "<!-- id:0a1b2c3d -->" {
		t.Errorf("marker = %+v %d %d %v", r, lo, hi, ok)
	}
	if _, _, _, ok := marker("`<!-- id:11111111 -->`"); ok {
		t.Error("marker inside a code span was taken")
	}
}

func TestIDMarkersIgnoreCodeSpans(t *testing.T) {
	for _, span := range []string{"`<!-- id:11111111 -->`", "``a ` <!-- id:11111111 -->``", "``<!-- id:11111111 -->``"} {
		line := "- example " + span
		if id := entryID([]string{line}); id != "" {
			t.Errorf("example became id %s: %q", id, line)
		}
		out, n := stampIDs("conventions.md", "- ", line+"\n", testEnv().Now)
		if n != 1 || !strings.Contains(out, span) || entryID(strings.Split(out, "\n")) == "11111111" {
			t.Errorf("example prevented stamping or changed: %d %q", n, out)
		}
		if stripID(line) != line || ownerID(line) != "" {
			t.Errorf("display or ownership consumed example: %q", line)
		}
	}
}

func TestMarkerOutsideEscapedBackticks(t *testing.T) {
	for _, line := range []string{
		"- literal \\` <!-- id:12345678 --> \\`",
		"- example `<!-- id:11111111 --> \\` after <!-- id:12345678 -->",
	} {
		if id := entryID([]string{line}); id != "12345678" {
			t.Errorf("escaped ticks changed the owner to %q: %q", id, line)
		}
	}
}

func TestStampIDsKeepsHeadingBlocks(t *testing.T) {
	keep := "## Existing section\n\n" + strings.Repeat("old   words ", 20) + "\n\n\n"
	in := "# C\n\n- needs id\n\n" + keep + "- has id <!-- id:12345678 -->\n"
	out, n := stampIDs("conventions.md", "- ", in, testEnv().Now)
	if n != 1 || !strings.Contains(out, keep) {
		t.Fatalf("stamping changed the next heading block: %d\n%s", n, out)
	}
}

func TestStampCRLFAndFencedMarkers(t *testing.T) {
	for _, hard := range []string{"", "  "} {
		in := "- example" + hard + "\r\n  ```md\r\n  <!-- id:11111111 -->\r\n  ```\r\n"
		out, n := stampIDs("conventions.md", "- ", in, testEnv().Now)
		lines := strings.Split(out, "\n")
		id := entryID(lines)
		if n != 1 || id == "" || id == "11111111" || !strings.HasSuffix(lines[0], hard+"\r") {
			t.Errorf("bad CRLF stamp or fenced example became id: %d %q", n, out)
		}
		if strings.Contains(strings.TrimSuffix(lines[0], "\r"), "\r") || !strings.Contains(out, in[strings.Index(in, "  ```"):]) {
			t.Errorf("embedded carriage return or changed fence: %q", out)
		}
	}
}

func TestCleanNewBlockNestedList(t *testing.T) {
	in := "para\n  - one\n    - two\nend"
	want := "para\n\n  - one\n    - two\n\nend"
	if got := cleanNewBlock(in); got != want {
		t.Fatalf("nested list gaps: got %q want %q", got, want)
	}
}

func TestArchiveAppendPreservesCleanSeparators(t *testing.T) {
	before := "# A\n\n## Old <!-- id:12345678 -->\n\nbody\n\n"
	block := "## New <!-- id:23456789 -->\n\nnew body\n\n"
	if got := appendedText("archive.md", before, []string{block}); got != before+block {
		t.Fatalf("append introduced blank lines: %q", got)
	}
}

type splitCase struct {
	name       string
	in         string
	wantHeader string
	want       []string
}

// splitEdgeCases is the splitHeadingBlocks edge-case table.
func splitEdgeCases() []splitCase {
	return []splitCase{
		{
			name:       "fence aware",
			in:         "header\n\n## real\nbody\n```\n## fake\n```\nmore\n\n## real2\nbody2\n",
			wantHeader: "header",
			want:       []string{"## real\nbody\n```\n## fake\n```\nmore", "## real2\nbody2"},
		},
		{
			name:       "mixed fence stays open",
			in:         "h\n\n## a\n```\n~~~\n## nope\n~~~\nstill fence\n",
			wantHeader: "h",
			want:       []string{"## a\n```\n~~~\n## nope\n~~~\nstill fence"},
		},
		{
			name:       "unclosed fence",
			in:         "h\n\n## a\n```\n## nope\n",
			wantHeader: "h",
			want:       []string{"## a\n```\n## nope"},
		},
		{
			name:       "CRLF keeps carriage returns",
			in:         "h\r\n\r\n## a\r\nbody\r\n",
			wantHeader: "h\r\n\r",
			want:       []string{"## a\r\nbody\r"},
		},
		{
			name:       "no trailing newline",
			in:         "h\n\n## a\nbody",
			wantHeader: "h",
			want:       []string{"## a\nbody"},
		},
		{
			name:       "duplicate headings",
			in:         "h\n\n## dup\nfirst\n\n## dup\nsecond\n",
			wantHeader: "h",
			want:       []string{"## dup\nfirst", "## dup\nsecond"},
		},
	}
}

// Heading splits respect matching fences, preserve line endings and keep duplicate headings.
func TestSplitHeadingBlocksEdgeCases(t *testing.T) {
	t.Parallel()
	for _, tt := range splitEdgeCases() {
		t.Run(tt.name, func(t *testing.T) {
			header, blocks := splitHeadingBlocks(tt.in)
			if header != tt.wantHeader {
				t.Errorf("header = %q, want %q", header, tt.wantHeader)
			}
			if !reflect.DeepEqual(blocks, tt.want) {
				t.Errorf("blocks = %#v, want %#v", blocks, tt.want)
			}
		})
	}
}

// Rejoin(splitOn(x)) == x for a well-formed, trailing-newline-terminated file (fences included).
func TestRejoinSplitHeadingBlocksRoundTrip(t *testing.T) {
	t.Parallel()
	in := "header\n\n## real\nbody\n```\n## fake\n```\nmore\n\n## real2\nbody2\n"
	header, blocks := splitHeadingBlocks(in)
	if got := rejoin(header, blocks); got != in {
		t.Errorf("rejoin(splitHeadingBlocks(x)) = %q, want %q", got, in)
	}
}

func TestAppendBlocksPreservesBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing string
		create   bool
		blocks   []string
		want     string
	}{
		{name: "missing", blocks: []string{"- first"}, want: "# topic (archived)\n\n- first\n"},
		{name: "empty", create: true, want: "# topic (archived)\n"},
		{name: "whitespace", create: true, existing: " \t\n", blocks: []string{""}, want: "# topic (archived)\n\n\n"},
		{name: "header without newline", create: true, existing: "# Existing", blocks: []string{"- first", "- second"}, want: "# Existing\n\n- first\n\n- second\n"},
		{name: "header with newlines", create: true, existing: "# Existing\n\n", blocks: []string{"- first\n", "", "## Heading\nbody\n\n"}, want: "# Existing\n\n- first\n\n\n\n## Heading\nbody\n\n"},
		{name: "no blocks", create: true, existing: "# Existing\n\n", want: "# Existing\n\n"},
		{name: "no blocks adds newline", create: true, existing: "# Existing", want: "# Existing\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "topic.md")
			if tt.create {
				write(t, path, tt.existing)
			}
			if err := appendBlocks(path, tt.blocks); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("appendBlocks bytes = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAppendBlocksUnreadableArchiveIsNotOverwritten: a read error other than not-exist aborts before any write.
func TestAppendBlocksUnreadableArchiveIsNotOverwritten(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "sessionHistory-2026-11.md") // a directory: ReadFile fails with EISDIR
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := appendBlocks(dir, []string{"## new\nbody"}); err == nil {
		t.Fatal("AppendBlocks on an unreadable archive = nil error, want the read error")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("unreadable archive was replaced: %v", err)
	}
}

// countBankReads swaps the readFile seam for one that counts reads of each typed bank file by path.
func countBankReads(t *testing.T) map[string]int {
	t.Helper()
	reads := map[string]int{}
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		name := filepath.Base(p)
		if _, rest, ok := strings.Cut(name, "--"); ok {
			name = rest
		}
		if slices.Contains(bankFiles, name) {
			reads[p]++
		}
		return orig(p)
	}
	t.Cleanup(func() { readFile = orig })
	return reads
}

// TestBlockIDsPersistAcrossEditsAndArchive: IDs are stamped once, survive a title edit, and `show` finds archives.
func TestBlockIDsPersistAcrossEditsAndArchive(t *testing.T) {
	runBank(t, func(t *testing.T) {
		initStore(t)
		log := Path("decisionLog.md")
		write(t, log, "# Decision Log\n\n## 2026-10-18 — First\nbody one\n\n## 2026-10-17 — Second\nbody two\n")
		now := time.Now()
		if n, err := assignIDsT(now); err != nil || n != 2 {
			t.Fatalf("first stamp = %d, %v; want 2", n, err)
		}
		if n, _ := assignIDsT(now.Add(time.Second)); n != 0 {
			t.Errorf("second stamp re-stamped %d blocks; IDs must be assigned once", n)
		}
		data, _ := os.ReadFile(log)
		id := firstBlockID(t, string(data))
		write(t, log, strings.Replace(string(data), "— First", "— First, retitled", 1))
		if _, block, err := ShowBlock(id); err != nil || !strings.Contains(block, "retitled") || !strings.Contains(block, "body one") {
			t.Errorf("show after a title edit = %q, %v", block, err)
		}
		moved, _ := os.ReadFile(log)
		archive := paths.MemorySub("archive", "decisionLog-2026-10.md")
		write(t, archive, string(moved))
		write(t, log, "# Decision Log\n")
		assertShownFromArchive(t, id, archive)
	})
}

// firstBlockID returns the id stamped on the 2026-10-18 block of data.
func firstBlockID(t *testing.T, data string) string {
	t.Helper()
	var id string
	for l := range strings.SplitSeq(data, "\n") {
		if strings.HasPrefix(l, "## 2026-10-18") {
			id = blockID(l)
		}
	}
	if len(id) != 8 {
		t.Fatalf("no id on the first block:\n%s", data)
	}
	return id
}

// assertShownFromArchive checks ShowBlock finds id in the archive, an unknown id gives errNoBlock, no raw marker shows.
func assertShownFromArchive(t *testing.T, id, archive string) {
	t.Helper()
	if path, _, err := ShowBlock(id); err != nil || !strings.Contains(path, "archive") {
		t.Errorf("show after archiving = %s, %v; want the archive file", path, err)
	}
	if _, _, err := ShowBlock("deadbeef"); err != errNoBlock {
		t.Errorf("unknown id err = %v, want errNoBlock", err)
	}
	if e, _ := entriesWithPrefix(archive, "## "); len(e) == 0 || !strings.HasSuffix(e[0], "["+id+"]") || strings.Contains(e[0], "<!--") {
		t.Errorf("index line must show [id] without the raw marker: %v", e)
	}
}

// TestAssignIDsSecondPassLeavesExistingIDsByteIdentical: a stamped block must never be re-stamped, even byte-wise.
func TestAssignIDsSecondPassLeavesExistingIDsByteIdentical(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("decisionLog.md")
		write(t, path, "## First\nbody one\n\n## Second\nbody two\n")
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if n, err := assignIDsT(now); err != nil || n != 2 {
			t.Fatalf("first stamp = %d, %v; want 2", n, err)
		}
		before, _ := os.ReadFile(path)
		if n, err := assignIDsT(now.Add(time.Hour)); err != nil || n != 0 {
			t.Fatalf("second stamp = %d, %v; want 0 (already stamped)", n, err)
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatalf("second pass changed stamped content:\nbefore: %q\nafter:  %q", before, after)
		}
	})
}

// TestAssignIDsThreeIdenticalBlocksGetThreeDistinctIDs: three byte-identical block headers must get three distinct IDs.
func TestAssignIDsThreeIdenticalBlocksGetThreeDistinctIDs(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("decisionLog.md")
		write(t, path, "## Fix the bug\nbody one\n\n## Fix the bug\nbody two\n\n## Fix the bug\nbody three\n")
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if n, err := assignIDsT(now); err != nil || n != 3 {
			t.Fatalf("assignIDs = %d, %v; want 3", n, err)
		}
		data, _ := os.ReadFile(path)
		seen := map[string]bool{}
		for l := range strings.SplitSeq(string(data), "\n") {
			if id := blockID(l); id != "" {
				if seen[id] {
					t.Fatalf("duplicate id %q across identical blocks", id)
				}
				seen[id] = true
			}
		}
		if len(seen) != 3 {
			t.Fatalf("got %d distinct ids, want 3: %v", len(seen), seen)
		}
	})
}

// TestAssignIDsSameTextDifferentFilesGetDistinctIDs: the same block text stamped in two typed files must not collide.
func TestAssignIDsSameTextDifferentFilesGetDistinctIDs(t *testing.T) {
	runBank(t, func(t *testing.T) {
		write(t, Path("decisionLog.md"), "## Fix the bug\nbody\n")
		write(t, Path("sessionHistory.md"), "## Fix the bug\nbody\n")
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if n, err := assignIDsT(now); err != nil || n != 2 {
			t.Fatalf("assignIDs = %d, %v; want 2", n, err)
		}
		var ids []string
		for _, name := range []string{"decisionLog.md", "sessionHistory.md"} {
			data, _ := os.ReadFile(Path(name))
			for l := range strings.SplitSeq(string(data), "\n") {
				if id := blockID(l); id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) != 2 || ids[0] == ids[1] {
			t.Fatalf("same text in different files got duplicate ids: %v", ids)
		}
	})
}

// A marker the formatter wrapped onto a later line counts: settling twice adds nothing.
func TestAssignIDsSeesMarkerOnLaterLine(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("conventions.md")
		body := "# C\n\n- long convention wrapped\n  onto two lines <!-- id:aaaa0001 -->\n\n- plain rule\n"
		write(t, path, body)
		n, err := assignIDsT(time.Now())
		if err != nil || n != 1 { // only "plain rule" is unmarked
			t.Fatalf("first settle stamped %d, err %v", n, err)
		}
		once, _ := os.ReadFile(path)
		if !strings.Contains(string(once), "- long convention wrapped\n  onto two lines <!-- id:aaaa0001 -->\n") {
			t.Errorf("a wrapped marker must stay where the formatter put it:\n%s", once)
		}
		if n, _ = assignIDsT(time.Now().Add(time.Second)); n != 0 {
			t.Errorf("second settle stamped %d", n)
		}
		twice, _ := os.ReadFile(path)
		if string(once) != string(twice) || strings.Count(string(twice), "<!-- id:") != 2 {
			t.Errorf("second settle changed the file:\n%s\n---\n%s", once, twice)
		}
	})
}

// Stacked markers collapse to the oldest (last) one, idempotently.
func TestAssignIDsCollapsesStackedMarkersToOldest(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("conventions.md")
		write(t, path, "# C\n\n- a rule that wraps\n  <!-- id:00000005 -->\n  <!-- id:00000004 -->\n  <!-- id:00000003 -->\n  <!-- id:00000002 -->\n  <!-- id:00000001 -->\n\n- next\n")
		if _, err := assignIDsT(time.Now()); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		s := string(got)
		if strings.Count(s, "<!-- id:00000001 -->") != 1 || strings.Count(s, "<!-- id:") != 2 { // the other is "next"'s fresh id
			t.Fatalf("want only the oldest marker kept on the wrapped block:\n%s", s)
		}
		if _, err := assignIDsT(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		again, _ := os.ReadFile(path)
		if string(again) != s {
			t.Errorf("collapse is not idempotent:\n%s\n---\n%s", s, again)
		}
		if first := strings.Split(s, "\n")[2]; !strings.HasSuffix(first, "wraps <!-- id:00000001 -->") {
			t.Errorf("the retained marker must end on the block's first line, got %q", first)
		}
	})
}

// A fenced example marker is not the block's id: settle keeps the real one and never adopts the example.
func TestAssignIDsIgnoresFencedMarkers(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("decisionLog.md")
		write(t, path, "# D\n\n## 2026-10-01 — real heading <!-- id:aaaa0001 -->\n\nexample:\n\n```md\n## x <!-- id:bbbb0002 -->\n```\n")
		if _, err := assignIDsT(time.Now()); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		s := string(got)
		if !strings.Contains(s, "real heading <!-- id:aaaa0001 -->") || strings.Count(s, "<!-- id:bbbb0002 -->") != 1 {
			t.Errorf("real id lost or example touched:\n%s", s)
		}
		if id := ownerID(strings.Join(strings.Split(s, "\n")[2:], "\n")); id != "aaaa0001" {
			t.Errorf("canonical id = %q, want the unfenced one", id)
		}
		// an unmarked block whose only marker is a fenced example still gets its own id
		write(t, path, "# D\n\n## 2026-10-02 — bare\n\n```md\n## x <!-- id:cccc0003 -->\n```\n")
		if n, _ := assignIDsT(time.Now()); n != 1 {
			t.Errorf("bare block with a fenced example must be stamped, stamped %d", n)
		}
	})
}

// Content refs of native topics and knowledge notes (indexed whole) expand through ShowBlock.
func TestShowBlockExpandsNativeAndKnowledgeRefs(t *testing.T) {
	isolatedConfig1(t)
	note := paths.MemorySub("knowledge", "wires.md")
	write(t, note, "---\ndescription: d\n---\n# Wires\nknowledge body zebra\n")
	hits, err := corpusBlocks()
	if err != nil {
		t.Fatal(err)
	}
	var got SearchHit
	for _, h := range hits {
		if h.Bank == "knowledge" {
			got = h
		}
	}
	if got.Block == "" {
		t.Fatal("fixture: knowledge note not indexed")
	}
	if _, block, err := ShowBlock(entryFromHit(got).ID); err != nil || !strings.Contains(block, "knowledge body zebra") {
		t.Errorf("knowledge ref does not expand: %v %q", err, block)
	}
}

func TestLogicalNameMatchesTypedStemExactlyOrArchive(t *testing.T) {
	cases := map[string]string{
		"/x/decisionLog.md":           "decisionLog.md",
		"/x/decisionLog-2026-10.md":   "decisionLog.md",
		"/x/sessionHistory-notes.md":  "sessionHistory-notes.md",
		"/x/conventionsGuide.md":      "conventionsGuide.md",
		"/x/decisionLog-2026-10-x.md": "decisionLog-2026-10-x.md",
	}
	for path, want := range cases {
		if got := logicalName(path); got != want {
			t.Errorf("logicalName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestShowBlockKeepsMatchBeforeUnreadableDirectory(t *testing.T) {
	isolatedConfig1(t)
	path := filepath.Join(paths.MemoryStoreDir(), "a-match", "decisionLog.md")
	block := "## Found <!-- id:abcd1234 -->\nmatched body"
	write(t, path, block+"\n")
	unreadable := filepath.Join(paths.MemoryStoreDir(), "z-unreadable")
	write(t, filepath.Join(unreadable, "decisionLog.md"), "## Hidden\nother body\n")
	denyBankAccess(t, unreadable)
	if _, err := os.ReadDir(unreadable); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("fixture must deny directory reads: %v", err)
	}

	gotPath, gotBlock, err := ShowBlock("abcd1234")
	if err != nil {
		t.Fatalf("later unreadable directory discarded a successful match: %v", err)
	}
	if gotPath != path {
		t.Errorf("ShowBlock path = %q, want %q", gotPath, path)
	}
	if gotBlock != block {
		t.Errorf("ShowBlock block = %q, want %q", gotBlock, block)
	}
}

func TestShowBlockSkipsLaterBlockQuotingMarker(t *testing.T) {
	isolatedConfig1(t)
	owner := "## Owner <!-- id:abcd1234 -->\nowner body"
	quote := "## Later <!-- id:bbbbbbbb -->\n```\n<!-- id:abcd1234 -->\n```"
	write(t, filepath.Join(paths.MemoryStoreDir(), "decisionLog.md"), owner+"\n\n"+quote+"\n")
	if _, got, err := ShowBlock("abcd1234"); err != nil || !strings.Contains(got, "owner body") {
		t.Fatalf("ShowBlock = %q, %v; want the owning block", got, err)
	}
}

func TestShowBlockReturnsWalkErrors(t *testing.T) {
	for _, sub := range []string{"", "unreadable"} {
		t.Run(sub, func(t *testing.T) {
			isolatedConfig1(t)
			dir := filepath.Join(paths.MemoryStoreDir(), sub)
			write(t, filepath.Join(dir, "decisionLog.md"), "## Hidden <!-- id:deadbeef -->\nbody\n")
			denyBankAccess(t, dir)
			_, _, err := ShowBlock("deadbeef")
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("ShowBlock = %v, want walk permission error, not errNoBlock", err)
			}
		})
	}
}

// A long conventions bullet keeps its wrapped marker: a second settle rewrites no byte and the catalog sees its id.
func TestSettleLongConventionIsIdempotentAndKeepsItsID(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("conventions.md")
		write(t, path, "# C\n\n- "+strings.TrimSpace(strings.Repeat("word ", 40))+"\n")
		settle := func(now time.Time) {
			t.Helper()
			if _, err := assignIDsT(now); err != nil {
				t.Fatal(err)
			}
			if _, err := formatFiles([]string{path}, false); err != nil {
				t.Fatal(err)
			}
		}
		settle(time.Now())
		once, _ := os.ReadFile(path)
		past := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
		settle(time.Now().Add(time.Second))
		twice, _ := os.ReadFile(path)
		fi, _ := os.Stat(path)
		if string(once) != string(twice) || !fi.ModTime().Equal(past) {
			t.Fatalf("second settle rewrote the file (mtime %v):\n%s\n---\n%s", fi.ModTime(), once, twice)
		}
		got, err := entriesWithPrefix(path, "- ")
		if err != nil || len(got) != 1 || !strings.HasSuffix(got[0], "["+ownerID(string(twice))+"]") {
			t.Fatalf("catalog entry lost the wrapped bullet's id: %q, %v\n%s", got, err, twice)
		}
	})
}

// TestShowBlockRefForms pins which refs ShowBlock resolves: a persisted id or an exact contentRef, nothing looser.
func TestShowBlockRefForms(t *testing.T) {
	runBank(t, func(t *testing.T) {
		initStore(t)
		log := Path("decisionLog.md")
		write(t, log, "# Decision Log\n\n## 2026-10-18 — Marked <!-- id:abcdef12 -->\nbody one\n\n## 2026-10-17 — Bare\nbody two\n")
		_, blocks := entryBlocks("decisionLog.md", mustRead(t, log))
		derived := contentRef(blocks[1])
		for ref, want := range map[string]bool{
			"abcdef12": true, derived: true, "abcdef12-foo": false, "abcdef1": false, "ABCDEF12": false,
			derived + "-foo": false, derived + "0": false, derived[:5]: false, "Z" + derived[1:]: false,
		} {
			if _, _, err := ShowBlock(ref); (err == nil) != want {
				t.Errorf("ShowBlock(%q) err = %v, want found %v", ref, err, want)
			}
		}
	})
}

// Every list marker kind gets a blank line before and after its list in new text.
func TestCleanNewBlockListMarkers(t *testing.T) {
	for _, m := range []string{"- ", "* ", "+ ", "1. ", "2) "} {
		in := "before\n" + m + "first\n" + m + "second\nafter"
		if got, want := cleanNewBlock(in), "before\n\n"+m+"first\n"+m+"second\n\nafter"; got != want {
			t.Errorf("%q: got %q, want %q", m, got, want)
		}
	}
}

func assignIDsT(now time.Time) (int, error) {
	n, _, err := assignIDTexts(now)
	return n, err
}
