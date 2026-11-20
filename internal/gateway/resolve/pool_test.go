package resolve

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/claude" // the vendor packages register the agents the table walks
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/opencode"
	_ "github.com/edotau/claude-code/internal/pi"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The pool table sorts agent, row, tier, omits a pair the row cannot run and fits 80 columns on a terminal.
func TestPoolTable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := PoolRows(context.Background(), reg, []string{"fast", "deep"}, false, "", time.Now())
	seen := map[string]PoolRow{}
	var keys []string
	for _, r := range rows {
		seen[r.Agent+" "+r.Row+" "+r.Tier] = r
		keys = append(keys, r.Agent+" "+r.Row+" "+map[string]string{"fast": "0", "deep": "1"}[r.Tier])
	}
	if !slices.IsSorted(keys) {
		t.Errorf("rows not sorted by agent, row, tier: %q", keys)
	}
	_, copilot := seen["copilot github·copilot fast"]
	if _, codex := seen["codex github·copilot fast"]; !copilot || codex {
		t.Errorf("copilot row %v, codex row %v on the copilot login; want only copilot: %v", copilot, codex,
			slices.Collect(maps.Keys(seen)))
	}
	if slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, "claude ") }) {
		t.Errorf("pool table has claude rows %q: claude no longer rotates rows", keys)
	}
	var out bytes.Buffer
	RenderPoolTable(&out, rows, 80, time.Now())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(rows)+1 {
		t.Errorf("%d lines for %d rows; want a header plus one per row", len(lines), len(rows))
	}
	for _, l := range lines {
		if n := textutil.DisplayWidth(l); n > 80 {
			t.Errorf("line is %d columns, want at most 80: %q", n, l)
		}
	}
}
