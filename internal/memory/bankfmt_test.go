package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// xword is a fixed-width filler token so first-line/continuation capacity is hand-computable.
const xword = "xxxxxxxxxx" // 10 runes

// repeatWords returns n copies of xword, space-joined.
func repeatWords(n int) []string {
	w := make([]string, n)
	for i := range w {
		w[i] = xword
	}
	return w
}

// TestFormatBankMarkdownWrapTable covers the wrap-point table: bullet, nested bullet, numbered item,
// plain paragraph (indented and not) — each wraps at a word boundary with continuation indented to the
// item's content column — plus every "never touch" surface (fence, table, heading, front matter,
// comment-only line, RULES region) and every "never split" span (code, link, HTML-comment marker).
func TestFormatBankMarkdownWrapTable(t *testing.T) {
	t.Parallel()
	words := repeatWords(15)
	all := strings.Join(words, " ")

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "bullet",
			input: "- " + all,
			want:  "- " + strings.Join(words[:10], " ") + "\n" + "  " + strings.Join(words[10:], " "),
		},
		{
			name:  "nested bullet",
			input: "  - " + all,
			want:  "  - " + strings.Join(words[:10], " ") + "\n" + "    " + strings.Join(words[10:], " "),
		},
		{
			name:  "numbered item",
			input: "1. " + all,
			want:  "1. " + strings.Join(words[:10], " ") + "\n" + "   " + strings.Join(words[10:], " "),
		},
		{
			name:  "plain paragraph, no indent",
			input: all,
			want:  strings.Join(words[:11], " ") + "\n" + strings.Join(words[11:], " "),
		},
		{
			name:  "plain paragraph, indented (idempotence-bug case)",
			input: "   " + all,
			want:  "   " + strings.Join(words[:10], " ") + "\n" + "   " + strings.Join(words[10:], " "),
		},
		{
			name:  "fenced code untouched even over-width",
			input: "```\n" + strings.Repeat("z", 200) + "\n```",
			want:  "```\n" + strings.Repeat("z", 200) + "\n```",
		},
		{
			name:  "table row untouched even over-width",
			input: "| " + strings.Repeat("a ", 70) + "|",
			want:  "| " + strings.Repeat("a ", 70) + "|",
		},
		{
			name:  "heading untouched even over-width",
			input: "## " + strings.Repeat("heading word ", 12),
			want:  "## " + strings.Repeat("heading word ", 12),
		},
		{
			name:  "front matter untouched even over-width",
			input: "---\nkey: " + strings.Repeat("v", 200) + "\n---\nbody\n",
			want:  "---\nkey: " + strings.Repeat("v", 200) + "\n---\nbody\n",
		},
		{
			name:  "comment-only line untouched even over-width",
			input: "<!-- " + strings.Repeat("note ", 30) + "-->",
			want:  "<!-- " + strings.Repeat("note ", 30) + "-->",
		},
		{
			name: "RULES region untouched even over-width",
			input: "before\n<!-- RULES:BEGIN -->\n" + strings.Repeat("rule word ", 15) +
				"\n<!-- RULES:END -->\nafter",
			want: "before\n<!-- RULES:BEGIN -->\n" + strings.Repeat("rule word ", 15) +
				"\n<!-- RULES:END -->\nafter",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatBankMarkdown(c.input)
			if got != c.want {
				t.Errorf("FormatBankMarkdown(%q)\n got: %q\nwant: %q", c.input, got, c.want)
			}
		})
	}
}

// TestFormatBankMarkdownNeverSplitsProtectedSpans: an inline code span, a markdown link, and an HTML
// comment marker stay byte-identical, contiguous substrings of the output even when the line around
// them is wrapped, and even when the span itself is wider than the wrap cap (kept whole, over-width).
func TestFormatBankMarkdownNeverSplitsProtectedSpans(t *testing.T) {
	t.Parallel()
	longCode := "`" + strings.Join(repeatWords(13), " ") + "`" // spaces inside: 13*10+12 = 142 runes, >120
	line := "see " + longCode + " for details"
	out := FormatBankMarkdown(line)
	if !strings.Contains(out, longCode) {
		t.Fatalf("code span split or altered:\n%s", out)
	}
	for l := range strings.SplitSeq(out, "\n") {
		if l == longCode && utf8RuneCount(l) <= BankFmtMaxLine {
			t.Errorf("expected the lone code-span line to stay over-width (unsplit), got %d runes", utf8RuneCount(l))
		}
	}

	link := "[" + strings.Join(repeatWords(6), " ") + "](http://example.com/a/b/c/with/a/long/path/here)"
	line = "read " + link + " " + strings.Join(repeatWords(10), " ")
	out = FormatBankMarkdown(line)
	if !strings.Contains(out, link) {
		t.Fatalf("link split or altered:\n%s", out)
	}

	marker := "<!-- id:abcdef0123456789 -->"
	line = "- " + strings.Join(repeatWords(12), " ") + " " + marker + " " + strings.Join(repeatWords(3), " ")
	out = FormatBankMarkdown(line)
	if !strings.Contains(out, marker) {
		t.Fatalf("HTML comment marker split or altered:\n%s", out)
	}
}

func utf8RuneCount(s string) int { return len([]rune(s)) }

// TestFormatBankMarkdownUnsplittableTokenStaysOverWidth: a single token with no space (a long URL-shaped
// run) is wider than the cap but is kept whole on its own line rather than cut mid-word.
func TestFormatBankMarkdownUnsplittableTokenStaysOverWidth(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("y", 150)
	out := FormatBankMarkdown("- " + token)
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (one unsplittable token): %q", len(lines), out)
	}
	if !strings.Contains(lines[0], token) {
		t.Fatalf("token was altered: %q", lines[0])
	}
	if utf8RuneCount(lines[0]) <= BankFmtMaxLine {
		t.Errorf("expected the unsplittable-token line to stay over-width, got %d runes", utf8RuneCount(lines[0]))
	}
}

// bankFmtFixture is synthetic text shaped like real decisionLog.md/sessionHistory.md entries — written
// fresh for this test, not copied from any real memory content.
const bankFmtFixture = `# decisionLog

harness_kind: bank

## 2026-10-21 — a long decision about wrap behavior and continuation indent columns for list items

why: because the implementation needed to wrap list items and plain paragraph lines at word boundaries without
ever splitting an inline code span, a markdown link, or an HTML comment marker, and the fix for the continuation
indent bug is covered by its own idempotence case.

- a bullet entry whose text runs well past the one hundred and twenty rune wrap column so it must be
  rewrapped at a word boundary with the continuation indented to the bullet's own content column
- short bullet
  - a nested bullet entry that also runs long enough to need wrapping onto a second continuation line too
1. a numbered item with enough words in it to push well past the wrap column and need its own rewrap
2. short item

See the [full note](http://example.com/some/long/path/that/keeps/the/whole/link/token/glued/together) and
run ` + "`claude-code memory fmt --check`" + ` before committing. <!-- id:deadbeef0123 -->
`

// TestFormatBankMarkdownLosslessWordEquality: word content survives — runs of whitespace collapse, but
// the ordered sequence of words in the document is identical before and after (assert word-equality,
// not byte-equality, per the documented contract).
func TestFormatBankMarkdownLosslessWordEquality(t *testing.T) {
	t.Parallel()
	out := FormatBankMarkdown(bankFmtFixture)
	before := strings.Fields(bankFmtFixture)
	after := strings.Fields(out)
	if len(before) != len(after) {
		t.Fatalf("word count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("word %d: before=%q after=%q", i, before[i], after[i])
		}
	}
}

// TestFormatBankMarkdownIdempotent: a second pass over already-wrapped text is a no-op, for the fixture
// and for the specific plain-paragraph-with-leading-indent case the implementer's fix targets.
func TestFormatBankMarkdownIdempotent(t *testing.T) {
	t.Parallel()
	once := FormatBankMarkdown(bankFmtFixture)
	twice := FormatBankMarkdown(once)
	if once != twice {
		t.Fatalf("not idempotent on the fixture:\nonce:\n%s\ntwice:\n%s", once, twice)
	}

	indented := "   " + strings.Join(repeatWords(15), " ")
	once = FormatBankMarkdown(indented)
	twice = FormatBankMarkdown(once)
	if once != twice {
		t.Fatalf("not idempotent on an indented paragraph:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
	for l := range strings.SplitSeq(once, "\n") {
		if !strings.HasPrefix(l, "   ") {
			t.Errorf("continuation line lost its original indent: %q", l)
		}
	}
}

// bankFmtRepoStore builds a cut-over store with one registered repo, returning the repo's bank dir.
func bankFmtRepoStore(t *testing.T) (globalDir, repoDir string) {
	t.Helper()
	migrateConfig1(t)
	cutover(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	return HomeBankDir(), dir
}

// TestBankFmtTargetsCoversSixFilesPlusColdTiersNeverMemoryOrIndex: BankFmtTargets walks the six typed
// files and archive/handoffs/reports under every store, and never index.md or MEMORY.md.
func TestBankFmtTargetsCoversSixFilesPlusColdTiersNeverMemoryOrIndex(t *testing.T) {
	global, repo := bankFmtRepoStore(t)
	for _, name := range bankFiles {
		write(t, filepath.Join(repo, name), "# "+name+"\nbody\n")
	}
	write(t, filepath.Join(repo, "archive", "sessionHistory-2026-01.md"), "# archived\nbody\n")
	write(t, filepath.Join(repo, "handoffs", "2026-01-01-x.md"), "# handoff\nbody\n")
	write(t, filepath.Join(repo, "reports", "review.md"), "# report\nbody\n")
	write(t, filepath.Join(repo, NativeIndexFile), "native MEMORY.md\n")
	write(t, filepath.Join(StoreDir(), CatalogFile), "catalog\n")
	write(t, filepath.Join(global, "decisionLog.md"), "# decisionLog\nbody\n")

	files, err := BankFmtTargets()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.Join(repo, "projectContext.md"):                    true,
		filepath.Join(repo, "activeContext.md"):                     true,
		filepath.Join(repo, "progress.md"):                          true,
		filepath.Join(repo, "decisionLog.md"):                       true,
		filepath.Join(repo, "conventions.md"):                       true,
		filepath.Join(repo, "sessionHistory.md"):                    true,
		filepath.Join(repo, "archive", "sessionHistory-2026-01.md"): true,
		filepath.Join(repo, "handoffs", "2026-01-01-x.md"):          true,
		filepath.Join(repo, "reports", "review.md"):                 true,
		filepath.Join(global, "decisionLog.md"):                     true,
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for f := range want {
		if !got[f] {
			t.Errorf("BankFmtTargets missing %s", f)
		}
	}
	for _, never := range []string{filepath.Join(repo, NativeIndexFile), filepath.Join(StoreDir(), CatalogFile)} {
		if got[never] {
			t.Errorf("BankFmtTargets included %s, want never", never)
		}
	}
}

// TestFormatBankCheckWritesNothingAndListsFiles: --check mode (check=true) reports every file that would
// change, writes nothing (mtimes untouched, bytes untouched), and the store stays uncommitted.
func TestFormatBankCheckWritesNothingAndListsFiles(t *testing.T) {
	global, _ := bankFmtRepoStore(t)
	target := filepath.Join(global, "decisionLog.md")
	longLine := "- " + strings.Join(repeatWords(15), " ")
	write(t, target, "# decisionLog\n\n"+longLine+"\n")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	beforeData, _ := os.ReadFile(target)
	headBefore := StoreRepo().headOrEmpty(t)

	changed, err := FormatBank(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != target {
		t.Fatalf("changed = %v, want exactly [%s]", changed, target)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("--check mode wrote the file (mtime changed)")
	}
	afterData, _ := os.ReadFile(target)
	if string(beforeData) != string(afterData) {
		t.Error("--check mode wrote the file (bytes changed)")
	}
	if headBefore != StoreRepo().headOrEmpty(t) {
		t.Error("--check mode committed the store")
	}
}

// TestFormatBankWritesAndCommitsOnlyOnChange: the default (non-check) pass rewrites a dirty file and
// commits the store; a second pass with nothing left to wrap makes no new commit.
func TestFormatBankWritesAndCommitsOnlyOnChange(t *testing.T) {
	global, _ := bankFmtRepoStore(t)
	target := filepath.Join(global, "decisionLog.md")
	longLine := "- " + strings.Join(repeatWords(15), " ")
	write(t, target, "# decisionLog\n\n"+longLine+"\n")

	changed, err := FormatBank(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != target {
		t.Fatalf("changed = %v, want exactly [%s]", changed, target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for l := range strings.SplitSeq(string(data), "\n") {
		if utf8RuneCount(l) > BankFmtMaxLine {
			t.Errorf("rewritten file still has an over-width line: %q", l)
		}
	}
	if before, after := strings.Fields(longLine), strings.Fields(string(data)); !wordsSubsequence(before, after) {
		t.Errorf("word content of the wrapped line lost: want words %v present in %v", before, after)
	}
	headAfterFirst := StoreRepo().headOrEmpty(t)
	if headAfterFirst == "" {
		t.Fatal("expected a commit after a real change, got no HEAD")
	}
	if st, err := StoreRepo().Status(); err != nil || len(st) != 0 {
		t.Fatalf("store dirty after fmt commit: status=%v err=%v", st, err)
	}

	changed, err = FormatBank(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("second pass changed = %v, want none (already wrapped)", changed)
	}
	if headAfterFirst != StoreRepo().headOrEmpty(t) {
		t.Error("a no-op pass created a new commit")
	}
}

// TestFormatBankNativeAndCatalogUntouched: MEMORY.md and the store catalog (index.md) are never rewritten,
// even with an over-width line, because FormatBank never targets them.
func TestFormatBankNativeAndCatalogUntouched(t *testing.T) {
	global, _ := bankFmtRepoStore(t)
	longLine := strings.Join(repeatWords(15), " ")
	nativePath := filepath.Join(global, NativeIndexFile)
	catalogPath := filepath.Join(StoreDir(), CatalogFile)
	write(t, nativePath, "native MEMORY.md\n"+longLine+"\n")
	write(t, catalogPath, "catalog\n"+longLine+"\n")
	nativeBefore, _ := os.ReadFile(nativePath)
	catalogBefore, _ := os.ReadFile(catalogPath)

	if _, err := FormatBank(false); err != nil {
		t.Fatal(err)
	}
	nativeAfter, _ := os.ReadFile(nativePath)
	catalogAfter, _ := os.ReadFile(catalogPath)
	if string(nativeBefore) != string(nativeAfter) {
		t.Error("MEMORY.md was rewritten by memory fmt")
	}
	if string(catalogBefore) != string(catalogAfter) {
		t.Error("index.md was rewritten by memory fmt")
	}
}

// headOrEmpty is Head() with the error folded away for test readability (Head() never errors; it
// returns "" when there is no commit yet).
func (r Repo) headOrEmpty(t *testing.T) string {
	t.Helper()
	h, _ := r.Head()
	return h
}

// wordsSubsequence reports whether every word in want appears, in order, somewhere in got.
func wordsSubsequence(want, got []string) bool {
	i := 0
	for _, w := range got {
		if i < len(want) && w == want[i] {
			i++
		}
	}
	return i == len(want)
}
