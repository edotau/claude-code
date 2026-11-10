package source

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCalcCyclomaticElseIfAddsOneElseAddsNone(t *testing.T) {
	if got := calcCyclomaticComplexity("if a {} else if b {} else {}"); got != 3 {
		t.Fatalf("cyclomatic = %d, want 3 (base + if + else if)", got)
	}
}

func TestCommentedCodePatternSkipsProse(t *testing.T) {
	re := commentedCodePattern()
	for _, line := range []string{"// for each file we walk the tree", "# if the cache is cold we rebuild"} {
		if re.MatchString(line) {
			t.Errorf("prose %q flagged as commented code", line)
		}
	}
	for _, line := range []string{"// if x > 0 {", "# def run(self):"} {
		if !re.MatchString(line) {
			t.Errorf("code %q not flagged", line)
		}
	}
}

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

func TestQualityGoGenericGodClass(t *testing.T) {
	limit := qualityThresholds["god_class_methods"]
	var src strings.Builder
	src.WriteString("package fixture\ntype Set[K comparable, V ~[]E, E any] struct{}\ntype Big struct{}\n")
	for i := range limit + 1 {
		fmt.Fprintf(&src, "func (s *Set[K, V, E]) M%d() {}\nfunc (b *Big) M%d() {}\n", i, i)
	}
	report, err := QualityContent("fixture.go", src.String(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var want []Smell
	for _, name := range []string{"Set", "Big"} {
		want = append(want, Smell{Type: "god_class", Severity: "high", Location: name,
			Message: fmt.Sprintf("Class '%s' has %d methods (max: %d)", name, limit+1, limit)})
	}
	if !slices.Equal(report.Smells, want) {
		t.Fatalf("smells = %+v, want generic Set and plain Big flagged: %+v", report.Smells, want)
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

func TestQualityTypeScriptNestedGenericDeclaration(t *testing.T) {
	src := "function f<T extends Record<string, unknown>>(x: T) { return x; }"
	report, err := QualityContent("fixture.ts", src, "typescript")
	if err != nil {
		t.Fatal(err)
	}
	want := []FunctionInfo{{Name: "f", Parameters: 1, Lines: 1, Complexity: 1}}
	if !slices.Equal(report.FunctionDetails, want) {
		t.Fatalf("functions = %+v, want nested generic declaration %+v", report.FunctionDetails, want)
	}
}

func TestQualityPythonNestedClassesOwnTheirMethods(t *testing.T) {
	var src strings.Builder
	src.WriteString("class Outer:\n    def before(self):\n        pass\n")
	for _, name := range []string{"First", "Second"} {
		fmt.Fprintf(&src, "    class %s:\n", name)
		for i := range 11 {
			fmt.Fprintf(&src, "        def method%d(self):\n            pass\n", i)
		}
	}
	src.WriteString("    def after(self):\n        pass\n")
	report, err := QualityContent("fixture.py", src.String(), "python")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ClassDetails) != 3 {
		t.Fatalf("classes = %+v, want Outer and two nested classes", report.ClassDetails)
	}
	for i, want := range []struct {
		name    string
		methods int
	}{{"Outer", 2}, {"First", 11}, {"Second", 11}} {
		got := report.ClassDetails[i]
		if got.Name != want.name || got.Methods != want.methods {
			t.Errorf("class %d = %+v, want %s with %d methods", i, got, want.name, want.methods)
		}
	}
	if len(report.Smells) != 0 {
		t.Fatalf("smells = %+v, want no false god_class finding", report.Smells)
	}
}

// Header shape (multiline, quoted or commented parens, a dedented comment) never changes Service's 21-method count.
func TestQualityPythonGodClassAcrossHeaderShapes(t *testing.T) {
	const standalone = "\ndef standalone():\n    pass\n"
	for _, tc := range []struct{ name, header, tail string }{
		{"multiline header", "class Service(\n    Base,\n):", standalone},
		{"double-quoted paren", `class Service(make_base(")")):`, ""},
		{"single-quoted paren", `class Service(make_base(')')):`, ""},
		{"triple-quoted paren", `class Service(make_base("""(""")):`, ""},
		{"escaped quote before paren", `class Service(make_base("\")")):`, ""},
		{"commented paren", "class Service(\n    Base, # )\n):", ""},
		{"dedented comment", "class Service:\n\n# Section heading", standalone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var src strings.Builder
			src.WriteString(tc.header + "\n")
			for i := range 21 {
				fmt.Fprintf(&src, "    def method%d(self):\n        pass\n", i)
			}
			src.WriteString(tc.tail)
			report, err := QualityContent("fixture.py", src.String(), "python")
			if err != nil {
				t.Fatal(err)
			}
			if len(report.ClassDetails) != 1 || report.ClassDetails[0].Methods != 21 {
				t.Fatalf("classes = %+v, want Service with exactly 21 methods", report.ClassDetails)
			}
			want := []Smell{{
				Type: "god_class", Severity: "high", Location: "Service",
				Message: "Class 'Service' has 21 methods (max: 20)",
			}}
			if !slices.Equal(report.Smells, want) {
				t.Fatalf("smells = %+v, want %+v", report.Smells, want)
			}
		})
	}
}
