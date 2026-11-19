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
			if off, _ := doc["disableRemoteControl"].(bool); off != tc.databricks {
				t.Fatalf("disableRemoteControl = %v, want %v", off, tc.databricks)
			}
		})
	}
}

func TestMissingTemplateEnv(t *testing.T) {
	tmpl := []byte(`{"env":{"A":"1","B":"2"}}`)
	for _, tc := range []struct{ name, user, want string }{
		{"missing", `{}`, "A,B"},
		{"user set", `{"env":{"A":"x"}}`, "B"},
	} {
		got := missingTemplateEnv(tmpl, []byte(tc.user))
		keys := ""
		for _, k := range []string{"A", "B"} {
			if _, ok := got[k]; ok {
				if keys != "" {
					keys += ","
				}
				keys += k
			}
		}
		if keys != tc.want || (tc.name == "missing" && got["A"] != "1") {
			t.Fatalf("%s: got %v, want keys %s", tc.name, got, tc.want)
		}
	}
}
