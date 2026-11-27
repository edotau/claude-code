package databricks

import (
	"fmt"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// check runs the smoke reply; exit 0 only if the child exits 0 and its last line is OK.
func check(self string, p launchPlan) int {
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	out, code := capture(self, p, assets.DatabricksSmokePrompt(), p.Rest...)
	if code != 0 || textutil.LastLine(out) != "OK" {
		fmt.Printf("FAIL smoke: %s\n", tail(out, 5))
		return 1
	}
	fmt.Println("PASS smoke")
	return 0
}
