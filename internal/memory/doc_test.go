package memory

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const docStoreCap = 20000

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
	root := os.Getenv("MEMORY_ROUNDTRIP_STORE")
	if root == "" {
		root = "/Users/edotau/.claude/memory"
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("store %s absent", root)
	}
	n := 0
	err := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			if err == nil && de.Name() == ".git" {
				return filepath.SkipDir
			}
			return err
		}
		if strings.HasSuffix(p, ".md") && n < docStoreCap {
			n += roundTripFile(t, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("round-trip: %d files, 0 differ (cap %d)", n, docStoreCap)
}

// roundTripFile returns 1 when p was read, failing the test on a differing byte.
func roundTripFile(t *testing.T, p string) int {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	if out := ParseDoc(p, b).Render(); !bytes.Equal(out, b) {
		t.Errorf("%s: first differing byte %d", p, firstDiff(out, b))
	}
	return 1
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

func TestFrontFields(t *testing.T) {
	d := ParseDoc("note.md", []byte(docFixtures["note.md"]))
	f, ok := d.FrontFields()
	if !ok || f != (Front{Name: "n", Description: "d", Type: "feedback"}) {
		t.Fatalf("FrontFields = %+v %v", f, ok)
	}
	nd := d.WithFront(Front{Name: "n", Description: "e", Type: "feedback"})
	want := strings.Replace(docFixtures["note.md"], `description: "d"`, "description: e", 1)
	if string(nd.Render()) != want || string(d.Render()) != docFixtures["note.md"] {
		t.Errorf("WithFront render = %q", nd.Render())
	}
	fresh := ParseDoc("x.md", []byte("- a\n")).WithFront(Front{Name: "x", Type: "t"})
	if g, _ := fresh.FrontFields(); g.Name != "x" || g.Type != "t" {
		t.Errorf("fresh front = %+v", g)
	}
}
