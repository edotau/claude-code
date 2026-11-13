package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/review/source"
)

// branchy returns a Go function whose cyclomatic complexity is n+1.
func branchy(name string, n int) string {
	return "func " + name + "(x int) int {\n\tr := 0\n" + strings.Repeat("\tif x > 1 {\n\t\tr++\n\t}\n", n) + "\treturn r\n}\n"
}

// TestComplexityGateOnlyFailsIntroducedFindings runs the real detector on two versions of one file.
func TestComplexityGateOnlyFailsIntroducedFindings(t *testing.T) {
	const head = "package pkg\n\n"
	base := head + branchy("Old", 12)
	(&fakeGates{}).install(t)
	gateSimplify = source.RunSimplify
	gateBlobs = func(context.Context, string, string, []string) (map[string]string, error) {
		return map[string]string{"pkg/p.go": base}, nil
	}
	dir, files := gateRepo(t)
	for name, tc := range map[string]struct{ body, want, notWant string }{
		"edited elsewhere": {base + "\nfunc Small() int { return 1 }\n", "", ""},
		"new function":     {base + "\n" + branchy("Fresh", 12), "Fresh", "Old"},
		"got worse":        {head + branchy("Old", 16), "Old", ""},
	} {
		writeRepoFile(t, dir, "pkg/p.go", tc.body)
		out, _, err := complexityGate(context.Background(), newGateInput(dir, "HEAD", files))
		if (tc.want == "") != (err == nil) || !strings.Contains(out, tc.want) || (tc.notWant != "" && strings.Contains(out, tc.notWant)) {
			t.Errorf("%s: err %v, output %q; want findings naming %q and not %q", name, err, out, tc.want, tc.notWant)
		}
	}
	untracked := []DiffFile{{Path: "pkg/p.go", Status: "?"}}
	writeRepoFile(t, dir, "pkg/p.go", base)
	if out, _, err := complexityGate(context.Background(), newGateInput(dir, "HEAD", untracked)); err == nil || !strings.Contains(out, "Old") {
		t.Errorf("a new file must report all its findings: err %v, output %q", err, out)
	}
}

func TestGitBlobsReadsBaseInOneCall(t *testing.T) {
	dir := newDiffRepo(t)
	got, err := gitBlobs(context.Background(), dir, "HEAD", []string{"pkg/p.go", "gone.go", "other/o.txt"})
	if err != nil || len(got) != 2 || got["pkg/p.go"] != "package pkg\n\nfunc P() int { return 1 }\n" || got["other/o.txt"] != "one\n" {
		t.Errorf("gitBlobs = %q, %v; want the two tracked files and no entry for gone.go", got, err)
	}
}
