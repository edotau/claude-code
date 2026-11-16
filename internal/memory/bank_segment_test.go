package memory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A native topic file in HomeBankDir() belongs to a repo's native corpus.
func TestCorpusFilesIncludesGlobalNativeTopic(t *testing.T) {
	root := isolatedConfig1(t)
	initStore(t)
	if _, err := EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(HomeBankDir(), "global-topic.md")
	write(t, topic, "---\nname: global-topic\n---\n\nglobal fact\n")

	var found bool
	for _, f := range corpusFiles(root) {
		if f.Path == topic {
			found = true
			if f.Bank != "native" {
				t.Errorf("global topic bank = %q, want native", f.Bank)
			}
		}
	}
	if !found {
		t.Errorf("corpusFiles(%s) missing global topic %s", root, topic)
	}
}

// routedFiles lists only the named stores other than root's own: never root's, never an unnamed one.
func TestRoutedFilesExcludesOwnStore(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	roots, dirs := map[string]string{}, map[string]string{}
	for _, name := range []string{"proja", "projb", "projc"} {
		roots[name] = gitRepo(t, t.TempDir(), name)
		dir, err := EnsureRepoDir(roots[name])
		if err != nil {
			t.Fatal(err)
		}
		dirs[name] = dir
	}
	files := routedFiles(roots["proja"], "proja projb")
	if len(files) == 0 {
		t.Fatal("routedFiles listed nothing for the named projb")
	}
	for _, f := range files {
		if f.Bank != "projb" || filepath.Dir(f.Path) != dirs["projb"] {
			t.Errorf("routedFiles listed %+v, want only projb's files under %s", f, dirs["projb"])
		}
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

// otherBank scaffolds a second repo's bank so the flat store has a foreign key to attribute files to.
func otherBank(t *testing.T) (root string, b Bank) {
	t.Helper()
	root = gitRepo(t, canonTemp(t, t.TempDir()), "other")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	return root, BankFor(root)
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
	eachLayoutRoot(t, func(t *testing.T, root string) {
		if _, err := Init(root); err != nil {
			t.Fatal(err)
		}
		_, other := otherBank(t)
		b := BankFor(root)
		own, foreign, homeTopic := b.Path("own-topic.md"), other.Path("foreign-topic.md"), HomeBank().Path("home-topic.md")
		ownArc, foreignArc := b.Sub("archive", "old.md"), other.Sub("archive", "old.md")
		for _, p := range []string{own, foreign, homeTopic, ownArc, foreignArc} {
			write(t, p, "---\nname: x\n---\nzebra\n")
		}
		files := corpusFiles(root)
		native, archive := pathsOf(files, "native"), pathsOf(files, "archive")
		for _, want := range []string{own, homeTopic} {
			if !slices.Contains(native, want) {
				t.Errorf("native corpus lacks %s: %v", want, native)
			}
		}
		for _, p := range native {
			if p == foreign || slices.Contains(bankFiles, strings.TrimPrefix(filepath.Base(p), b.Prefix())) {
				t.Errorf("native corpus holds %s: a foreign or typed file", p)
			}
		}
		if !slices.Contains(archive, ownArc) || slices.Contains(archive, foreignArc) {
			t.Errorf("archive corpus = %v, want %s and not %s", archive, ownArc, foreignArc)
		}
		if got := pathsOf(files, "local"); len(got) != len(bankFiles) || got[0] != b.Path(bankFiles[0]) {
			t.Errorf("local typed files = %v", got)
		}
	})
}

func TestRoutedFilesStayInTheNamedBank(t *testing.T) {
	eachLayoutRoot(t, func(t *testing.T, root string) {
		if _, err := Init(root); err != nil {
			t.Fatal(err)
		}
		_, other := otherBank(t)
		write(t, other.Path("foreign-topic.md"), "---\nname: x\n---\nzebra\n")
		files := routedFiles(root, "about other things")
		if len(files) == 0 {
			t.Fatal("routedFiles listed nothing for the named bank")
		}
		for _, f := range files {
			if f.Bank != "other" || ownerKey(filepath.Base(f.Path), BankKeys()) != "other" && storeFlat() {
				t.Errorf("routed %+v, want only other's files", f)
			}
			if strings.HasPrefix(filepath.Base(f.Path), "other--projectContext") && f.Mode != segEntries {
				t.Errorf("typed file %s routed as native", f.Path)
			}
		}
		if len(routedFiles(root, "proj other")) != len(files) {
			t.Error("routedFiles listed root's own bank")
		}
	})
}
