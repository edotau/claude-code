package assets

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	"renderSkeleton": "files/workflows/skeletons/",
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
			if stem, _, block := strings.Cut(p, "/"); block && dir != "" {
				p = stem + ".tmpl"
			}
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

// withoutFieldsHeader drops a leading {{/* fields: ... */}} comment, which renders nothing.
func withoutFieldsHeader(src []byte) []byte {
	if !bytes.HasPrefix(src, []byte("{{/* fields:")) {
		return src
	}
	_, rest, _ := bytes.Cut(src, []byte("*/}}"))
	return rest
}

// readerBytesErr requires the exact embedded bytes; only a rendered workflow skeleton may lose its header.
func readerBytesErr(path, kind, reader string, read func() []byte) error {
	want, err := files.ReadFile(path)
	if err == nil && (inventoryBytesMatch(read, kind, reader, want) ||
		strings.HasSuffix(path, ".js.tmpl") && inventoryBytesMatch(read, kind, reader, withoutFieldsHeader(want))) {
		return nil
	}
	return fmt.Errorf("%s: %s() does not return the embedded bytes (%v)", path, reader, err)
}

func TestInventoryOneReaderEach(t *testing.T) {
	byPath := map[string][]string{}
	for name, paths := range readers(t) {
		for _, p := range paths {
			byPath[p] = append(byPath[p], name)
		}
	}
	for _, e := range inventory {
		got := slices.Clone(byPath[e.path])
		slices.Sort(got)
		if strings.Join(got, ",") != e.reader {
			t.Errorf("%s: readers %v, want [%s]", e.path, got, e.reader)
		}
		if bad := badReaderList(e.reader); bad != "" {
			t.Errorf("%s: reader list %q %s", e.path, e.reader, bad)
		}
		if err := readerBytesErr(e.path, e.kind, e.reader, e.read); err != nil {
			t.Error(err)
		}
		delete(byPath, e.path)
	}
	for _, p := range slices.Sorted(maps.Keys(byPath)) {
		t.Errorf("%v read %s, which is not in the inventory", byPath[p], p)
	}
}

// badReaderList names what is wrong with a comma-separated accessor list: blank or repeated names.
func badReaderList(list string) string {
	names := strings.Split(list, ",")
	for i, n := range names {
		if strings.TrimSpace(n) == "" {
			return "has a blank accessor name"
		}
		if slices.Contains(names[:i], n) {
			return "repeats " + n
		}
	}
	return ""
}

func TestInventoryReaderListsWellFormed(t *testing.T) {
	for list, want := range map[string]string{"": "blank", "A,": "blank", "A,,B": "blank", "A,A": "repeats A", "A,B": ""} {
		if got := badReaderList(list); !strings.Contains(got, want) || (want == "") != (got == "") {
			t.Errorf("badReaderList(%q) = %q, want %q", list, got, want)
		}
	}
	for _, row := range Inventory() {
		if slices.Contains(row.Accessors, "") || len(row.Accessors) == 0 {
			t.Errorf("%s: doctor accessors %q hold an empty element or none", row.Path, row.Accessors)
		}
	}
	saved := inventory
	defer func() { inventory = saved }()
	inventory = append(saved[:0:0], saved[0])
	inventory[0].reader = ""
	if got := Inventory()[0].Accessors; got == nil || len(got) != 0 {
		t.Errorf("empty reader column became %q, want []", got)
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
		{"ContractBlock", "plain", func() string {
			return ContractBlock(ContractFrame{Unit: "u1", Kind: "k", Terms: "a, b", Report: "rep", Tokens: 5, Leaves: 2, Depth: 1})
		}},
		{"ContractBlock", "fleet", func() string {
			return ContractBlock(ContractFrame{"u1", "k", "/p.md", "2.1", "lane1", "a, b", "rep", 5, 2, 1, true})
		}},
		{"AgentSchemaInstruction", "", func() string { return AgentSchemaInstruction(`{"a":1}`) }},
		{"PackPrompt", "", func() string { return PackPrompt("task", "inv", two) }},
		{"PackIndex", "", func() string { return PackIndex(two) }},
		{"PackCoverageWarning", "", func() string { return PackCoverageWarning(PackCoverage{3, 9, 12, 6, "big"}) }},
		{"InspectNoScope", "", func() string { return InspectNoScope(3) }},
		{"InspectAdvisory", "", func() string { return InspectAdvisory(2) }},
		{"InspectOutsideModified", "", func() string { return InspectOutsideModified("p.go", "d9") }},
		{"InspectNewDecls", "", func() string { return InspectNewDecls(2) }},
		{"ToolsDryRun", "", func() string { return ToolsDryRun("download and install x") }},
		{"LoopReportHeader", "", func() string { return LoopReportHeader(LoopHeader{"L1", "planner done", "5s", 2, 70}) }},
		{"LoopReportSummary", "", func() string { return LoopReportSummary("all good") }},
		{"LoopReportResume", "", func() string { return LoopReportResume("L1") }},
		{"PackInventory", "all", func() string { return PackInventory([]string{"- a\n", "- b\n"}, []string{"- c (x)\n"}, 4) }},
		{"PackInventory", "none", func() string { return PackInventory(nil, nil, 0) }},
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
		{"HookBusyPeer", "lanes", func() string {
			return HookBusyPeer(BusyPeer{Comm: "claude", Name: "claude-7", PID: 42, Branch: "main", Files: "/a, /b", Lanes: true})
		}},
		{"HookBusyPeer", "nolanes", func() string { return HookBusyPeer(BusyPeer{Comm: "claude", PID: 42, Branch: "main"}) }},
		{"HookNoticeLine", "", func() string { return HookNoticeLine("peer", "text") }},
		{"HookNoticeDrainWarn", "", func() string { return HookNoticeDrainWarn("e") }},
		{"HookFailureLine", "", func() string { return HookFailureLine("audit", "s: boom", "x") }},
		{"HookFailureClaim", "", func() string { return HookFailureClaim("abc") }},
		{"HookFailuresMore", "", func() string { return HookFailuresMore(2) }},
		{"HookFailuresWarn", "", func() string { return HookFailuresWarn("e") }},
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
	rendered, branches := map[string]bool{}, map[string]bool{}
	renderedKey = func(key string) { rendered[key] = true }
	renderedBranch = func(key string, n *parse.IfNode, then bool) { branches[branchID(key, n, then)] = true }
	var label string
	renderedFields = func(key string, dot any) { checkCallerFields(t, label, key, dot) }
	defer func() { renderedKey, renderedFields, renderedBranch = nil, nil, nil }()
	var got strings.Builder
	for _, c := range cases {
		label = c.acc
		if c.variant != "" {
			label += " (" + c.variant + ")"
		}
		got.WriteString("== " + label + " ==\n" + c.fn() + "\n")
	}
	missing := unrenderedKeys(rendered)
	if len(missing) != 0 {
		t.Fatalf("templates never rendered by TestRendersGolden: %v", missing)
	}
	for _, gap := range untakenBranches(branches) {
		t.Log("branch never taken by a golden case: " + gap)
	}
	const golden = "testdata/renders.golden"
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("RE-RECORDED %s: rerun without -update to check it", golden)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("rendered output differs from %s (first difference at byte %d)", golden, firstDiff(got.String(), string(want)))
	}
}

// templateSets is every loaded embedded template set.
func templateSets() []func() map[string]*parse.ListNode {
	return []func() map[string]*parse.ListNode{agentTemplates, hookTemplates, briefTemplates, memoryTemplates, safetyTemplates, rtkTemplates, skeletonTemplates}
}

// unrenderedKeys lists every loaded template key (all sets, block keys included) absent from rendered.
func unrenderedKeys(rendered map[string]bool) []string {
	var missing []string
	for _, load := range templateSets() {
		for key := range load() {
			if !rendered[key] {
				missing = append(missing, key)
			}
		}
	}
	slices.Sort(missing)
	return missing
}

// fieldsHeader is the one canonical header: single spaces, "-" for none, "." for the bare value.
var fieldsHeader = regexp.MustCompile(`^/\* fields: (-|\.|[A-Za-z_]\w*(?: [A-Za-z_]\w*)*) \*/$`)

// declaredFields reads a template's canonical header; found is false without one, bad says why one is malformed.
func declaredFields(root *parse.ListNode) (names []string, found bool, bad string) {
	if root == nil || len(root.Nodes) == 0 {
		return nil, false, ""
	}
	c, ok := root.Nodes[0].(*parse.CommentNode)
	if !ok || !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(c.Text, "/*")), "fields:") {
		return nil, false, ""
	}
	m := fieldsHeader.FindStringSubmatch(c.Text)
	if m == nil {
		return nil, true, "malformed fields header " + strconv.Quote(c.Text) + `, want "/* fields: A B */"`
	}
	if m[1] == "-" {
		return nil, true, ""
	}
	names = strings.Split(m[1], " ")
	for i, n := range names {
		if slices.Contains(names[:i], n) {
			return names, true, "declares " + n + " twice"
		}
	}
	return names, true, ""
}

// fieldWalk: top is the dot's fields (bare-dot blocks included); items maps a range scope to its item fields.
type fieldWalk struct {
	set      map[string]*parse.ListNode
	lenient  bool
	top      map[string]bool
	items    map[string]map[string]bool
	reached  map[string]bool
	problems []string
}

// walkFields is scope-aware; lenient (text/template briefs) allows with and variables, the executor fails them.
func walkFields(set map[string]*parse.ListNode, key string, lenient bool) *fieldWalk {
	w := &fieldWalk{set: set, lenient: lenient, top: map[string]bool{}, items: map[string]map[string]bool{},
		reached: map[string]bool{key: true}}
	w.visit([]string{key}, set[key], "")
	return w
}

func (w *fieldWalk) use(scope, name string) {
	if scope != "" && name == "." {
		return
	}
	if scope == "" {
		w.top[name] = true
		return
	}
	if w.items[scope] == nil {
		w.items[scope] = map[string]bool{}
	}
	w.items[scope][name] = true
}

func (w *fieldWalk) bad(path []string, msg string) {
	w.problems = append(w.problems, path[len(path)-1]+": "+msg)
}

// visit walks n for template path[len-1]; scope "" is the top dot, else the range or with scope whose dot is an item.
func (w *fieldWalk) visit(path []string, n parse.Node, scope string) {
	switch n := n.(type) {
	case *parse.FieldNode:
		w.use(scope, strings.Join(n.Ident, "."))
	case *parse.DotNode:
		w.use(scope, ".")
	case *parse.VariableNode:
		w.variable(path, n)
	case *parse.PipeNode:
		w.pipe(path, n, scope)
	case *parse.WithNode:
		if !w.lenient {
			w.bad(path, "uses {{with "+n.Pipe.String()+"}}, which the executor does not support")
		}
		w.scoped(path, n.Pipe, n.List, n.ElseList, "~with "+n.Pipe.String(), scope)
	case *parse.RangeNode:
		w.scoped(path, n.Pipe, n.List, n.ElseList, rangeScope(n, scope), scope)
	case *parse.TemplateNode:
		w.call(path, n, scope)
	default:
		w.children(path, n, scope)
	}
}

func (w *fieldWalk) pipe(path []string, n *parse.PipeNode, scope string) {
	if len(n.Decl) != 0 && !w.lenient {
		w.bad(path, "declares a variable in "+n.String()+", which the executor does not support")
	}
	w.children(path, n, scope)
}

// scoped visits a range or with: the pipe and else list in the outer scope, the body in the item scope.
func (w *fieldWalk) scoped(path []string, pipe *parse.PipeNode, body, other *parse.ListNode, inner, outer string) {
	w.visit(path, pipe, outer)
	w.visit(path, body, inner)
	w.visit(path, other, outer)
}

func (w *fieldWalk) children(path []string, n parse.Node, scope string) {
	for _, c := range templateChildren(n) {
		w.visit(path, c, scope)
	}
}

// variable counts $.X as a top field; the parse-only executor fails every variable.
func (w *fieldWalk) variable(path []string, n *parse.VariableNode) {
	if !w.lenient {
		w.bad(path, "uses variable "+n.String()+", which the executor does not support")
	}
	switch {
	case n.Ident[0] != "$":
	case len(n.Ident) == 1:
		w.top["."] = true
	default:
		w.top[strings.Join(n.Ident[1:], ".")] = true
	}
}

func rangeScope(n *parse.RangeNode, scope string) string {
	if c := n.Pipe.Cmds; scope == "" && len(n.Pipe.Decl) == 0 && len(c) == 1 && len(c[0].Args) == 1 {
		switch a := c[0].Args[0].(type) {
		case *parse.FieldNode:
			return strings.Join(a.Ident, ".")
		case *parse.DotNode:
			return "."
		}
	}
	return "~range " + n.Pipe.String()
}

// call checks a {{template}} target exists and is not already on the path; a bare dot hands the scope to the block.
func (w *fieldWalk) call(path []string, n *parse.TemplateNode, scope string) {
	target := calleeKey(w.set, path[len(path)-1], n.Name)
	if _, ok := w.set[target]; !ok {
		w.bad(path, "calls unknown template "+strconv.Quote(n.Name))
		return
	}
	if slices.Contains(path, target) {
		w.bad(path, "recurses through "+strings.Join(append(slices.Clone(path), target), " -> "))
		return
	}
	if _, bare := passesDot(n); !bare {
		if n.Pipe != nil {
			w.visit(path, n.Pipe, scope)
		}
		return
	}
	w.reached[target] = true
	w.visit(append(slices.Clone(path), target), w.set[target], scope)
}

// passesDot reports a {{template "x" .}} call and the block name it targets.
func passesDot(n *parse.TemplateNode) (string, bool) {
	if n.Pipe == nil || len(n.Pipe.Cmds) != 1 || len(n.Pipe.Cmds[0].Args) != 1 {
		return n.Name, false
	}
	_, isDot := n.Pipe.Cmds[0].Args[0].(*parse.DotNode)
	return n.Name, isDot
}

// calleeKey resolves a block name the way callTemplate does.
func calleeKey(set map[string]*parse.ListNode, from, name string) string {
	if _, ok := set[templateStem(from)+"/"+name]; ok {
		return templateStem(from) + "/" + name
	}
	return name
}

// declarationProblems lists how key's header, body fields and nothing else disagree.
func declarationProblems(set map[string]*parse.ListNode, key string, lenient bool) []string {
	declared, ok, bad := declaredFields(set[key])
	if !ok {
		return []string{key + ": no {{/* fields: ... */}} header as the first node"}
	}
	if bad != "" {
		return []string{key + ": " + bad}
	}
	w := walkFields(set, key, lenient)
	out := slices.Clone(w.problems)
	for name := range w.top {
		if !slices.Contains(declared, name) {
			out = append(out, key+": uses "+name+" but does not declare it")
		}
	}
	for _, name := range declared {
		if !w.top[name] {
			out = append(out, key+": declares "+name+" but never uses it")
		}
	}
	if !slices.IsSorted(declared) {
		out = append(out, key+": declared fields are not sorted")
	}
	slices.Sort(out)
	return out
}

func TestTemplateFieldsDeclared(t *testing.T) {
	for _, load := range templateSets() {
		set := load()
		for key := range set {
			for _, p := range declarationProblems(set, key, false) {
				t.Error(p)
			}
		}
	}
}

// TestReviewBriefFieldsDeclared holds the text/template review briefs to the same header and to their input struct.
func TestReviewBriefFieldsDeclared(t *testing.T) {
	for path, input := range map[string]any{
		"files/review/bench/briefs/fix.tmpl":    BenchFixInput{},
		"files/review/bench/briefs/review.tmpl": BenchReviewInput{},
		"files/review/autofix/briefs/fix.tmpl":  AutofixInput{},
	} {
		trees := map[string]*parse.Tree{}
		root := parse.New("brief")
		root.Mode = parse.ParseComments
		if _, err := root.Parse(string(read(path)), "", "", trees, templateFuncs); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		set := map[string]*parse.ListNode{path: trees["brief"].Root}
		for _, p := range declarationProblems(set, path, true) {
			t.Error(p)
		}
		declared, _, _ := declaredFields(set[path])
		for _, name := range declared {
			if _, ok := reflect.TypeOf(input).FieldByName(name); !ok {
				t.Errorf("%s: declares %s, which %T lacks", path, name, input)
			}
		}
		for _, p := range itemFieldProblems(path, reflect.TypeOf(input), walkFields(set, path, true).items) {
			t.Error(p)
		}
	}
}

// itemFieldProblems checks each named range scope's item fields against the element type of that input field.
func itemFieldProblems(path string, input reflect.Type, items map[string]map[string]bool) []string {
	var out []string
	for scope, names := range items {
		if strings.HasPrefix(scope, "~") {
			continue
		}
		f, ok := input.FieldByName(scope)
		if !ok || f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.Struct {
			out = append(out, path+": ranges over "+scope+", which is not a slice of structs in "+input.String())
			continue
		}
		for name := range names {
			if _, ok := f.Type.Elem().FieldByName(name); !ok {
				out = append(out, path+": item field "+name+" is not in "+f.Type.Elem().String())
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestTemplateFieldsNegativeControl(t *testing.T) {
	const blk = `{{define "x"}}{{/* fields: - */}}`
	for _, c := range []struct{ name, src, want string }{
		{"omitted", `{{/* fields: A */}}{{.A}}{{.B}}`, "neg.tmpl: uses B but does not declare it"},
		{"unused", `{{/* fields: A B */}}{{.A}}`, "neg.tmpl: declares B but never uses it"},
		{"headerless", `{{.A}}`, "neg.tmpl: no {{/* fields"},
		{"unsorted", `{{/* fields: B A */}}{{.A}}{{.B}}`, "neg.tmpl: declared fields are not sorted"},
		{"through-block", `{{/* fields: - */}}{{template "x" .}}{{define "x"}}{{/* fields: A */}}{{.A}}{{end}}`, "neg.tmpl: uses A but does not declare it"},
		{"root-variable", `{{/* fields: - */}}{{if $.A}}x{{end}}`, "neg.tmpl: uses variable $.A"},
		{"root-variable-field", `{{/* fields: - */}}{{if $.A}}x{{end}}`, "neg.tmpl: uses A but does not declare it"},
		{"with", `{{/* fields: A */}}{{with .A}}x{{end}}`, "neg.tmpl: uses {{with .A}}"},
		{"variable-decl", `{{/* fields: A */}}{{$x := .A}}`, "neg.tmpl: declares a variable"},
		{"unknown-block", `{{/* fields: - */}}{{template "missing" .}}`, `neg.tmpl: calls unknown template "missing"`},
		{"self-recursion", `{{/* fields: - */}}{{template "x" .}}` + blk + `{{template "x" .}}{{end}}`, "neg/x: recurses through neg/x -> neg/x"},
		{"mutual-recursion", `{{define "a"}}{{/* fields: - */}}{{template "b" .}}{{end}}{{define "b"}}{{/* fields: - */}}{{template "a" .}}{{end}}`, "neg/b: recurses through neg/a -> neg/b -> neg/a"},
		{"no-space", `{{/*fields: A*/}}{{.A}}`, "neg.tmpl: malformed fields header"},
		{"double-space", `{{/* fields: A  B */}}{{.A}}{{.B}}`, "neg.tmpl: malformed fields header"},
		{"tab", "{{/* fields: A\tB */}}{{.A}}{{.B}}", "neg.tmpl: malformed fields header"},
		{"empty-declaration", `{{/* fields: */}}x`, "neg.tmpl: malformed fields header"},
		{"dash-with-names", `{{/* fields: - A */}}{{.A}}`, "neg.tmpl: malformed fields header"},
		{"duplicate", `{{/* fields: A A */}}{{.A}}`, "neg.tmpl: declares A twice"},
	} {
		set := parseTemplates("neg.tmpl", []byte(c.src))
		var got []string
		for key := range set {
			got = append(got, declarationProblems(set, key, false)...)
		}
		if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, c.want) }) {
			t.Errorf("%s: problems %q lack %q", c.name, got, c.want)
		}
	}
}

func TestFieldWalkScopes(t *testing.T) {
	w := walkFields(parseTemplates("s.tmpl", []byte(`{{/* fields: - */}}{{range .L}}{{.X}}{{$.T}}{{end}}`)), "s.tmpl", true)
	if !w.top["L"] || !w.top["T"] || w.top["X"] || !w.items["L"]["X"] {
		t.Errorf("top %v items %v: want L,T top and X an item field of L", w.top, w.items)
	}
	type row struct{ A string }
	type in struct{ L []row }
	got := itemFieldProblems("p", reflect.TypeFor[in](), map[string]map[string]bool{"L": {"A": true, "Zed": true}})
	if len(got) != 1 || !strings.Contains(got[0], "item field Zed") {
		t.Errorf("item problems = %q, want one for Zed", got)
	}
}

func TestCallerFieldsBothDirections(t *testing.T) {
	set := parseTemplates("c.tmpl", []byte(`{{/* fields: A B C */}}{{if .A}}{{.B}}{{else}}{{.C}}{{end}}`))
	for name, c := range map[string]struct {
		fields tmplFields
		want   string
	}{
		"exact":   {tmplFields{"A": 1, "B": 1, "C": 1}, ""},
		"extra":   {tmplFields{"A": 1, "B": 1, "C": 1, "D": 1}, "passes D to c.tmpl"},
		"missing": {tmplFields{"A": 1, "B": 1}, "omits C for c.tmpl"},
	} {
		got := strings.Join(callerFieldProblems(set, "L", "c.tmpl", c.fields), "\n")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: problems %q, want %q", name, got, c.want)
		}
	}
	blocks := parseTemplates("b.tmpl", []byte(`{{/* fields: A */}}{{.A}}{{template "x" .}}{{define "x"}}{{/* fields: A Z */}}{{.A}}{{.Z}}{{end}}`))
	if got := callerFieldProblems(blocks, "L", "b.tmpl", tmplFields{"A": 1}); len(got) != 1 || !strings.Contains(got[0], "omits Z") {
		t.Errorf("block fields: problems %q, want one omitting Z", got)
	}
}

func TestTemplateReservedDefineName(t *testing.T) {
	for _, src := range []string{`{{define "\x00file"}}owned{{end}}`, `x{{define "\x00file"}}owned{{end}}`} {
		mustPanic(t, "reserved", func() { parseTemplates("r.tmpl", []byte(src)) })
	}
}

// callerFieldProblems lists keys a tmplFields caller passes that key does not declare, and declared ones it omits.
func callerFieldProblems(set map[string]*parse.ListNode, label, key string, dot any) []string {
	fields, ok := dot.(tmplFields)
	if _, found, _ := declaredFields(set[key]); !ok || !found {
		return nil
	}
	want := map[string]bool{}
	for k := range walkFields(set, key, false).reached {
		names, _, _ := declaredFields(set[k])
		for _, n := range names {
			want[n] = true
		}
	}
	var out []string
	for name := range fields {
		if !want[name] {
			out = append(out, label+" passes "+name+" to "+key+", which does not declare it")
		}
	}
	for name := range want {
		if _, ok := fields[name]; !ok {
			out = append(out, label+" omits "+name+" for "+key+", which declares it")
		}
	}
	slices.Sort(out)
	return out
}

func checkCallerFields(t *testing.T, label, key string, dot any) {
	for _, load := range templateSets() {
		set := load()
		if _, ok := set[key]; !ok {
			continue
		}
		for _, p := range callerFieldProblems(set, label, key, dot) {
			t.Error(p)
		}
	}
}

func branchID(key string, n *parse.IfNode, then bool) string {
	return fmt.Sprintf("%s@%d/%t", key, n.Pos, then)
}

// untakenBranches lists each {{if}} then or else branch (absent else counts) no golden ran; ranges untracked.
func untakenBranches(taken map[string]bool) []string {
	var gaps []string
	for _, load := range templateSets() {
		for key, root := range load() {
			walkIfs(root, func(n *parse.IfNode) {
				for _, then := range []bool{true, false} {
					if id := branchID(key, n, then); !taken[id] {
						gaps = append(gaps, id)
					}
				}
			})
		}
	}
	slices.Sort(gaps)
	return gaps
}

func walkIfs(n parse.Node, visit func(*parse.IfNode)) {
	if i, ok := n.(*parse.IfNode); ok {
		visit(i)
	}
	for _, c := range templateChildren(n) {
		walkIfs(c, visit)
	}
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
	maps.Copy(set, parseTemplates("bad.tmpl", []byte(`{{template "nope" .}}`)))
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), `no template nope`) {
			t.Errorf("unknown block did not panic as expected: %v", r)
		}
	}()
	renderFrom(set, "bad.tmpl", nil)
}

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), want) {
			t.Errorf("panic = %v, want it to contain %q", r, want)
		}
	}()
	fn()
}

func TestTemplateRecursionLimit(t *testing.T) {
	set := parseTemplates("loop.tmpl", []byte(`{{define "self"}}{{template "self" .}}{{end}}{{define "a"}}{{template "b" .}}{{end}}{{define "b"}}{{template "a" .}}{{end}}`))
	mustPanic(t, "nests deeper than the limit", func() { renderFrom(set, "loop/self", nil) })
	mustPanic(t, "nests deeper than the limit", func() { renderFrom(set, "loop/a", nil) })
}

func TestTemplateDefineShadowsFile(t *testing.T) {
	for _, src := range []string{`{{define "lane.tmpl"}}x{{end}}`, `{{define  "lane.tmpl"}}x{{end}}`,
		`{{- define "lane.tmpl" -}}x{{end}}`, "{{define `lane.tmpl`}}x{{end}}", `a{{define "lane.tmpl"}}x{{end}}`} {
		mustPanic(t, "may not reuse its file name", func() { parseTemplates("lane.tmpl", []byte(src)) })
	}
	for _, src := range []string{`prose: define "lane.tmpl" here`, `{{/* define "lane.tmpl" */}}ok`} {
		if got := parseTemplates("lane.tmpl", []byte(src)); got["lane.tmpl"] == nil {
			t.Errorf("%q: false shadow panic or lost file root: %v", src, got)
		}
	}
}

func TestStandaloneTemplateBlocks(t *testing.T) {
	set := parseTemplates("solo", []byte(`{{define "hi"}}<{{.}}>{{end}}a{{template "hi" .}}`))
	if got := renderFrom(set, "solo", "x"); got != "a<x>" {
		t.Errorf("solo = %q", got)
	}
}

func TestMemoryMissingTemplateNamesArea(t *testing.T) {
	mustPanic(t, "no memory template nope", func() { renderMemory("nope", nil) })
}

func firstDiff(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}
