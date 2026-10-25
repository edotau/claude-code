package cli

import (
	"slices"
	"strings"
	"testing"
)

// AUTH, SESSION, DIAGNOSTICS and SETTINGS list verbs A→Z; ANTHROPIC keeps its tier order.
func TestMenuGroupsAlphabetical(t *testing.T) {
	byGroup := map[string][]string{}
	group := ""
	for _, r := range menuRows() {
		if r.head != "" {
			group = r.head
		} else if r.name != "" {
			byGroup[group] = append(byGroup[group], strings.TrimSpace(r.name))
		}
	}
	for g := range alphabetical {
		if !slices.IsSorted(byGroup[g]) {
			t.Errorf("%s not alphabetical: %v", g, byGroup[g])
		}
	}
	if slices.IsSorted(byGroup[groupAnthropic]) {
		t.Errorf("ANTHROPIC must keep tier order, got sorted %v", byGroup[groupAnthropic])
	}
}
