package assets

import (
	"bytes"
	"strings"
	"testing"
)

func inventoryBytesMatch(reader func() []byte, kind, name string, want []byte) bool {
	if reader == nil {
		return kind == "template" && templateFixture(name)
	}
	return bytes.Equal(reader(), want)
}

func templateFixture(name string) bool {
	fix := BenchFixInput{Workdir: "/tmp/owner's lane", Package: "./internal/shell", OwnedLine: `["x"]`,
		Benchmark: "Bench", Cause: "cause", Numbers: "numbers", BeforeFile: "before", BenchCommand: "bench",
		IdentityCommand: "identity", Verify: "verify"}
	review := BenchReviewInput{Workdir: fix.Workdir, Package: fix.Package, OwnedLine: fix.OwnedLine,
		Benchmark: fix.Benchmark, Numbers: fix.Numbers, BeforeFile: fix.BeforeFile, AfterFile: "after",
		IdentityCommand: fix.IdentityCommand, Verify: fix.Verify, Diff: "- old\n+ new"}
	input := AutofixInput{Sha: "abc", Lane: "lane", Live: "live", Gate: "gate", Owned: `["internal/a.go"]`,
		Fence: "AUTOFIX-DATA-00000000000000000000000000000000", Judgment: []AutofixFinding{
			{Class: "complexity", Path: "internal/a.go", Line: 3, Rule: "rule", Message: "hostile text"}}}
	var err error
	switch name {
	case "BenchFixBrief":
		_, err = BenchFixBrief(fix)
	case "BenchReviewBrief":
		_, err = BenchReviewBrief(review)
	case "AutofixBrief":
		_, err = AutofixBrief(input)
	default:
		return false
	}
	return err == nil
}

func TestBenchBriefsCodeMode(t *testing.T) {
	fix, review := BenchFixInput{Workdir: "/w", Package: "./p"}, BenchReviewInput{Workdir: "/w", Package: "./p"}
	for _, code := range []bool{false, true} {
		fix.CodeMode, review.CodeMode = code, code
		f, err := BenchFixBrief(fix)
		r, err2 := BenchReviewBrief(review)
		if err != nil || err2 != nil {
			t.Fatal(err, err2)
		}
		for _, out := range []string{f, r} {
			if got := strings.Contains(out, "rtk"); got == code {
				t.Errorf("CodeMode=%v: rtk present=%v", code, got)
			}
			if !strings.Contains(out, "git diff --no-ext-diff") {
				t.Errorf("CodeMode=%v: diff line missing", code)
			}
		}
	}
}
