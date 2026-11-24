package databricks

import (
	"fmt"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/textutil"
)

// check runs the smoke reply; exit 0 only if the child exits 0 and its last line is OK.
func check(self string, p launchPlan) int {
	rc := 0
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	if out, code := capture(self, p, assets.DatabricksSmokePrompt(), p.Rest...); code == 0 && textutil.LastLine(out) == "OK" {
		fmt.Println("PASS smoke")
	} else {
		fmt.Printf("FAIL smoke: %s\n", tail(out, 5))
		rc = 1
	}
	return rc
}
