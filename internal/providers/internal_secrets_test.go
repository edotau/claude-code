package providers

import (
	"testing"
)

// The <PROFILE>_M2M_ACCESS cache slot is a route secret, so it is filtered from child env like <PROFILE>_M2M.
func TestRouteSecretCoversM2MAccessSlot(t *testing.T) {
	r := &Registry{}
	if !r.RouteSecret("DATABRICKS_DEV_M2M_ACCESS") {
		t.Fatal("DATABRICKS_DEV_M2M_ACCESS not treated as a route secret")
	}
	if r.RouteSecret("DATABRICKS_DEV_ACCESS") {
		t.Fatal("unrelated _ACCESS name treated as a route secret")
	}
}
