package ask

import (
	"fmt"
	"strings"
	"testing"
)

func fileDiff(path string, hunks, hunkLines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	for h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,1 +%d,1 @@\n", h*10, h*10)
		for range hunkLines {
			b.WriteByte('+')
			b.WriteString(strings.Repeat("x", 78))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestSplitDiffSmallIsOnePart(t *testing.T) {
	d := fileDiff("internal/a/a.go", 1, 2)
	if parts := splitDiff([]byte(d), 100, 10_000); len(parts) != 1 || string(parts[0].Diff) != d {
		t.Fatalf("small diff must stay whole, got %d parts", len(parts))
	}
}

func TestSplitDiffGroupsByDirAndFitsBudget(t *testing.T) {
	d := fileDiff("internal/a/x.go", 1, 40) + fileDiff("internal/a/y.go", 1, 40) +
		fileDiff("internal/b/z.go", 1, 40) + fileDiff("cmd/tool/m.go", 1, 40) + fileDiff("README.md", 1, 5)
	budget := 100 + partOverhead + 7000
	parts := splitDiff([]byte(d), 100, budget)
	if len(parts) < 3 {
		t.Fatalf("want >=3 parts, got %d", len(parts))
	}
	var seen []string
	for i, p := range parts {
		if len(p.Diff)+100+partOverhead > budget {
			t.Errorf("part %d is %d chars, over budget", i, len(p.Diff))
		}
		t.Log(p.Header(i+1, len(parts)))
		seen = append(seen, p.Files...)
	}
	if got := strings.Join(seen, ","); !strings.Contains(got, "internal/a/x.go,internal/a/y.go") || len(seen) != 5 {
		t.Errorf("internal/a files must share a part and none be lost: %v", seen)
	}
}

func TestSplitDiffOversizedFileSplitsOnHunks(t *testing.T) {
	d := fileDiff("internal/a/big.go", 20, 10)
	parts := splitDiff([]byte(d), 100, 100+partOverhead+3000)
	if len(parts) < 3 {
		t.Fatalf("want a big file cut into >=3 parts, got %d", len(parts))
	}
	for _, p := range parts {
		if !strings.HasPrefix(string(p.Diff), "diff --git a/internal/a/big.go") || !strings.Contains(string(p.Diff), "\n@@ ") {
			t.Errorf("each piece repeats the file header and starts on a hunk: %.60q", p.Diff)
		}
	}
}

func TestMergeReportsWorstVerdictAndDedup(t *testing.T) {
	parts := []part{{Files: []string{"a"}}, {Files: []string{"b"}}}
	out := mergeReports(false, parts, []string{"SHIP IT\n[LOW] a:1 — x — y", "BLOCK\n[LOW] a:1 — x — y\n[HIGH] b:2 — z — w"})
	if !strings.HasPrefix(out, "BLOCK\n") || strings.Count(out, "a:1") != 1 {
		t.Fatalf("want BLOCK first and one a:1 line:\n%s", out)
	}
	adv := mergeReports(true, parts, []string{cleanVerdict, cleanVerdict})
	if !strings.HasPrefix(adv, cleanVerdict+"\n") {
		t.Fatalf("clean adversarial parts keep the clean verdict:\n%s", adv)
	}
}

func TestMergeReportsFailedFirstAdversarialPart(t *testing.T) {
	parts := []part{{Files: []string{"a.go"}}, {Files: []string{"b.go"}}}
	got := mergeReports(true, parts, nil)
	if got != "VERDICT: no review completed\n" {
		t.Fatalf("failed first part must not produce a clean review verdict: %q", got)
	}
}

func TestMergeReportsBlankAdversarialAnswerIsNotClean(t *testing.T) {
	for _, answer := range []string{"", " \t\n", cleanVerdict} {
		t.Run(fmt.Sprintf("answer=%q", answer), func(t *testing.T) {
			parts := []part{{Files: []string{"a.go"}}}
			got := mergeReports(true, parts, []string{answer})
			want := "VERDICT: no review completed\n"
			marker := "(no review returned)\n"
			if answer == cleanVerdict {
				want = cleanVerdict + "\n"
				marker = "(no findings)\n"
			}
			if !strings.HasPrefix(got, want) {
				t.Fatalf("answer %q: verdict must be %q, got %q", answer, want, got)
			}
			if !strings.Contains(got, marker) {
				t.Fatalf("answer %q: missing review-content marker %q in %q", answer, marker, got)
			}
		})
	}
}

func TestMergeReportsUnjudgedPartIsNotShipIt(t *testing.T) {
	parts := []part{{Files: []string{"a"}}, {Files: []string{"b"}}}
	out := mergeReports(false, parts, []string{"SHIP IT\n[LOW] a:1 — x — y", ""})
	if !strings.HasPrefix(out, "NEEDS WORK\n") {
		t.Fatalf("an unjudged part must not merge to SHIP IT:\n%s", out)
	}
	out = mergeReports(false, parts, []string{"BLOCK\n[HIGH] a:1 — x — y", "no verdict here"})
	if !strings.HasPrefix(out, "BLOCK\n") {
		t.Fatalf("a worse verdict from another part still wins:\n%s", out)
	}
}

func TestMergeReportsPartialAnswersNumberAgainstFullPartCount(t *testing.T) {
	parts := []part{{Files: []string{"a"}}, {Files: []string{"b"}}, {Files: []string{"c"}}}
	got := mergeReports(true, parts, []string{"SHIP IT\n[LOW] a:1 — x — y", cleanVerdict})
	for _, want := range []string{"## part 1/3", "## part 2/3", "findings across 3 parts"} {
		if !strings.Contains(got, want) {
			t.Errorf("partial report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/2") {
		t.Errorf("partial report numbers against answer count:\n%s", got)
	}
}
