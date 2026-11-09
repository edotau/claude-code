package source

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestQualityPythonDedentedCommentPreservesGodClass(t *testing.T) {
	var src strings.Builder
	src.WriteString("class Service:\n\n# Section heading\n")
	for i := range 21 {
		fmt.Fprintf(&src, "    def method%d(self):\n        pass\n", i)
	}
	src.WriteString("\ndef standalone():\n    pass\n")
	report, err := QualityContent("service.py", src.String(), "python")
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
		t.Fatalf("smells = %+v, want %+v after a dedented comment", report.Smells, want)
	}
}
