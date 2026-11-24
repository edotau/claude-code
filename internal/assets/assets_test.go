package assets

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

const importPath = "github.com/edotau/claude-code/internal/assets"

// inventory is the closed list of embedded files: adding or deleting one fails until this table changes.
var inventory = []struct {
	path, kind, reader string
	read               func() []byte
}{
	{"files/catalog/ranks.json", "json", "RankTable", RankTable},
	{"files/review/source/retired_words.json", "json", "RetiredWords", RetiredWords},
	{"files/ui/theme/default_config.json", "json", "ThemeDefaultConfig", ThemeDefaultConfig},
	{"files/ui/theme/default_palettes.json", "json", "ThemeDefaultPalettes", ThemeDefaultPalettes},
	{"files/memory/templates/activeContext.md", "md", "MemoryActiveContextSeed", func() []byte { return []byte(MemoryActiveContextSeed()) }},
	{"files/memory/templates/conventions.md", "md", "MemoryConventionsSeed", func() []byte { return []byte(MemoryConventionsSeed()) }},
	{"files/memory/templates/decisionLog.md", "md", "MemoryDecisionLogSeed", func() []byte { return []byte(MemoryDecisionLogSeed()) }},
	{"files/memory/templates/progress.md", "md", "MemoryProgressSeed", func() []byte { return []byte(MemoryProgressSeed()) }},
	{"files/memory/templates/projectContext.md", "md", "MemoryProjectContextSeed", func() []byte { return []byte(MemoryProjectContextSeed()) }},
	{"files/memory/templates/sessionHistory.md", "md", "MemorySessionHistorySeed", func() []byte { return []byte(MemorySessionHistorySeed()) }},
	{"files/memory/prompts/dream.md", "md", "MemoryDreamProcedure", func() []byte { return read("files/memory/prompts/dream.md") }},
	{"files/memory/prompts/init.md", "md", "MemoryInitProcedure", func() []byte { return read("files/memory/prompts/init.md") }},
	{"files/memory/prompts/promote.md", "md", "MemoryPromoteProcedure", func() []byte { return read("files/memory/prompts/promote.md") }},
	{"files/memory/prompts/review.md", "md", "MemoryReviewProcedure", func() []byte { return read("files/memory/prompts/review.md") }},
	{"files/memory/prompts/update.md", "md", "MemoryUpdateProcedure", func() []byte { return read("files/memory/prompts/update.md") }},
	{"files/memory/prompts/update-opening.tmpl", "tmpl", "MemoryUpdateWorker", func() []byte { return read("files/memory/prompts/update-opening.tmpl") }},
	{"files/memory/prompts/dream-opening.tmpl", "tmpl", "MemoryDreamWorker", func() []byte { return read("files/memory/prompts/dream-opening.tmpl") }},
	{"files/memory/prompts/legacy-update-opening.tmpl", "tmpl", "MemoryLegacyUpdateWorker", func() []byte { return read("files/memory/prompts/legacy-update-opening.tmpl") }},
	{"files/memory/prompts/legacy-dream-opening.tmpl", "tmpl", "MemoryLegacyDreamWorker", func() []byte { return read("files/memory/prompts/legacy-dream-opening.tmpl") }},
	{"files/memory/prompts/historical.txt", "txt", "MemoryHistoricalRule", func() []byte { return read("files/memory/prompts/historical.txt") }},
	{"files/memory/prompts/update-worker.tmpl", "tmpl", "MemoryUpdatePrompt", func() []byte { return read("files/memory/prompts/update-worker.tmpl") }},
	{"files/memory/prompts/transcript-pointer.tmpl", "tmpl", "MemoryTranscriptPointer", func() []byte { return read("files/memory/prompts/transcript-pointer.tmpl") }},
	{"files/memory/prompts/session-digest.tmpl", "tmpl", "MemorySessionDigest", func() []byte { return read("files/memory/prompts/session-digest.tmpl") }},
	{"files/memory/prompts/git-activity.tmpl", "tmpl", "MemoryGitActivity", func() []byte { return read("files/memory/prompts/git-activity.tmpl") }},
	{"files/memory/prompts/session-tags.tmpl", "tmpl", "MemorySessionTags", func() []byte { return read("files/memory/prompts/session-tags.tmpl") }},
	{"files/memory/prompts/dream-worker.tmpl", "tmpl", "MemoryDreamPrompt", func() []byte { return read("files/memory/prompts/dream-worker.tmpl") }},
	{"files/memory/prompts/dream-unchanged.txt", "txt", "MemoryDreamUnchanged", func() []byte { return read("files/memory/prompts/dream-unchanged.txt") }},
	{"files/memory/prompts/open-tasks.tmpl", "tmpl", "MemoryOpenTasks", func() []byte { return read("files/memory/prompts/open-tasks.tmpl") }},
	{"files/memory/prompts/rtk-misses.tmpl", "tmpl", "MemoryRTKMisses", func() []byte { return read("files/memory/prompts/rtk-misses.tmpl") }},
	{"files/memory/prompts/index-hint.txt", "txt", "MemoryIndexHint", func() []byte { return read("files/memory/prompts/index-hint.txt") }},
	{"files/memory/prompts/block-begin.txt", "txt", "MemoryBlockBegin", func() []byte { return read("files/memory/prompts/block-begin.txt") }},
	{"files/memory/prompts/bank-head.tmpl", "tmpl", "MemoryBankHead", func() []byte { return read("files/memory/prompts/bank-head.tmpl") }},
	{"files/memory/prompts/more-entries.tmpl", "tmpl", "MemoryMoreEntries", func() []byte { return read("files/memory/prompts/more-entries.tmpl") }},
	{"files/memory/prompts/unresolved.tmpl", "tmpl", "MemoryUnresolvedEntries", func() []byte { return read("files/memory/prompts/unresolved.tmpl") }},
	{"files/memory/prompts/catalog-header.tmpl", "tmpl", "MemoryCatalogHeader", func() []byte { return read("files/memory/prompts/catalog-header.tmpl") }},
	{"files/memory/prompts/catalog-bank.tmpl", "tmpl", "MemoryCatalogBank", func() []byte { return read("files/memory/prompts/catalog-bank.tmpl") }},
	{"files/memory/prompts/home-index.txt", "txt", "MemoryHomeIndexHeading", func() []byte { return read("files/memory/prompts/home-index.txt") }},
	{"files/memory/prompts/knowledge.txt", "txt", "MemoryKnowledgeHeading", func() []byte { return read("files/memory/prompts/knowledge.txt") }},
	{"files/memory/prompts/home-overflow.tmpl", "tmpl", "MemoryHomeOverflow", func() []byte { return read("files/memory/prompts/home-overflow.tmpl") }},
	{"files/memory/folder-roles.json", "json", "MemoryFolderRoles", func() []byte { return read("files/memory/folder-roles.json") }},
	{"files/memory/prompts/layout.txt", "txt", "MemoryLayoutHeading", func() []byte { return read("files/memory/prompts/layout.txt") }},
	{"files/memory/prompts/layout-row.tmpl", "tmpl", "MemoryLayoutRow", func() []byte { return read("files/memory/prompts/layout-row.tmpl") }},
	{"files/memory/prompts/native.txt", "txt", "MemoryNativeHeading", func() []byte { return read("files/memory/prompts/native.txt") }},
	{"files/memory/prompts/native-folder.tmpl", "tmpl", "MemoryNativeFolder", func() []byte { return read("files/memory/prompts/native-folder.tmpl") }},
	{"files/memory/prompts/native-unparsed.tmpl", "tmpl", "MemoryNativeUnparsed", func() []byte { return read("files/memory/prompts/native-unparsed.tmpl") }},
	{"files/memory/prompts/recall-line.tmpl", "tmpl", "MemoryRecallLine", func() []byte { return read("files/memory/prompts/recall-line.tmpl") }},
	{"files/memory/prompts/recall-header.tmpl", "tmpl", "MemoryRecallHeader", func() []byte { return read("files/memory/prompts/recall-header.tmpl") }},
	{"files/memory/prompts/recall-footer.txt", "txt", "MemoryRecallFooter", func() []byte { return read("files/memory/prompts/recall-footer.txt") }},
	{"files/memory/prompts/recall-marker.txt", "txt", "MemoryRecallMarker", func() []byte { return read("files/memory/prompts/recall-marker.txt") }},
	{"files/memory/prompts/bank-load.tmpl", "tmpl", "MemoryBankLoadNote", func() []byte { return read("files/memory/prompts/bank-load.tmpl") }},
	{"files/memory/prompts/previous-session.tmpl", "tmpl", "MemoryPreviousSession", func() []byte { return read("files/memory/prompts/previous-session.tmpl") }},
	{"files/memory/prompts/update-recovery.tmpl", "tmpl", "MemoryUpdateRecovery", func() []byte { return read("files/memory/prompts/update-recovery.tmpl") }},
	{"files/memory/prompts/scaffold.tmpl", "tmpl", "MemoryScaffoldNote", func() []byte { return read("files/memory/prompts/scaffold.tmpl") }},
	{"files/memory/prompts/catalog-pointer.tmpl", "tmpl", "MemoryCatalogPointer", func() []byte { return read("files/memory/prompts/catalog-pointer.tmpl") }},
	{"files/memory/prompts/bank-heading.tmpl", "tmpl", "MemoryBankHeading", func() []byte { return read("files/memory/prompts/bank-heading.tmpl") }},
	{"files/memory/prompts/native-context.tmpl", "tmpl", "MemoryNativeContext", func() []byte { return read("files/memory/prompts/native-context.tmpl") }},
	{"files/memory/prompts/transcript-handoff.tmpl", "tmpl", "TranscriptHandoff", func() []byte { return read("files/memory/prompts/transcript-handoff.tmpl") }},
}

// readers maps each exported production accessor to the embedded paths its body reads.
func readers(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, f := range parseProduction(t) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			out[fn.Name.Name] = append(out[fn.Name.Name], readPaths(fn)...)
		}
	}
	return out
}

func readPaths(fn *ast.FuncDecl) []string {
	var paths []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 1 {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		lit, isLit := call.Args[0].(*ast.BasicLit)
		if ok && (id.Name == "read" || id.Name == "renderMemory") && isLit && lit.Kind == token.STRING {
			p, _ := strconv.Unquote(lit.Value)
			if id.Name == "renderMemory" {
				p = "files/memory/prompts/" + p
			}
			paths = append(paths, p)
		}
		return true
	})
	return paths
}

// parseProduction parses this package's non-test Go files.
func parseProduction(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestInventoryClosed(t *testing.T) {
	var embedded []string
	err := fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			embedded = append(embedded, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, e := range inventory {
		if listed[e.path] {
			t.Errorf("%s listed twice", e.path)
		}
		listed[e.path] = true
	}
	for _, p := range embedded {
		if !listed[p] {
			t.Errorf("embedded %s is not in the inventory", p)
		}
		delete(listed, p)
	}
	for _, p := range slices.Sorted(maps.Keys(listed)) {
		t.Errorf("inventory lists %s, which is not embedded", p)
	}
}

func TestInventoryOneReaderEach(t *testing.T) {
	byPath := map[string][]string{}
	for name, paths := range readers(t) {
		for _, p := range paths {
			byPath[p] = append(byPath[p], name)
		}
	}
	for _, e := range inventory {
		if got := byPath[e.path]; len(got) != 1 || got[0] != e.reader {
			t.Errorf("%s: readers %v, want exactly [%s]", e.path, got, e.reader)
		}
		want, err := files.ReadFile(e.path)
		if err != nil || !bytes.Equal(e.read(), want) {
			t.Errorf("%s: %s() does not return the embedded bytes (%v)", e.path, e.reader, err)
		}
		delete(byPath, e.path)
	}
	for _, p := range slices.Sorted(maps.Keys(byPath)) {
		t.Errorf("%v read %s, which is not in the inventory", byPath[p], p)
	}
}

func TestReadersReturnFreshCopies(t *testing.T) {
	for _, e := range inventory {
		b := e.read()
		want := bytes.Clone(b)
		clear(b)
		if !bytes.Equal(e.read(), want) {
			t.Errorf("%s: mutating one result changed the next", e.reader)
		}
	}
}

// TestAccessorsUsed requires every exported accessor to have a caller outside this package, tests included.
func TestAccessorsUsed(t *testing.T) {
	used := map[string]bool{}
	for _, root := range []string{"..", "../../cmd"} {
		walkUses(t, root, used)
	}
	for name := range readers(t) {
		if !used[name] {
			t.Errorf("assets.%s has no caller outside internal/assets", name)
		}
	}
}

func walkUses(t *testing.T, root string, used map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || p == filepath.Join("..", "assets")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			collectUses(t, p, used)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func collectUses(t *testing.T, path string, used map[string]bool) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src, []byte(importPath)) {
		return
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	local := importName(f)
	if local == "" {
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
				used[sel.Sel.Name] = true
			}
		}
		return true
	})
}

// importName is the identifier a file binds the assets import to, or "" when it does not import it by name.
func importName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != importPath {
			continue
		}
		if imp.Name == nil {
			return "assets"
		}
		if n := imp.Name.Name; n != "_" && n != "." {
			return n
		}
	}
	return ""
}
