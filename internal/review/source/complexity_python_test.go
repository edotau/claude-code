package source

import (
	"strings"
	"testing"
)

func TestPythonPrematureAbstractionIgnoresCommentsAndStrings(t *testing.T) {
	th := ComplexityThresholds{}
	src := "class A:\n    \"\"\"HTTP Protocol helper, ABC\"\"\"\n    # Protocol\n    pass\n"
	for _, f := range analyzePython(src, strings.Count(src, "\n"), th) {
		if f.Rule == "premature-abstraction" {
			t.Fatalf("unexpected finding: %+v", f)
		}
	}
	real := "from typing import Protocol\nclass A(Protocol):\n    pass\n"
	found := false
	for _, f := range analyzePython(real, 3, th) {
		found = found || f.Rule == "premature-abstraction"
	}
	if !found {
		t.Fatal("real Protocol base not flagged")
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
