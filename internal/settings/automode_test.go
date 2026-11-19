package settings

import "testing"

func TestLaunchSettingsOverlay(t *testing.T) {
	for _, tc := range []struct {
		name       string
		databricks bool
		wantRC     bool
	}{{"anthropic", false, false}, {"databricks", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := LaunchSettings(tc.databricks)
			if err != nil {
				t.Fatal(err)
			}
			auto, ok := doc["autoMode"].(map[string]any)
			if !ok || len(auto) == 0 {
				t.Fatalf("autoMode layer missing: %v", doc)
			}
			rc, has := doc["remoteControlAtStartup"]
			if has != tc.wantRC || (has && rc != false) {
				t.Fatalf("remoteControlAtStartup = %v (set %v), want set %v and false", rc, has, tc.wantRC)
			}
		})
	}
}
