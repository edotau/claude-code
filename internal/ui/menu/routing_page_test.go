package menu

import (
	"bufio"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/configure"
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

func TestEditRoutingTextReasksOnInvalid(t *testing.T) {
	var f configure.RoutingField
	for _, m := range configure.Modules() {
		for _, c := range m.Fields() {
			if c.Kind == configure.RoutingNumber {
				f = c
			}
		}
	}
	var out strings.Builder
	d := &dialog{out: &out, br: bufio.NewReader(strings.NewReader("abc\n0.5\n"))}
	got, ok := editRoutingText(d, f)
	if !ok || got != "0.5" {
		t.Fatalf("editRoutingText = %q, %v; want 0.5, true (output %q)", got, ok, out.String())
	}
}
