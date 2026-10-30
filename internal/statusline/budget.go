package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// budgetCacheMaxAge bounds how stale the gateway-budget cache (written by `claude-code usage`) may be before it is hidden.
const budgetCacheMaxAge = 12 * time.Hour

// budgetSegment is "bud:N%" for the fullest budget window the session profile's cache holds; "" when absent or stale.
// It reads the cache file only: the status line never calls the network.
func budgetSegment(session string) string {
	raw, err := os.ReadFile(filepath.Join(paths.StateDir(), "usage", "gateway-budget.json"))
	if err != nil {
		return ""
	}
	var c struct {
		FetchedAt time.Time `json:"fetched_at"`
		Profile   string    `json:"profile"`
		Rows      []struct {
			UsedPct float64 `json:"used_percent"`
		} `json:"rows"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Profile != session || len(c.Rows) == 0 || time.Since(c.FetchedAt) > budgetCacheMaxAge {
		return ""
	}
	worst := c.Rows[0].UsedPct
	for _, r := range c.Rows[1:] {
		worst = max(worst, r.UsedPct)
	}
	return fmt.Sprintf("bud:%d%%", int(worst+0.5))
}
