package memory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// A native topic file in paths.MemoryStoreDir() belongs to a repo's native corpus.
func TestCorpusFilesIncludesGlobalNativeTopic(t *testing.T) {
	root := isolatedConfig1(t)
	initStore(t)
	if err := EnsureBank(""); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(paths.MemoryStoreDir(), "global-topic.md")
	write(t, topic, "---\nname: global-topic\n---\n\nglobal fact\n")

	var found bool
	for _, f := range corpusOf() {
		found = found || f.Path == topic && f.Bank == "native"
	}
	if !found {
		t.Errorf("corpusOf(%s) missing global topic %s", root, topic)
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
		topic, arc := Path("topic.md"), paths.MemorySub("archive", "old.md")
		for _, p := range []string{topic, arc} {
			write(t, p, "---\nname: x\n---\nzebra\n")
		}
		files := corpusOf()
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
	file := filepath.Join(paths.MemoryStoreDir(), "decisionLog.md")
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

// containingOracle is the per-term scan containing replaced.
func containingOracle(s segView, term string) []string {
	termStart := segHeader + s.Docs()*docStride
	nterms := int(binary.LittleEndian.Uint32(s[termStart:]))
	postStart := termStart + 4 + nterms*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	var out []string
	for i := range nterms {
		off := termStart + 4 + i*16
		start := blobStart + int(binary.LittleEndian.Uint32(s[off:]))
		t := s[start : start+int(binary.LittleEndian.Uint32(s[off+4:]))]
		if len(t) > len(term) && bytes.Contains(t, []byte(term)) {
			out = append(out, string(t))
		}
	}
	return out
}

// A segment whose term starts are scrambled (a damaged or foreign file) must end the term walk, never loop.
func TestContainingTerminatesOnScrambledTermStarts(t *testing.T) {
	base := encodeSegment([]string{"- agent subagent agents reagent agentic", "- routing router rerouting routed"})
	termStart := segHeader + base.Docs()*docStride
	nterms := int(binary.LittleEndian.Uint32(base[termStart:]))
	var blobLen int
	for i := range nterms {
		blobLen += int(binary.LittleEndian.Uint32(base[termStart+4+i*16+4:]))
	}
	r := rand.New(rand.NewSource(1))
	for n := range 200 {
		seg := segView(bytes.Clone(base))
		for i := range nterms {
			binary.LittleEndian.PutUint32(seg[termStart+4+i*16:], uint32(r.Intn(blobLen)))
		}
		done := make(chan bool, 1)
		go func() { defer func() { _ = recover(); done <- true }(); seg.containing("agent") }() // a panic is not a loop
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("scramble %d: containing did not terminate", n)
		}
	}
}

// The one-pass term scan must return what the per-term scan did, including hits that straddle two stored terms.
func TestContainingMatchesPerTermScan(t *testing.T) {
	seg := encodeSegment([]string{"- agent subagent agents reagent agentic", "- routing router rerouting routed", "- fta gyy class subclass classes"})
	for _, q := range []string{"agent", "agent", "rout", "outer", "class", "zzzz", "tag", "agentic", "gyy", "fta", "a"} {
		got, want := seg.containing(q), containingOracle(seg, q)
		if !slices.Equal(got, want) {
			t.Errorf("containing(%q) = %v, want %v", q, got, want)
		}
	}
	if got := seg.containing("agent"); len(got) != 4 {
		t.Errorf("containing(agent) = %v, want agents, agentic, reagent, subagent", got)
	}
}

// TestSortByScoreNaNMatchesOldSort: a NaN score orders exactly as the replaced sort.SliceStable did.
func TestSortByScoreNaNMatchesOldSort(t *testing.T) {
	var hits []SearchHit
	for i := range 90 {
		score := float64(i * 7 % 5)
		if i%6 == 0 {
			score = math.NaN()
		}
		hits = append(hits, SearchHit{Score: score, ID: strconv.Itoa(i)})
	}
	want := append([]SearchHit(nil), hits...)
	sort.SliceStable(want, func(i, j int) bool { return want[i].Score > want[j].Score })
	got := sortByScore(append([]SearchHit(nil), hits...))
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("row %d: got id %s, want %s", i, got[i].ID, want[i].ID)
		}
	}
}

// stripIDMarks must match the whitespace-eating regexp it replaced on well-formed, unterminated and stacked markers.
func TestStripIDMarksMatchesMarkerRegexp(t *testing.T) {
	re := regexp.MustCompile(`\s*<!-- id:[^>]*-->`)
	for _, s := range []string{"", "plain", "a <!-- id:0000002a-->", "a \n<!-- id:x-->  <!-- id:y--> b", "a <!-- id:", "<!-- id:x > --> b <!-- id:-->", "x<!-- id:ab>--> y<!-- id:c-->"} {
		if got, want := stripIDMarks(s), re.ReplaceAllString(s, ""); got != want {
			t.Errorf("stripIDMarks(%q) = %q, want %q", s, got, want)
		}
	}
}
