package cli

import (
	"testing"
	"unicode/utf8"
)

// Every menu row fits an 80-column terminal, every group is present, and every alias resolves.
func TestMenuRowsFitAndAliasesResolve(t *testing.T) {
	want := map[string]bool{groupAnthropic: false, groupVendors: false, groupAuth: false, groupSession: false, groupDiagnostics: false, groupSettings: false}
	for _, c := range commands() {
		if n := utf8.RuneCountInString(c.summary); n > summaryMax {
			t.Errorf("%s: summary is %d chars (max %d): %q", c.name, n, summaryMax, c.summary)
		}
		if !c.hidden {
			if _, ok := want[c.group]; !ok {
				t.Errorf("%s: unknown group %q", c.name, c.group)
			}
			want[c.group] = true
		}
		for _, a := range c.aliases {
			if got := lookup(a); got == nil || got.name != c.name {
				t.Errorf("alias %s does not resolve to %s", a, c.name)
			}
		}
	}
	for g, seen := range want {
		if !seen {
			t.Errorf("group %s has no rows", g)
		}
	}
	for _, name := range []string{"token", "providers", "use", "settings"} {
		if lookup(name) == nil {
			t.Errorf("legacy name %s no longer dispatches", name)
		}
	}
}
