package settings

import "testing"

func TestLaunchSettingsWebTavilyHookOnlyOnDatabricks(t *testing.T) {
	for _, databricks := range []bool{true, false} {
		doc, err := LaunchSettings(databricks)
		if err != nil {
			t.Fatal(err)
		}
		if _, has := doc["hooks"]; has != databricks {
			t.Errorf("databricks=%v: hooks present=%v", databricks, has)
		}
	}
}
