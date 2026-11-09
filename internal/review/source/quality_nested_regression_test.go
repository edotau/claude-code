package source

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

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

func TestQualityPythonHeaderIgnoresQuotedAndCommentParentheses(t *testing.T) {
	for _, header := range []string{
		`class Service(make_base(")")):`,
		`class Service(make_base(')')):`,
		`class Service(make_base("""(""")):`,
		`class Service(make_base("\")")):`,
		"class Service(\n    Base, # )\n):",
	} {
		t.Run(header, func(t *testing.T) {
			var src strings.Builder
			src.WriteString(header + "\n")
			for i := range 21 {
				fmt.Fprintf(&src, "    def method%d(self):\n        pass\n", i)
			}
			report, err := QualityContent("fixture.py", src.String(), "python")
			if err != nil {
				t.Fatal(err)
			}
			if len(report.ClassDetails) != 1 || report.ClassDetails[0].Methods != 21 {
				t.Fatalf("classes = %+v, want Service with 21 methods", report.ClassDetails)
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
