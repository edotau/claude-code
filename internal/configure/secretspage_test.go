package configure

import (
	"os"
	"testing"
)

// TestConfirmPageKeysSavesOrDiscardsOnlyThePagesKeys: y stores the page's staged keys (Submit has none left),
// N restores env; a key staged before the page opened is untouched either way.
func TestConfirmPageKeysSavesOrDiscardsOnlyThePagesKeys(t *testing.T) {
	for _, yes := range []bool{true, false} {
		menuTestEnv(t)
		t.Setenv("OLD_KEY", "")
		os.Unsetenv("PAGE_KEY")
		d := &dialog{out: discardWriter{}, pending: map[string]stagedKey{}}
		d.stage("OLD_KEY", "old", true)
		before := map[string]stagedKey{"OLD_KEY": d.pending["OLD_KEY"]}
		d.stage("PAGE_KEY", "page-value", true)
		orig := actionConfirmed
		actionConfirmed = func(string) bool { return yes }
		confirmPageKeys(d, before)
		actionConfirmed = orig
		if _, still := d.pending["PAGE_KEY"]; still {
			t.Errorf("yes=%v: PAGE_KEY still pending after the page closed", yes)
		}
		if _, ok := d.pending["OLD_KEY"]; !ok {
			t.Errorf("yes=%v: OLD_KEY (staged before the page) must stay for Submit", yes)
		}
		if got := os.Getenv("PAGE_KEY"); (got == "page-value") != yes {
			t.Errorf("yes=%v: PAGE_KEY env = %q", yes, got)
		}
		d.pending = nil
		os.Unsetenv("PAGE_KEY")
	}
}
