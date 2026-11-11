package menu

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// The routing page runRoutingMenu shows, with long vault-set values, lays out on an 80-column panel unclipped.
func TestRoutingRowFitsEightyColumns(t *testing.T) {
	worst := providers.Routing{
		Weights: map[string]float64{"price": 0.123456789, "headroom": 0.876543211},
		Opencode: providers.OpencodeRouting{
			Ladder:          &providers.RouteOpencodeSubIDs,
			WorkRepoMarkers: &[]string{"gitlab.example.com", "github.example.net"},
			FreeTiers:       &[]string{"sonnet", "haiku"},
			FreeFirstOutput: "90s",
		},
	}
	orig := pickMenu
	t.Cleanup(func() { pickMenu = orig })
	var rows [][2]string
	pickMenu = func(_ string, secs []tui.Section, trailing [2]string, _ int) (int, error) {
		for _, s := range secs {
			rows = append(rows, s.Rows...)
		}
		rows = append(rows, trailing)
		return -1, nil
	}
	runRoutingMenu(&dialog{out: discardWriter{}}, worst, worst)
	assertFitsEighty(t, "routing page", rows)
}
