package opencode

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestOpencodeRouteDetailHonorsPin: with an own login present, a Databricks pin names the pin, not own-login.
func TestOpencodeRouteDetailHonorsPin(t *testing.T) {
	fakeOpencode(t, "1.19.3", "", nil)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(OpencodeAPIKeyEnv, "")
	regressionDatabricksProfile(t, "databricks-test", []providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2"}})
	vaulttest.Secret(t, OpencodeAPIKeyEnv, "placeholder")
	InvalidateOpencodeAuth()
	own := "provider=opencode-go dispatch=own-login opencode_login=true "
	if row, ok := (opencodeEngine{}).RouteDetail(); !ok || !strings.HasPrefix(row, own) {
		t.Fatalf("unpinned row = %q, want today's own-login row %q…", row, own)
	}
	vaulttest.SetRecord(t, "opencode", "profile", "databricks-test")
	if row, _ := (opencodeEngine{}).RouteDetail(); !strings.HasPrefix(row, "provider=databricks-test ") {
		t.Fatalf("pinned row = %q, want it to name the pinned profile databricks-test", row)
	}
}
