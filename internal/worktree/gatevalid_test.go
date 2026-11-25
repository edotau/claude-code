package worktree

import (
	"slices"
	"strings"
	"testing"
)

const gvMod = "m/"

func gvPkg(rel string, imports ...string) goPkg {
	for i, imp := range imports {
		imports[i] = gvMod + imp
	}
	return goPkg{ImportPath: gvMod + rel, Dir: "/r/" + rel, Imports: imports}
}

// gvList: lane <- a <- w, a -> b -> c, e -> lane (added by the case), emb embeds x.txt (and imports lane), d alone.
func gvList() []goPkg {
	emb := gvPkg("emb", "lane")
	emb.EmbedFiles = []string{"x.txt"}
	emb.TestEmbedFiles = []string{"testembed.txt"}
	return []goPkg{
		gvPkg("lane"), gvPkg("a", "lane", "b"), gvPkg("b", "c"), gvPkg("c"), gvPkg("d"),
		gvPkg("w", "a"), emb,
	}
}

func gvScope() gateScope {
	set := func(s ...string) map[string]bool {
		m := map[string]bool{}
		for _, v := range s {
			m[v] = true
		}
		return m
	}
	return gateScope{Changed: set("lane"), Tested: set("lane", "a", "emb"), Scoped: true}
}

func nul(files ...string) string { return strings.Join(files, "\x00") + "\x00" }

type gvCase struct {
	name   string
	mutate func(*gateScope)
	moved  string
	addE   bool
	kind   revalKind
	why    revalWhy
	retest []string
}

var gvCases = []gvCase{
	{name: "empty move reuses", kind: revalReuse},
	{name: "disjoint change reuses", moved: nul("d/d.go"), kind: revalReuse},
	{name: "unowned markdown reuses", moved: nul("docs/readme.md"), kind: revalReuse},
	{name: "change in tested package", moved: nul("a/a.go"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "change in transitive dependency", moved: nul("c/c.go"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "breaking commit in closure never reuses", moved: nul("b/b.go", "d/d.go"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "new importer of lane package at T'", moved: nul("e/e.go"), addE: true, kind: revalRetest, why: whyNewImporter, retest: []string{"e"}},
	{name: "test file in dependency spares importers", moved: nul("b/b_test.go"), kind: revalReuse},
	{name: "test file invalidates its package only", moved: nul("a/a_test.go"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "test file of lane package spares importers", moved: nul("lane/lane_test.go"), kind: revalRetest, why: whyTouched, retest: []string{"lane"}},
	{name: "embedded file retests embedder", moved: nul("emb/x.txt"), kind: revalRetest, why: whyTouched, retest: []string{"emb"}},
	{name: "test embed file retests its package", moved: nul("emb/testembed.txt"), kind: revalRetest, why: whyTouched, retest: []string{"emb"}},
	{name: "deleted embedded file retests embedder", moved: nul("emb/gone.txt"), kind: revalRetest, why: whyTouched, retest: []string{"emb"}},
	{name: "testdata invalidates its package", moved: nul("a/testdata/f.json"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "nested testdata invalidates owner", moved: nul("a/sub/testdata/f.json"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "cgo C file", moved: nul("a/a.c"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "build-constrained file", moved: nul("a/a_windows.go"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "generated file", moved: nul("a/zz_generated.go"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "non-Go file in package dir", moved: nul("a/notes.txt"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "non-Go file in unlisted subdir", moved: nul("a/fixtures/in.json"), kind: revalRetest, why: whyTouched, retest: []string{"a"}},
	{name: "go.mod", moved: nul("go.mod"), kind: revalFull, why: whyModFile},
	{name: "go.sum", moved: nul("d/d.go", "go.sum"), kind: revalFull, why: whyModFile},
	{name: "go.work", moved: nul("go.work"), kind: revalFull, why: whyModFile},
	{name: "vendor path", moved: nul("vendor/x/y.go"), kind: revalFull, why: whyModFile},
	{name: "unowned non-markdown file", moved: nul("scripts/x.sh"), kind: revalFull, why: whyUnowned},
	{name: "deleted package", moved: nul("old/gone.go"), kind: revalFull, why: whyUnowned},
	{name: "go file in unlisted dir", moved: nul("a/inner/x.go"), kind: revalFull, why: whyUnowned},
	{name: "first gate not scoped", mutate: func(s *gateScope) { s.Scoped = false }, kind: revalFull, why: whyUnscoped},
	{name: "rebase conflict", mutate: func(s *gateScope) { s.Conflict = true }, kind: revalFull, why: whyConflict},
}

func TestRevalidate(t *testing.T) {
	for _, c := range gvCases {
		t.Run(c.name, func(t *testing.T) {
			first := gvScope()
			if c.mutate != nil {
				c.mutate(&first)
			}
			list := gvList()
			if c.addE {
				list = append(list, gvPkg("e", "lane"))
			}
			got := revalidate(list, "/r", first, nul("lane/lane.go"), c.moved)
			if got.Kind != c.kind || got.Why != c.why || !slices.Equal(got.Retest, c.retest) || got.Full != (c.kind == revalFull) {
				t.Fatalf("revalidate(%q) = %+v, want kind=%s why=%q retest=%v", c.moved, got, c.kind, c.why, c.retest)
			}
		})
	}
}

func TestRevalidateLaneFiles(t *testing.T) {
	got := revalidate(gvList(), "/r", gvScope(), nul("tools/gen.sh"), nul("d/d.go"))
	if got.Kind != revalFull || got.Why != whyUnowned {
		t.Fatalf("unowned lane file = %+v, want full/unowned", got)
	}
}

func TestRevalidateRootPackage(t *testing.T) {
	list := []goPkg{gvPkg(".")}
	list[0].ImportPath = "m"
	first := gateScope{Changed: map[string]bool{".": true}, Tested: map[string]bool{".": true}, Scoped: true}
	if got := revalidate(list, "/r", first, nul("main.go"), nul("scripts/x.txt")); got.Kind != revalFull {
		t.Fatalf("subdir non-Go file must not be root-owned: %+v", got)
	}
	if got := revalidate(list, "/r", first, nul("main.go"), nul("testdata/x.txt")); got.Kind != revalRetest {
		t.Fatalf("root testdata must retest root: %+v", got)
	}
}

func TestClosure(t *testing.T) {
	list := gvList()
	list[0].TestImports = []string{gvMod + "d"}
	got := closure(list, "/r", []string{"a"})
	for _, want := range []string{"a", "lane", "b", "c"} {
		if !got[want] {
			t.Errorf("closure(a) misses %s: %v", want, got)
		}
	}
	// a dependency's test imports are not followed, a root's own are
	if got["d"] || got["w"] {
		t.Errorf("closure(a) = %v, must not hold d or w", got)
	}
	if !closure(list, "/r", []string{"lane"})["d"] {
		t.Error("closure(lane) must follow the root's own test imports")
	}
}
