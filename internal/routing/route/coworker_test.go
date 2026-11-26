package route

import "testing"

func TestCheckProvider(t *testing.T) {
	for _, bad := range []Route{{Provider: "nope"}, {Provider: "databricks", Profile: "a:b"}, {Provider: "default"}} {
		if bad.CheckProvider() == nil {
			t.Errorf("CheckProvider(%+v) accepted", bad)
		}
	}
	for _, ok := range []Route{{}, {Provider: "anthropic"}, {Provider: "github", Profile: "personal"}, {Provider: "databricks", Profile: "dev"}} {
		if err := ok.CheckProvider(); err != nil {
			t.Errorf("CheckProvider(%+v) = %v", ok, err)
		}
	}
}
