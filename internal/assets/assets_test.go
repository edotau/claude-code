package assets

import (
	"bytes"
	"flag"
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
	"text/template/parse"

	_ "github.com/edotau/claude-code/internal/nonet"
)

const importPath = "github.com/edotau/claude-code/internal/assets"

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

// readerDirs maps a template renderer to the directory its first argument is relative to; read takes full paths.
var readerDirs = map[string]string{
	"read": "", "renderMemory": "files/memory/prompts/", "renderAgent": "files/agent/", "renderHook": "files/hooks/", "renderBriefText": "files/brief/", "renderSafety": "files/safety/", "renderRtk": "files/rtk/",
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
		if !ok || !isLit || lit.Kind != token.STRING {
			return true
		}
		if dir, known := readerDirs[id.Name]; known {
			p, _ := strconv.Unquote(lit.Value)
			paths = append(paths, dir+p)
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
		if err != nil || !inventoryBytesMatch(e.read, e.kind, e.reader, want) {
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
		if e.read == nil {
			continue
		}
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

var updateGolden = flag.Bool("update", false, "rewrite testdata/renders.golden from the current renderers")

type renderCase struct {
	acc, variant string
	fn           func() string
}

const (
	two         = "line one\nline two"
	digestLimit = 100
)

// agentRenderCases and its siblings call every accessor that renders a template, with fixed sample inputs.
func agentRenderCases() []renderCase {
	return []renderCase{
		{"AgentPreamble", "", func() string { return AgentPreamble(two) }},
		{"AgentRubricBody", "", func() string { return AgentRubricBody("std", "task") }},
		{"AgentPersona", "", func() string { return AgentPersona(PersonaFrame{"nm", "desc", two}) }},
		{"AgentMidRun", "edit", func() string { return AgentMidRun("who", "part", "rec", two, true) }},
		{"AgentMidRun", "read", func() string { return AgentMidRun("who", "part", "rec", two, false) }},
		{"AgentPartial", "", func() string { return AgentPartial(two) }},
		{"AgentReconcile", "", func() string { return AgentReconcile(two) }},
		{"AgentHandoff", "", func() string { return AgentHandoff("codex", two) }},
		{"AgentRecovery", "files", func() string { return AgentRecovery(RecoveryFrame{"p", "id1", "st", "cut", "a.go", two, true}) }},
		{"AgentRecovery", "nofiles", func() string { return AgentRecovery(RecoveryFrame{"p", "id1", "st", "cut", "", two, false}) }},
		{"AgentSchemaInstruction", "", func() string { return AgentSchemaInstruction(`{"a":1}`) }},
		{"PackPrompt", "", func() string { return PackPrompt("task", "inv", two) }},
		{"PackIndex", "", func() string { return PackIndex(two) }},
		{"PackCoverageWarning", "", func() string { return PackCoverageWarning(PackCoverage{3, 9, 12, 6, "big"}) }},
	}
}

func hookRenderCases() []renderCase {
	return []renderCase{
		{"HookSimplicityHeader", "", func() string { return HookSimplicityHeader(4, "a.go") }},
		{"HookSimplicityMore", "", func() string { return HookSimplicityMore(7) }},
		{"HookPathFailed", "", func() string { return HookPathFailed("/p", "boom") }},
		{"HookPathMapped", "", func() string { return HookPathMapped(`C:\x`, "/mnt/x", "d") }},
		{"HookPathDetail", "", func() string { return HookPathDetail("/p", "d") }},
		{"HookPathsContext", "", func() string { return HookPathsContext(two, "img") }},
		{"HookBusyPeer", "lanes", func() string { return HookBusyPeer("claude", 42, "main", true) }},
		{"HookBusyPeer", "nolanes", func() string { return HookBusyPeer("claude", 42, "main", false) }},
		{"HookNoticeLine", "", func() string { return HookNoticeLine("peer", "text") }},
		{"HookNoticeDrainWarn", "", func() string { return HookNoticeDrainWarn("e") }},
		{"HookNoticeDivergeWarn", "", func() string { return HookNoticeDivergeWarn("e") }},
		{"HookDiverged", "rewritten", func() string { return HookDiverged("main", "origin/main", 2, 3, true) }},
		{"HookDiverged", "plain", func() string { return HookDiverged("main", "origin/main", 2, 3, false) }},
		{"HookClaimHowTo", "", HookClaimHowTo},
		{"HookClaimStageHint", "", HookClaimStageHint},
		{"ClaudeAttachImage", "", func() string { return ClaudeAttachImage("/i.png") }},
		{"AttachFile", "", func() string { return AttachFile(`"/f"`, two) }},
		{"AttachRef", "", func() string { return AttachRef("/f") }},
	}
}

func planHookRenderCases() []renderCase {
	return []renderCase{
		{"HookPlanPathChoose", "", func() string { return HookPlanPathChoose("err") }},
		{"HookPlanPathRedirect", "", func() string { return HookPlanPathRedirect("/a", `"/b"`) }},
		{"HookPlanPathMoved", "", func() string { return HookPlanPathMoved("/a", "/b") }},
		{"HookPlanTemplateBlock", "repo", func() string { return HookPlanTemplateBlock("n", "Goal", "", "H1, H2") }},
		{"HookPlanTemplateBlock", "user", func() string { return HookPlanTemplateBlock("n", "Goal", "/t.md", "H1, H2") }},
		{"HookPlanAgentWrite", "", func() string { return HookPlanAgentWrite("/plans") }},
		{"HookDocsPlacement", "", func() string { return HookDocsPlacement(DocsPlacement{"b", "w", "d", "h", "r", "ho"}) }},
		{"HookDocsWhere", "docs", func() string { return HookDocsWhere("/d", true) }},
		{"HookDocsWhere", "nodocs", func() string { return HookDocsWhere("/d", false) }},
		{"HookDocsRetired", "", func() string { return HookDocsRetired("/d") }},
		{"HookMemoryNotePath", "", func() string { return HookMemoryNotePath("/a", "/b") }},
		{"HookMemoryStoreName", "", func() string { return HookMemoryStoreName("/a", "/b") }},
		{"HookLaneQueueLine", "", func() string { return HookLaneQueueLine("l", "b", "r", "d", "n") }},
		{"HookLaneQueueMore", "", func() string { return HookLaneQueueMore(5) }},
		{"HookLaneLanded", "removed", func() string { return HookLaneLanded("l", "d", true) }},
		{"HookLaneLanded", "kept", func() string { return HookLaneLanded("l", "d", false) }},
	}
}

func briefRenderCases() []renderCase {
	return []renderCase{
		{"BriefBody", "pi", func() string { return BriefBody(BriefFrame{"pi", "own", "ver", "rep", "task", "/r", true}) }},
		{"BriefBody", "claude", func() string { return BriefBody(BriefFrame{"claude", "", "ver", "rep", "task", "/r", false}) }},
		{"LoopPlanner", "", func() string { return LoopPlanner("goal", 2, 5, 3, 9, digestLimit, two) }},
		{"ReviewAdversarial", "", func() string { return ReviewAdversarial("clean") }},
		{"TakeoverBrief", "", func() string { return TakeoverBrief("codex", "cut", "task", 12, two) }},
		{"ExplorePrompt", "web", func() string { return ExplorePrompt("q", true) }},
		{"ExplorePrompt", "noweb", func() string { return ExplorePrompt("q", false) }},
		{"SafetyMessage", "pattern", func() string { return SafetyMessage("BLOCKED", "what", "pat", "cmd", "hint") }},
		{"SafetyMessage", "nopattern", func() string { return SafetyMessage("ASK", "what", "", "cmd", "hint") }},
		{"SafetyNoAsk", "", func() string { return SafetyNoAsk("codex") }},
		{"SafetyArgMissing", "", func() string { return SafetyArgMissing("Bash", "command") }},
		{"RtkNote", "", func() string { return RtkNote("git") }},
		{"RtkShadowNone", "", func() string { return RtkShadowNone("rtk") }},
		{"RtkShadowFound", "", func() string { return RtkShadowFound("/usr/bin/rtk") }},
		{"WorkflowPipelineSkeleton", "", func() string { return WorkflowPipelineSkeleton("it's") }},
		{"WorkflowParallelSkeleton", "", func() string { return WorkflowParallelSkeleton("it's") }},
		{"WorkflowEvaluatorSkeleton", "", func() string { return WorkflowEvaluatorSkeleton("it's") }},
		{"WorkflowOrchestratorSkeleton", "", func() string { return WorkflowOrchestratorSkeleton("it's") }},
		{"BenchFixBrief", "rtk", func() string { return benchFix(false) }},
		{"BenchFixBrief", "bare", func() string { return benchFix(true) }},
		{"BenchReviewBrief", "rtk", func() string { return benchReview(false) }},
		{"BenchReviewBrief", "bare", func() string { return benchReview(true) }},
		{"AutofixBrief", "policy", func() string { return autofix(true) }},
		{"AutofixBrief", "plain", func() string { return autofix(false) }},
	}
}

func benchFix(code bool) string {
	s, err := BenchFixBrief(BenchFixInput{"own", "ver", "BenchX", "pkg", "/w", "cause", "1ns", "/b", "go test", "id", code})
	return mustRender(s, err)
}

func benchReview(code bool) string {
	s, err := BenchReviewBrief(BenchReviewInput{"own", "ver", "BenchX", "pkg", "/w", "1ns", "/b", "/a", "diff", "id", code})
	return mustRender(s, err)
}

func autofix(policy bool) string {
	in := AutofixInput{Lane: "l", Sha: "abc", Live: "/live", Gate: "g", Owned: "o", Fence: "```", Policy: policy,
		Judgment: []AutofixFinding{{"c", "a.go", "r", "m", 3}}}
	s, err := AutofixBrief(in)
	return mustRender(s, err)
}

func mustRender(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

func memoryRenderCases() []renderCase {
	return []renderCase{
		{"MemoryUpdateWorker", "", func() string { return MemoryUpdateWorker("/bank") }},
		{"MemoryDreamWorker", "", func() string { return MemoryDreamWorker("/bank") }},
		{"MemoryLegacyUpdateWorker", "", func() string { return MemoryLegacyUpdateWorker("/bank", "/home") }},
		{"MemoryLegacyDreamWorker", "", func() string { return MemoryLegacyDreamWorker("/bank", "/home") }},
		{"MemoryUpdatePrompt", "", func() string { return MemoryUpdatePrompt(MemoryUpdateInput{"w", "proc", "hist", "dig", "git", "tags"}) }},
		{"MemoryTranscriptPointer", "", func() string { return MemoryTranscriptPointer("/t") }},
		{"MemorySessionDigest", "", func() string { return MemorySessionDigest("/t", two) }},
		{"MemoryGitActivity", "", func() string { return MemoryGitActivity("c1", "f1") }},
		{"MemorySessionTags", "", func() string { return MemorySessionTags([]string{"a", "b"}) }},
		{"MemoryDreamPrompt", "", func() string { return MemoryDreamPrompt("w", "chg", "proc") }},
		{"MemoryOpenTasks", "", func() string { return MemoryOpenTasks(two) }},
		{"MemoryRTKMisses", "", func() string { return MemoryRTKMisses(two) }},
		{"MemoryBankHead", "", func() string { return MemoryBankHead("fold", "router") }},
		{"MemoryMoreEntries", "", func() string { return MemoryMoreEntries(4, "router") }},
		{"MemoryUnresolvedEntries", "", func() string { return MemoryUnresolvedEntries(6) }},
		{"MemoryCatalogHeader", "", func() string { return MemoryCatalogHeader("2026-11-25") }},
		{"MemoryCatalogBank", "", func() string { return MemoryCatalogBank("hook", "2026-11-25") }},
		{"MemoryHomeOverflow", "", func() string { return MemoryHomeOverflow(8) }},
		{"MemoryLayoutRow", "", func() string { return MemoryLayoutRow("fold", "role", 3) }},
		{"MemoryNativeFolder", "", func() string { return MemoryNativeFolder("fold") }},
		{"MemoryNativeUnparsed", "", func() string { return MemoryNativeUnparsed(2) }},
		{"MemoryRecallLine", "label", func() string { return MemoryRecallLine("f.md", "txt", true) }},
		{"MemoryRecallLine", "plain", func() string { return MemoryRecallLine("f.md", "txt", false) }},
		{"MemoryRecallHeader", "expand", func() string { return MemoryRecallHeader(true) }},
		{"MemoryRecallHeader", "plain", func() string { return MemoryRecallHeader(false) }},
		{"MemoryBankLoadNote", "", func() string { return MemoryBankLoadNote("pre") }},
		{"MemoryPreviousSession", "", func() string { return MemoryPreviousSession("sid") }},
		{"MemoryUpdateRecovery", "", func() string { return MemoryUpdateRecovery("id", "st", two) }},
		{"MemoryScaffoldNote", "", func() string { return MemoryScaffoldNote("/d") }},
		{"MemoryCatalogPointer", "", func() string { return MemoryCatalogPointer("/c") }},
		{"MemoryBankHeading", "", func() string { return MemoryBankHeading("/root") }},
		{"MemoryNativeContext", "", func() string { return MemoryNativeContext(two) }},
		{"TranscriptHandoff", "", func() string { return TranscriptHandoff("/cwd", "/t", "note") }},
	}
}

func TestRendersGolden(t *testing.T) {
	var cases []renderCase
	for _, set := range [][]renderCase{agentRenderCases(), hookRenderCases(), planHookRenderCases(), briefRenderCases(), memoryRenderCases()} {
		cases = append(cases, set...)
	}
	called := map[string]bool{}
	var got strings.Builder
	for _, c := range cases {
		label := c.acc
		if c.variant != "" {
			label += " (" + c.variant + ")"
		}
		called[c.acc] = true
		got.WriteString("== " + label + " ==\n" + c.fn() + "\n")
	}
	if missing := unrenderedTemplates(t, called); len(missing) != 0 {
		t.Fatalf("templates never rendered by TestRendersGolden: %v", missing)
	}
	const golden = "testdata/renders.golden"
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("rendered output differs from %s (first difference at byte %d)", golden, firstDiff(got.String(), string(want)))
	}
}

// unrenderedTemplates lists embedded .tmpl files that no called accessor reads.
func unrenderedTemplates(t *testing.T, called map[string]bool) []string {
	t.Helper()
	covered := map[string]bool{}
	for acc, paths := range readers(t) {
		if !called[acc] {
			continue
		}
		for _, p := range paths {
			covered[p] = true
		}
	}
	var missing []string
	err := fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".tmpl") && !covered[p] {
			missing = append(missing, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return missing
}

func TestTemplateBlocks(t *testing.T) {
	set := map[string]*parse.ListNode{}
	maps.Copy(set, parseTemplates("lane.tmpl", []byte(`{{define "head"}}[{{.Who}}]{{end}}{{define "body"}}{{template "head" .}} {{template "lane/head" .}}{{end}}`)))
	maps.Copy(set, parseTemplates("plain.tmpl", []byte(`hi {{.}}`)))
	if _, ok := set["lane.tmpl"]; ok {
		t.Error("a file of only defines registered a file-level key")
	}
	if got := renderFrom(set, "lane/body", tmplFields{"Who": "pi"}); got != "[pi] [pi]" {
		t.Errorf("lane/body = %q", got)
	}
	if got := renderFrom(set, "plain.tmpl", "x"); got != "hi x" {
		t.Errorf("plain.tmpl = %q", got)
	}
	set["bad.tmpl"] = parseTemplate("bad.tmpl", []byte(`{{template "nope" .}}`))
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), `no template nope`) {
			t.Errorf("unknown block did not panic as expected: %v", r)
		}
	}()
	renderFrom(set, "bad.tmpl", nil)
}

func firstDiff(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}
