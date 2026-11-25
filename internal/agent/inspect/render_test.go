package inspect

import (
	"bytes"
	"strings"
	"testing"
)

// Given each prose-bearing render path, When it runs, Then the bytes match the pinned text.
func TestRenderProsePinned(t *testing.T) {
	var noScope, adv, out, decls bytes.Buffer
	renderDiff(DiffOptions{Out: &noScope}, DiffReport{noScope: true, Files: []DiffFile{{Path: "a.go"}, {Path: "b.go"}}}, false)
	list := []string{"x.go", "y.go"}
	renderVerify(&adv, &VerifyReport{ID: "d1", PreDirty: &list, Proxy: ProxyMatch{Match: "none"}})
	renderOutsidePath(&out, DiffReport{PreDirtyOutside: []string{"p.go"}, dispatch: "d9"}, "p.go")
	renderNewDecls(&decls, []string{"func A", "func B"})
	var advisory string
	for _, line := range strings.SplitAfter(adv.String(), "\n") {
		if strings.Contains(line, "ADVISORY") {
			advisory = line
		}
	}
	for _, c := range []struct{ name, got, want string }{
		{"noScope", noScope.String(), "scope not checked: the brief names no owned files and no --path was given; 2 files changed" +
			" (details: --format json or --path)\nagents diff: 0 files +0 -0 (prod +0 -0, test +0 -0)\n"},
		{"advisory", advisory, "  ADVISORY: 2 file(s) had uncommitted changes before the run and differ now;" +
			" if they were not yours to edit, check them:\n"},
		{"outside", out.String(), "  p.go: was already modified before dispatch d9 started and is outside --path\n"},
		{"decls", decls.String(), "new tests and helpers (2, approve or cut):\n  func A\n  func B\n"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
