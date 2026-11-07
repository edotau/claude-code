package roles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

func TestSplitFallsBackOnError(t *testing.T) {
	orig := splitRun
	splitRun = func(*agent.Client, context.Context, agent.RunOptions, agent.Sink) (agent.Result, error) {
		return agent.Result{Answer: "a\nb"}, errors.New("down")
	}
	t.Cleanup(func() { splitRun = orig })
	if got := Split(context.Background(), nil, "q"); len(got) != 1 || got[0] != "q" {
		t.Errorf("got %v", got)
	}
}

func TestSplitCapsAtTwelve(t *testing.T) {
	orig := splitRun
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf(" part %d ", i))
	}
	splitRun = func(*agent.Client, context.Context, agent.RunOptions, agent.Sink) (agent.Result, error) {
		return agent.Result{Answer: strings.Join(lines, "\n\n")}, nil
	}
	t.Cleanup(func() { splitRun = orig })
	got := Split(context.Background(), nil, "q")
	if len(got) != 12 || got[0] != "part 0" || got[11] != "part 11" {
		t.Errorf("len=%d first/last=%q/%q", len(got), got[0], got[len(got)-1])
	}
}

func TestSplitSingleLineReturnsQuestion(t *testing.T) {
	orig := splitRun
	splitRun = func(*agent.Client, context.Context, agent.RunOptions, agent.Sink) (agent.Result, error) {
		return agent.Result{Answer: "only one"}, nil
	}
	t.Cleanup(func() { splitRun = orig })
	if got := Split(context.Background(), nil, "q"); len(got) != 1 || got[0] != "q" {
		t.Errorf("got %v", got)
	}
}

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

func TestMergeReportsUnjudgedPartIsNotShipIt(t *testing.T) {
	parts := []Part{{Files: []string{"a"}}, {Files: []string{"b"}}}
	out := MergeReports(false, parts, []string{"SHIP IT\n[LOW] a:1 — x — y", ""})
	if !strings.HasPrefix(out, "NEEDS WORK\n") {
		t.Fatalf("an unjudged part must not merge to SHIP IT:\n%s", out)
	}
	out = MergeReports(false, parts, []string{"BLOCK\n[HIGH] a:1 — x — y", "no verdict here"})
	if !strings.HasPrefix(out, "BLOCK\n") {
		t.Fatalf("a worse verdict from another part still wins:\n%s", out)
	}
}
