package worktree

import (
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// revalKind is the verdict of revalidate.
type revalKind string

const (
	revalReuse  revalKind = "reuse"
	revalRetest revalKind = "retest"
	revalFull   revalKind = "full"
)

// revalWhy is the typed reason recorded with a verdict.
type revalWhy string

const (
	whyNone        revalWhy = ""
	whyUnscoped    revalWhy = "first_gate_not_scoped"
	whyConflict    revalWhy = "rebase_conflict"
	whyModFile     revalWhy = "module_file_changed"
	whyUnowned     revalWhy = "unowned_file"
	whyTouched     revalWhy = "closure_touched"
	whyNewImporter revalWhy = "newly_in_scope"
)

// gateScope is what the first (passed) gate ran: Changed are the lane's packages, Tested every package it tested.
type gateScope struct {
	Changed, Tested map[string]bool
	Scoped          bool
	// Conflict marks a rebase conflict between the first gate and the re-check.
	Conflict bool
}

// reval is the verdict: reuse (Retest empty), retest these packages, or a full re-gate.
type reval struct {
	Kind   revalKind
	Full   bool
	Why    revalWhy
	Retest []string
}

// graph indexes the package list by repo-relative dir and by import path.
type graph struct {
	byDir  map[string]goPkg
	byPath map[string]string
	embed  map[string][]embedOwner
	// embedDir maps a package dir with embeds to whether any are non-test (deep).
	embedDir map[string]bool
}

type embedOwner struct {
	dir  string
	test bool
}

// hits: deep packages affect every importer, self ones their own tests; all (testdata) retests every package in scope.
type hits struct {
	deep, self map[string]bool
	all        bool
}

func newGraph(list []goPkg, abs string) graph {
	g := graph{map[string]goPkg{}, map[string]string{}, map[string][]embedOwner{}, map[string]bool{}}
	for _, p := range list {
		r, err := filepath.Rel(abs, p.Dir)
		if err != nil || r == ".." || strings.HasPrefix(r, "../") {
			continue
		}
		r = filepath.ToSlash(r)
		g.byDir[r], g.byPath[p.ImportPath] = p, r
		g.addEmbeds(r, p.EmbedFiles, false)
		g.addEmbeds(r, slices.Concat(p.TestEmbedFiles, p.XTestEmbedFiles), true)
	}
	return g
}

func (g graph) addEmbeds(dir string, files []string, test bool) {
	for _, e := range files {
		g.embedDir[dir] = g.embedDir[dir] || !test
		k := path.Join(dir, filepath.ToSlash(e))
		g.embed[k] = append(g.embed[k], embedOwner{dir, test})
	}
}

// closure returns each root plus every module package it reaches (a root's own test imports count, a dependency's not).
func closure(list []goPkg, abs string, roots []string) map[string]bool {
	return newGraph(list, abs).closure(roots)
}

func (g graph) closure(roots []string) map[string]bool {
	out, queue := map[string]bool{}, []string{}
	for _, r := range roots {
		p, ok := g.byDir[r]
		if !ok {
			continue
		}
		out[r] = true
		for _, i := range slices.Concat(p.Imports, p.TestImports, p.XTestImports) {
			queue = append(queue, g.byPath[i])
		}
	}
	for len(queue) > 0 {
		d := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		p, ok := g.byDir[d]
		if d == "" || out[d] || !ok {
			continue
		}
		out[d] = true
		for _, i := range p.Imports {
			queue = append(queue, g.byPath[i])
		}
	}
	return out
}

// owner returns the nearest package dir at or above dir; the root package owns a subdir file only under testdata.
func (g graph) owner(dir string, testdata bool) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if _, ok := g.byDir[d]; ok && (d != "." || dir == "." || testdata) {
			return d, true
		}
		if d == "." || d == "/" || d == "" {
			return "", false
		}
	}
}

// classify maps NUL-separated changed files to hits; a non-empty why demands a full re-gate.
func (g graph) classify(names string) (hits, revalWhy) {
	h := &hits{deep: map[string]bool{}, self: map[string]bool{}}
	for _, f := range textutil.SplitNUL(names) {
		if why := g.classifyFile(f, h); why != whyNone {
			return hits{}, why
		}
	}
	return *h, whyNone
}

func modFile(f string) bool {
	switch path.Base(f) {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}
	return strings.HasPrefix(f, "vendor/") || strings.Contains(f, "/vendor/")
}

func (g graph) classifyFile(f string, h *hits) revalWhy {
	if modFile(f) {
		return whyModFile
	}
	dir := path.Dir(f)
	if strings.HasSuffix(f, ".go") {
		// a .go file must sit in a listed package: deleted, excluded or unlisted packages are unclassifiable
		if _, ok := g.byDir[dir]; !ok {
			return whyUnowned
		}
		mark(h, dir, !strings.HasSuffix(f, "_test.go"))
		return whyNone
	}
	return g.classifyOther(f, dir, h)
}

func (g graph) classifyOther(f, dir string, h *hits) revalWhy {
	if strings.Contains("/"+f, "/testdata/") {
		h.all = true
		if d, ok := g.owner(dir, true); ok {
			mark(h, d, false)
		}
		return whyNone
	}
	if owners := g.embed[f]; len(owners) > 0 {
		for _, o := range owners {
			mark(h, o.dir, !o.test)
		}
		return whyNone
	}
	if g.markEmbedDirs(f, h) {
		return whyNone
	}
	if buildSource(f) {
		if d, ok := g.owner(dir, false); ok {
			mark(h, d, true)
			return whyNone
		}
	} else if pureDoc(f) {
		return whyNone
	}
	return whyUnowned
}

// markEmbedDirs credits f to every package whose directory tree holds it and has embeds (a pattern may add f).
func (g graph) markEmbedDirs(f string, h *hits) bool {
	found := false
	for d, deep := range g.embedDir {
		if d == "." || strings.HasPrefix(f, d+"/") {
			mark(h, d, deep)
			found = true
		}
	}
	return found
}

// buildSource reports a non-Go file extension the go tool compiles into its package.
func buildSource(f string) bool {
	switch path.Ext(f) {
	case ".c", ".h", ".s", ".S", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".m", ".f", ".syso", ".swig", ".swigcxx":
		return true
	}
	return false
}

// pureDoc reports a .md file under a location no test reads (docs/, plans/, memory/).
func pureDoc(f string) bool {
	return strings.HasSuffix(f, ".md") && (strings.HasPrefix(f, "docs/") || strings.HasPrefix(f, "plans/") || strings.HasPrefix(f, "memory/"))
}

func mark(h *hits, dir string, deep bool) {
	if deep {
		h.deep[dir] = true
		return
	}
	h.self[dir] = true
}

// revalidate picks the first gate's packages to retest at the rebased head; files are NUL-separated names.
func revalidate(list []goPkg, abs string, first gateScope, laneFiles, movedFiles string) reval {
	full := func(why revalWhy) reval { return reval{Kind: revalFull, Full: true, Why: why} }
	switch {
	case !first.Scoped:
		return full(whyUnscoped)
	case first.Conflict:
		return full(whyConflict)
	}
	g := newGraph(list, abs)
	lane, why := g.classify(laneFiles)
	if why != whyNone {
		return full(why)
	}
	moved, why := g.classify(movedFiles)
	if why != whyNone {
		return full(why)
	}
	lpk := map[string]bool{}
	for _, m := range []map[string]bool{lane.deep, lane.self} {
		for d := range m {
			lpk[d] = true
		}
	}
	// scope must match scopedGoGate in gate.go: lane packages plus their direct importers (TestRevalidateScopeMatchesGate)
	return g.retestSet(first, reverseDeps(list, abs, lpk), moved)
}

func (g graph) retestSet(first gateScope, scope map[string]bool, moved hits) reval {
	out := reval{Kind: revalReuse}
	for _, p := range slices.Sorted(maps.Keys(scope)) {
		switch {
		case !first.Tested[p]:
			out.Retest = append(out.Retest, p)
			if out.Why == whyNone {
				out.Why = whyNewImporter
			}
		case moved.all || moved.self[p] || anyIn(g.closure([]string{p}), moved.deep):
			out.Retest = append(out.Retest, p)
			out.Why = whyTouched
		}
	}
	if len(out.Retest) > 0 {
		out.Kind = revalRetest
	}
	return out
}

func anyIn(set, probe map[string]bool) bool {
	for k := range probe {
		if set[k] {
			return true
		}
	}
	return false
}
