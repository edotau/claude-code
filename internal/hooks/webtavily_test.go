package hooks

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestWebTavilySkipsWithoutCallingTavily(t *testing.T) {
	search := `{"tool_name":"WebSearch","tool_input":{"query":"q"},"tool_response":{"query":"q","results":[]}}`
	for _, tc := range []struct{ name, key, payload, verdict string }{
		{"no key is a no-op", "", search, ""},
		{"databricks search is the proxy's", "qwzxmplrtv", search, webSkipProxy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tavilyKeyName, tc.key)
			prev := sessionOnDatabricks
			sessionOnDatabricks = func() bool { return true }
			t.Cleanup(func() { sessionOnDatabricks = prev })
			r := WebTavily(core.ParseInputFor(strings.NewReader(tc.payload), "claude"))
			if r.UpdatedToolOutput != nil || r.Verdict != tc.verdict {
				t.Errorf("got verdict %q, output %v; want %q and none", r.Verdict, r.UpdatedToolOutput, tc.verdict)
			}
		})
	}
}
