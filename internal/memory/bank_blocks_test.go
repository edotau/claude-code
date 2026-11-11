package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

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

// A ~~~ line inside a ``` fence does not close it: the marker after it is still a fenced example.

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
		{name: "header with newlines", create: true, existing: "# Existing\n\n", blocks: []string{"- first\n", "", "## Heading\nbody\n\n"}, want: "# Existing\n\n\n- first\n\n\n\n\n## Heading\nbody\n\n\n"},
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
		if slices.Contains(bankFiles, filepath.Base(p)) {
			reads[p]++
		}
		return orig(p)
	}
	t.Cleanup(func() { readFile = orig })
	return reads
}

// TestBlockIDsPersistAcrossEditsAndArchive: IDs are stamped once, survive a title edit, and `show` finds a block in an archive.
func TestBlockIDsPersistAcrossEditsAndArchive(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	dir := filepath.Join(StoreDir(), "repo")
	log := filepath.Join(dir, "decisionLog.md")
	write(t, log, "# Decision Log\n\n## 2026-10-18 — First\nbody one\n\n## 2026-10-17 — Second\nbody two\n")
	now := time.Now()
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("first stamp = %d, %v; want 2", n, err)
	}
	if n, _ := assignIDs(dir, now.Add(time.Second)); n != 0 {
		t.Errorf("second stamp re-stamped %d blocks; IDs must be assigned once", n)
	}
	data, _ := os.ReadFile(log)
	id := firstBlockID(t, string(data))
	write(t, log, strings.Replace(string(data), "— First", "— First, retitled", 1))
	if _, block, err := ShowBlock(id); err != nil || !strings.Contains(block, "retitled") || !strings.Contains(block, "body one") {
		t.Errorf("show after a title edit = %q, %v", block, err)
	}
	moved, _ := os.ReadFile(log)
	archive := filepath.Join(dir, "archive", "decisionLog-2026-10.md")
	write(t, archive, string(moved))
	write(t, log, "# Decision Log\n")
	assertShownFromArchive(t, id, archive)
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

// assertShownFromArchive checks ShowBlock finds id in the archive, an unknown id reports errNoBlock, and the index line hides the raw marker.
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
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "decisionLog.md")
	write(t, path, "## First\nbody one\n\n## Second\nbody two\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("first stamp = %d, %v; want 2", n, err)
	}
	before, _ := os.ReadFile(path)
	if n, err := assignIDs(dir, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("second stamp = %d, %v; want 0 (already stamped)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("second pass changed stamped content:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestAssignIDsThreeIdenticalBlocksGetThreeDistinctIDs: three byte-identical block headers must get three distinct IDs.
func TestAssignIDsThreeIdenticalBlocksGetThreeDistinctIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "decisionLog.md")
	write(t, path, "## Fix the bug\nbody one\n\n## Fix the bug\nbody two\n\n## Fix the bug\nbody three\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 3 {
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
}

// TestAssignIDsSameTextDifferentFilesGetDistinctIDs: the same block text stamped in two different typed files must not collide.
func TestAssignIDsSameTextDifferentFilesGetDistinctIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "decisionLog.md"), "## Fix the bug\nbody\n")
	write(t, filepath.Join(dir, "sessionHistory.md"), "## Fix the bug\nbody\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("assignIDs = %d, %v; want 2", n, err)
	}
	var ids []string
	for _, name := range []string{"decisionLog.md", "sessionHistory.md"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		for l := range strings.SplitSeq(string(data), "\n") {
			if id := blockID(l); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("same text in different files got duplicate ids: %v", ids)
	}
}

// A marker the formatter wrapped onto a later line counts: settling twice adds nothing.
func TestAssignIDsSeesMarkerOnLaterLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conventions.md")
	body := "# C\n\n- long convention wrapped\n  onto two lines <!-- id:aaaa0001 -->\n\n- plain rule\n"
	write(t, path, body)
	n, err := assignIDs(dir, time.Now())
	if err != nil || n != 1 { // only "plain rule" is unmarked
		t.Fatalf("first settle stamped %d, err %v", n, err)
	}
	once, _ := os.ReadFile(path)
	if !strings.Contains(string(once), "- long convention wrapped\n  onto two lines <!-- id:aaaa0001 -->\n") {
		t.Errorf("a wrapped marker must stay where the formatter put it:\n%s", once)
	}
	if n, _ = assignIDs(dir, time.Now().Add(time.Second)); n != 0 {
		t.Errorf("second settle stamped %d", n)
	}
	twice, _ := os.ReadFile(path)
	if string(once) != string(twice) || strings.Count(string(twice), "<!-- id:") != 2 {
		t.Errorf("second settle changed the file:\n%s\n---\n%s", once, twice)
	}
}

// Stacked markers collapse to the oldest (last) one, idempotently.
func TestAssignIDsCollapsesStackedMarkersToOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conventions.md")
	write(t, path, "# C\n\n- a rule that wraps\n  <!-- id:00000005 -->\n  <!-- id:00000004 -->\n  <!-- id:00000003 -->\n  <!-- id:00000002 -->\n  <!-- id:00000001 -->\n\n- next\n")
	if _, err := assignIDs(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if strings.Count(s, "<!-- id:00000001 -->") != 1 || strings.Count(s, "<!-- id:") != 2 { // the other is "next"'s fresh id
		t.Fatalf("want only the oldest marker kept on the wrapped block:\n%s", s)
	}
	if _, err := assignIDs(dir, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != s {
		t.Errorf("collapse is not idempotent:\n%s\n---\n%s", s, again)
	}
	if first := strings.Split(s, "\n")[2]; !strings.HasSuffix(first, "wraps <!-- id:00000001 -->") {
		t.Errorf("the retained marker must end on the block's first line, got %q", first)
	}
}

// A fenced example marker is not the block's id: settle keeps the real one and never adopts the example.
func TestAssignIDsIgnoresFencedMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisionLog.md")
	write(t, path, "# D\n\n## 2026-10-01 — real heading <!-- id:aaaa0001 -->\n\nexample:\n\n```md\n## x <!-- id:bbbb0002 -->\n```\n")
	if _, err := assignIDs(dir, time.Now()); err != nil {
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
	if n, _ := assignIDs(dir, time.Now()); n != 1 {
		t.Errorf("bare block with a fenced example must be stamped, stamped %d", n)
	}
}

// Content refs of native topics and knowledge notes (indexed whole) expand through ShowBlock.
func TestShowBlockExpandsNativeAndKnowledgeRefs(t *testing.T) {
	isolatedConfig1(t)
	note := filepath.Join(HomeBankDir(), "knowledge", "wires.md")
	write(t, note, "---\ndescription: d\n---\n# Wires\nknowledge body zebra\n")
	hits, err := corpusBlocks(t.TempDir())
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
	path := filepath.Join(StoreDir(), "a-match", "decisionLog.md")
	block := "## Found <!-- id:abcd1234 -->\nmatched body"
	write(t, path, block+"\n")
	unreadable := filepath.Join(StoreDir(), "z-unreadable")
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

func TestShowBlockReturnsWalkErrors(t *testing.T) {
	for _, sub := range []string{"", "unreadable"} {
		t.Run(sub, func(t *testing.T) {
			isolatedConfig1(t)
			dir := filepath.Join(StoreDir(), sub)
			write(t, filepath.Join(dir, "decisionLog.md"), "## Hidden <!-- id:deadbeef -->\nbody\n")
			denyBankAccess(t, dir)
			_, _, err := ShowBlock("deadbeef")
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("ShowBlock = %v, want walk permission error, not errNoBlock", err)
			}
		})
	}
}

// TestRememberSameFactSameClockGetsDistinctIDs: a repeat prepended at the same index and clock never reuses an id.
func TestRememberSameFactSameClockGetsDistinctIDs(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	now := time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC)
	first, err := Remember(root, "project", "same fact", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Remember(root, "project", "same fact", now)
	if err != nil || second == first {
		t.Fatalf("second Remember = %q, %v; want an id distinct from the first, %q", second, err, first)
	}
	if body := mustRead(t, filepath.Join(Dir(root), "decisionLog.md")); strings.Count(body, "<!-- id:"+second+" -->") != 1 {
		t.Errorf("id %s is not unique in decisionLog.md:\n%s", second, body)
	}
}

// A long conventions bullet keeps its wrapped marker: a second settle rewrites no byte, and the catalog still sees its id.
func TestSettleLongConventionIsIdempotentAndKeepsItsID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conventions.md")
	write(t, path, "# C\n\n- "+strings.TrimSpace(strings.Repeat("word ", 40))+"\n")
	settle := func(now time.Time) {
		t.Helper()
		if _, err := assignIDs(dir, now); err != nil {
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
}
