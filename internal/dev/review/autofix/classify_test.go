package autofix

import (
	"slices"
	"testing"
)

// Gate sections copied verbatim from state/harness/verify/out-*/*-diff.txt captures.
const (
	gofmtOut = `M +1 -0        pkg/c.go
FAIL gofmt (0.0s)
pkg/c.go: gofmt: needs formatting
ok build (0.0s)
ok vet (0.1s)
FAIL golangci-lint (0.2s)
state/harness/verify/tree-079662488ee5/pkg/c.go:3: golangci-lint/gofmt: File is not properly formatted
ok complexity (0.0s)
ok test (0.2s)
agents diff: 2 files +2 -0 (prod +2 -0, test +0 -0); --path ok; gates 4 ok, 2 FAIL
`
	complexityOut = `new tests and helpers (4, approve or cut):
  internal/review/bench/loop_test.go:193: new test TestQueueKindFilter
FAIL complexity (0.0s)
internal/review/bench/bench.go: complexity/high_complexity: flagConflict medium Function 'flagConflict' has complexity 11 (max: 10)
internal/review/bench/bench.go:175: complexity/cyclomatic-complexity: warn 'flagConflict' has cyclomatic complexity 11 (max 10). Flatten branching.
agents diff: 5 files +95 -1 (prod +46 -1, test +49 -0); --path ok; gates 5 ok, 1 FAIL
`
	docsOut = `FAIL docs (0.0s)
agents/Plan.md:46: mdsmith/MDS001: line too long (129 > 120)
agents diff: 24 files +101 -239 (prod +71 -215, test +30 -24); --path ok; gates 5 ok, 2 FAIL
`
	lintOut = `FAIL golangci-lint (2.4s)
internal/agent/ask/carry_test.go:95: golangci-lint/ineffassign: ineffectual assignment to closeLog
FAIL complexity (0.1s)
`
	testOut = `FAIL test (5.2s)
test: --- FAIL: TestRed (0.00s)
example.com/vt/pkg: test: FAIL
pkg_test.go:14: test: red
agents diff: 1 files +3 -0 (prod +3 -0, test +0 -0); --path ok; gates 3 ok, 3 FAIL
`
	// Messages from schemas_test.go:266 and workflowdrift.go:113 as the test gate prints them.
	regenOut = `FAIL test (1.6s)
schemas_test.go:290: test: internal-review-bench.txt:4: row bench.go:writeOut is duplicate or unsorted
test: workflows/loop.js: retry block differs from embedded source (run claude-code workflow sync)
`
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Class
	}{
		{"gofmt gate and lint row", gofmtOut, []Class{ClassGofmt, ClassGofmt}},
		{"complexity", complexityOut, []Class{ClassComplexity, ClassComplexity}},
		{"markdown", docsOut, []Class{ClassMarkdownLong}},
		{"ineffassign", lintOut, []Class{ClassIneffassign}},
		{"failing test", testOut, []Class{ClassFailingTest}},
		{"regeneration", regenOut, []Class{ClassWriterRows, ClassWorkflowBlock}},
		{"clean", "agents diff: 1 files +1 -0; --path ok; gates 6 ok\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []Class
			for _, f := range Classify(tt.in) {
				got = append(got, f.Class)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("classes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyFields(t *testing.T) {
	got := Classify(lintOut)
	want := Finding{ClassIneffassign, "golangci-lint", "internal/agent/ask/carry_test.go", 95, "ineffassign",
		"ineffectual assignment to closeLog"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Classify = %+v, want %+v", got, want)
	}
	if !ClassGofmt.Deterministic() || ClassComplexity.Deterministic() {
		t.Error("Deterministic split wrong")
	}
}
