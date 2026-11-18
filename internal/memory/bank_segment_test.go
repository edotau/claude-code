package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// A native topic file in StoreDir() belongs to a repo's native corpus.
func TestCorpusFilesIncludesGlobalNativeTopic(t *testing.T) {
	root := isolatedConfig1(t)
	initStore(t)
	if err := EnsureBank(""); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(StoreDir(), "global-topic.md")
	write(t, topic, "---\nname: global-topic\n---\n\nglobal fact\n")

	var found bool
	for _, f := range corpusFiles() {
		found = found || f.Path == topic && f.Bank == "native"
	}
	if !found {
		t.Errorf("corpusFiles(%s) missing global topic %s", root, topic)
	}
}

// A topic or knowledge note saved with CRLF endings is indexed like its LF twin, frontmatter stripped.
func TestParseBlocksIndexesCRLFNotes(t *testing.T) {
	note := []byte("---\r\nname: crlf-topic\r\ndescription: windows saved\r\n---\r\n\r\nzebra fact\r\n")
	native := parseBlocks(segNative, "t.md", note)
	if len(native) != 1 || !strings.HasPrefix(native[0], "crlf-topic — windows saved\n") || !strings.Contains(native[0], "zebra fact") {
		t.Errorf("CRLF native topic blocks = %q", native)
	}
	knowledge := parseBlocks(segKnowledge, "k.md", note)
	if len(knowledge) != 1 || strings.Contains(knowledge[0], "name:") || knowledge[0] != "zebra fact" {
		t.Errorf("CRLF knowledge blocks = %q", knowledge)
	}
	if got := strings.TrimSpace(stripFrontmatter(string(note))); got != "zebra fact" {
		t.Errorf("stripFrontmatter(CRLF) = %q", got)
	}
}

// frontmatterValue tolerates whitespace on the fence line and never coerces a scalar's YAML type.
func TestFrontmatterValueTolerantFencesAndRawValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, key, want string
	}{
		{"trailing-space fence", "---  \nname: a\n---  \nbody", "name", "a"},
		{"indented fence", "  ---\nname: a\n  ---\nbody", "name", "a"},
		{"no final newline", "---\nname: a\n---", "name", "a"},
		{"name: yes stays raw text, not a bool", "---\nname: yes\n---\nbody", "name", "yes"},
		{"description: [draft] stays raw text, not a list", "---\ndescription: [draft]\n---\nbody", "description", "[draft]"},
		{"name: 007 stays raw text, not a number", "---\nname: 007\n---\nbody", "name", "007"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := frontmatterValue(tc.raw, tc.key); got != tc.want {
				t.Errorf("frontmatterValue(%q, %q) = %q, want %q", tc.raw, tc.key, got, tc.want)
			}
		})
	}
}

// Only an exact "\n---\n" fence closes front matter; scalars stay raw text.
func TestTopicFrontmatterFencesAndRawValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, wantName, wantDesc string
		wantOK                        bool
	}{
		{"trailing-space fence is unclosed (strict \\n---\\n)", "---  \nname: a\n---  \nbody", "", "", false},
		{"indented fence is unclosed (strict ---\\n opener)", "  ---\nname: a\n  ---\nbody", "", "", false},
		{"no final newline is unclosed (strict \\n---\\n closer)", "---\nname: a\n---", "", "", false},
		{"name: yes stays raw text, not a bool", "---\nname: yes\n---\nbody", "yes", "", true},
		{"description: [draft] stays raw text, not a list", "---\nname: x\ndescription: [draft]\n---\nbody", "x", "[draft]", true},
		{"name: 007 stays raw text, not a number", "---\nname: 007\n---\nbody", "007", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, desc, _, ok := topicFrontmatter(tc.raw)
			if ok != tc.wantOK || name != tc.wantName || desc != tc.wantDesc {
				t.Errorf("topicFrontmatter(%q) = name=%q desc=%q ok=%v, want name=%q desc=%q ok=%v",
					tc.raw, name, desc, ok, tc.wantName, tc.wantDesc, tc.wantOK)
			}
		})
	}
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

// readIfExists returns a file's bytes, nil for an absent file, and every other read error.
func TestReadIfExists(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	dir := t.TempDir()
	if b, err := readIfExists(dir); !errors.Is(err, syscall.EISDIR) || len(b) != 0 {
		t.Errorf("directory: %q, %v; want the EISDIR read error", b, err)
	}
	if b, err := readIfExists(filepath.Join(dir, "nope")); b != nil || err != nil {
		t.Errorf("absent: %q, %v; want nil, nil", b, err)
	}
	write(t, filepath.Join(dir, "topic.md"), "body\n")
	if b, err := readIfExists(filepath.Join(dir, "topic.md")); string(b) != "body\n" || err != nil {
		t.Errorf("present: %q, %v; want its bytes", b, err)
	}
}

func pathsOf(files []corpusFile, label string) []string {
	var out []string
	for _, f := range files {
		if f.Bank == label {
			out = append(out, f.Path)
		}
	}
	return out
}

func TestCorpusFilesOwnershipInEachLayout(t *testing.T) {
	runRoot(t, func(t *testing.T, root string) {
		if _, err := Init(); err != nil {
			t.Fatal(err)
		}
		topic, arc := Path("topic.md"), Sub("archive", "old.md")
		for _, p := range []string{topic, arc} {
			write(t, p, "---\nname: x\n---\nzebra\n")
		}
		files := corpusFiles()
		native, archive := pathsOf(files, "native"), pathsOf(files, "archive")
		if !slices.Contains(native, topic) {
			t.Errorf("native corpus lacks %s: %v", topic, native)
		}
		for _, p := range native {
			if slices.Contains(bankFiles, filepath.Base(p)) {
				t.Errorf("native corpus holds %s: a typed file", p)
			}
		}
		if !slices.Contains(archive, arc) {
			t.Errorf("archive corpus = %v, want %s", archive, arc)
		}
		if got := pathsOf(files, "home"); len(got) != len(bankFiles) || got[0] != Path(bankFiles[0]) {
			t.Errorf("typed files = %v", got)
		}
	})
}

func TestCorpusBlocksReturnsReadErrors(t *testing.T) {
	isolatedConfig1(t)
	file := filepath.Join(StoreDir(), "decisionLog.md")
	write(t, file, "## Hidden\nbody\n")
	denyBankAccess(t, file)
	hits, err := corpusBlocks()
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("corpusBlocks = %v, %v; want file permission error", hits, err)
	}
}

// The sum and ref a segment stores per document must equal what hashing the text gives, marked or not.
func TestSegmentStoresBlockSumAndRef(t *testing.T) {
	blocks := []string{"## marked entry\nrouter retry <!-- id:0a1b2c3d -->", "- unmarked   rule\twith\n  spacing router"}
	seg := encodeSegment(blocks)
	if !seg.Valid() || seg.Docs() != len(blocks) {
		t.Fatalf("segment invalid or wrong doc count %d", seg.Docs())
	}
	for i, block := range blocks {
		text, _ := seg.Doc(i)
		sum, ref := seg.DocRef(i)
		if sum != blockSum(text) || ref != blockRef(text, "") {
			t.Errorf("doc %d: stored (%x, %q), hashed (%x, %q)", i, sum, ref, blockSum(text), blockRef(text, ""))
		}
		if hit := (SearchHit{Block: block, sum: sum}); hit.key() != blockKey(block) {
			t.Errorf("doc %d: key from stored sum %q != blockKey %q", i, hit.key(), blockKey(block))
		}
	}
}
