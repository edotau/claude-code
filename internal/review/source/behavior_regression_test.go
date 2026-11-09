package source

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFindFunctionsJSTSCapturesParameters(t *testing.T) {
	for _, language := range []string{"javascript", "typescript"} {
		t.Run(language, func(t *testing.T) {
			src := "function declared(a, b) {}\nconst arrow = (x, y, z) => {};\nlet empty = async () => {};"
			got := findFunctions(src, language)
			if len(got) != 3 {
				t.Fatalf("functions = %+v, want three", got)
			}
			for i, want := range []struct {
				name   string
				params int
			}{{"declared", 2}, {"arrow", 3}, {"empty", 0}} {
				if got[i].Name != want.name || got[i].Parameters != want.params {
					t.Errorf("function %d = %+v, want name %q and %d parameters", i, got[i], want.name, want.params)
				}
			}
		})
	}
}

func TestFindFunctionsFinalBodyBeyondTwoThousand(t *testing.T) {
	padding := strings.Repeat("    value = 1\n", 200)
	src := "def first():\n    pass\ndef last(flag):\n" + padding + "    if flag:\n        return 1\n"
	got := findFunctions(src, "python")
	want := []FunctionInfo{
		{Name: "first", Lines: 3, Complexity: 1},
		{Name: "last", Parameters: 1, Lines: 204, Complexity: 2},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("functions = %+v, want %+v (including the final branch after byte 2000)", got, want)
	}
}

func TestFindFunctionsGoGenericSignature(t *testing.T) {
	src := "package fixture\nfunc Identity[T any](value T, fallback T) T { return value }\n"
	got := findFunctions(src, "go")
	want := []FunctionInfo{{Name: "Identity", Parameters: 2, Lines: 1, Complexity: 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("functions = %+v, want %+v", got, want)
	}
}

func TestFindClassesPythonStopsAtModuleDefinition(t *testing.T) {
	src := "class Service:\n    def run(self):\n        pass\n\ndef standalone():\n    pass\nclass Other:\n    def stop(self):\n        pass\n"
	got := findClasses(src, "python")
	want := []ClassInfo{{Name: "Service", Methods: 1, Lines: 3}, {Name: "Other", Methods: 1, Lines: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("classes = %+v, want %+v (module function belongs to neither class)", got, want)
	}
}

func TestPyFuncNestingInlineSuites(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []pyLine
		want int
	}{
		{"top level", []pyLine{{indent: 4, text: "    for item in items: visit(item)"}}, 1},
		{"nested", []pyLine{{indent: 4, text: "    if ready:"}, {indent: 8, text: "        for item in items: visit(item)"}}, 2},
		{"siblings", []pyLine{{indent: 4, text: "    if ready: visit()"}, {indent: 4, text: "    while busy: wait()"}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pyFuncNesting(tc.body); got != tc.want {
				t.Fatalf("nesting = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestChangedNamesRejectsOptionBase(t *testing.T) {
	// Validation must happen before invoking git, even outside a repository.
	names, err := changedNames(t.TempDir(), "--output=unexpected")
	if err == nil || !strings.Contains(err.Error(), "invalid base") {
		t.Fatalf("names = %v, error = %v; want invalid base error before running git", names, err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %v, want none for an option-like base", names)
	}
}

func TestEmitQualityJSONReplacesWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "report.json")
	writeFx(t, dir, "report.json", "previous report")
	old, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var stdout bytes.Buffer
	if err := emitQualityJSON(map[string]int{"files": 1}, output, &stdout); err != nil {
		t.Fatal(err)
	}
	previous, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "previous report" {
		t.Errorf("open previous file = %q, want unchanged contents", previous)
	}
	current, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "{\n  \"files\": 1\n}" {
		t.Errorf("replacement = %q, want complete JSON report", current)
	}
	if stdout.String() != "Results written to "+output+"\n" {
		t.Errorf("stdout = %q, want success message naming output", stdout.String())
	}
}

func TestWalkExtensionsTracksTraversalFailures(t *testing.T) {
	dir := t.TempDir()
	writeFx(t, dir, "not-directory", "contents")
	root := filepath.Join(dir, "not-directory", "child")
	reports, failed := walkExtensions(root, []string{".py"})
	if len(reports) != 0 || !slices.Equal(failed, []string{root}) {
		t.Fatalf("reports = %+v, failed = %v; want no reports and traversal failure %q", reports, failed, root)
	}
	if _, err := analyzeDirectory(root, "python"); err == nil || !strings.Contains(err.Error(), "analysis failed") {
		t.Fatalf("analysis error = %v, want tracked traversal failure rather than no supported files", err)
	}
}
