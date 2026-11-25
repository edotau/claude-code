package memory

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
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

func TestFrontWriteReadIdentity(t *testing.T) {
	vals := []string{
		"Grouped feedback: Dockerfile", "a # b", "[x]", "{x}", "*x", "&x", "!x", "|x", ">x", "'x'", `"x"`, "%x", "@x",
		"`x`", "#x", "- x", "? x", "-", "say \"hi\" \\ there", `back\slash`, "café 日本", "", "plain text",
		"true", "123", "x:", " lead", "tab\there", "it's", "a, b",
	}
	for _, v := range vals {
		d, err := ParseDoc("n.md", []byte("---\nname: old\ndescription: old\n---\n- a\n")).withFront(noteFront{Name: v, Description: v})
		if err != nil {
			t.Fatalf("WithFront(%q): %v", v, err)
		}
		got, ok, err := d.frontFields()
		if err != nil || !ok || got.Name != v || got.Description != v {
			t.Errorf("value %q read back %+v %v %v from %q", v, got, ok, err, d.Front)
		}
		again, err := ParseDoc("n.md", d.Render()).withFront(noteFront{Name: v, Description: v})
		if err != nil || strings.Join(again.Front, "\n") != strings.Join(d.Front, "\n") {
			t.Errorf("value %q not stable: %v %q", v, err, again.Front)
		}
	}
	d, _ := ParseDoc("n.md", []byte("---\nname: old\n---\n")).withFront(noteFront{Name: "plain text"})
	if d.Front[1] != "name: plain text" {
		t.Errorf("safe value was quoted: %q", d.Front)
	}
}

func TestFrontRefusesBadInput(t *testing.T) {
	if _, err := ParseDoc("n.md", nil).withFront(noteFront{Name: "a\nb"}); err == nil {
		t.Error("newline value accepted")
	}
	bad := ParseDoc("n.md", []byte("---\nname: \"open\n---\n"))
	if _, ok, err := bad.frontFields(); !ok || err == nil {
		t.Errorf("parse error dropped: ok=%v err=%v", ok, err)
	}
	if _, err := bad.withFront(noteFront{Name: "x"}); err == nil {
		t.Error("WithFront rewrote a front block it could not parse")
	}
}

func TestFrontFlowMetadata(t *testing.T) {
	d := ParseDoc("n.md", []byte("---\nname: n\nmetadata: {type: x}\n---\n"))
	if f, _, err := d.frontFields(); err != nil || f.Type != "x" {
		t.Fatalf("flow type = %+v %v", f, err)
	}
	if _, err := d.withFront(noteFront{Name: "n", Type: "y"}); !errors.Is(err, errFrontFlow) {
		t.Errorf("flow edit err = %v, want ErrFrontFlow", err)
	}
	if _, err := d.withFront(noteFront{Name: "m", Type: "x"}); err != nil {
		t.Errorf("unrelated edit refused: %v", err)
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

func TestFrontFields(t *testing.T) {
	d := ParseDoc("note.md", []byte(docFixtures["note.md"]))
	f, ok, err := d.frontFields()
	if !ok || err != nil || f != (noteFront{Name: "n", Description: "d", Type: "feedback"}) {
		t.Fatalf("FrontFields = %+v %v %v", f, ok, err)
	}
	nd, err := d.withFront(noteFront{Name: "n", Description: "e", Type: "feedback"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(docFixtures["note.md"], `description: "d"`, "description: e", 1)
	if string(nd.Render()) != want || string(d.Render()) != docFixtures["note.md"] {
		t.Errorf("WithFront render = %q", nd.Render())
	}
	fresh, err := ParseDoc("x.md", []byte("- a\n")).withFront(noteFront{Name: "x", Type: "t"})
	if g, _, _ := fresh.frontFields(); err != nil || g.Name != "x" || g.Type != "t" {
		t.Errorf("fresh front = %+v", g)
	}
}
