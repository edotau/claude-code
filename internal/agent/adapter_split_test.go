package agent

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
	if parts := SplitDiff([]byte(d), 100, 10_000); len(parts) != 1 || string(parts[0].Diff) != d {
		t.Fatalf("small diff must stay whole, got %d parts", len(parts))
	}
}

func TestSplitDiffGroupsByDirAndFitsBudget(t *testing.T) {
	d := fileDiff("internal/a/x.go", 1, 40) + fileDiff("internal/a/y.go", 1, 40) +
		fileDiff("internal/b/z.go", 1, 40) + fileDiff("cmd/tool/m.go", 1, 40) + fileDiff("README.md", 1, 5)
	budget := 100 + partOverhead + 7000
	parts := SplitDiff([]byte(d), 100, budget)
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
	parts := SplitDiff([]byte(d), 100, 100+partOverhead+3000)
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
	parts := []Part{{Files: []string{"a"}}, {Files: []string{"b"}}}
	out := MergeReports(false, parts, []string{"SHIP IT\n[LOW] a:1 — x — y", "BLOCK\n[LOW] a:1 — x — y\n[HIGH] b:2 — z — w"})
	if !strings.HasPrefix(out, "BLOCK\n") || strings.Count(out, "a:1") != 1 {
		t.Fatalf("want BLOCK first and one a:1 line:\n%s", out)
	}
	adv := MergeReports(true, parts, []string{cleanVerdict, cleanVerdict})
	if !strings.HasPrefix(adv, cleanVerdict+"\n") {
		t.Fatalf("clean adversarial parts keep the clean verdict:\n%s", adv)
	}
}
