package route

import (
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestPickTakesWholeLayer(t *testing.T) {
	low := Route{Provider: "databricks", Profile: "qa"}
	if got := Pick(Route{}, Route{Profile: "x"}, low); got != (Route{Profile: "x"}) {
		t.Fatalf("got %+v", got)
	}
	if !Pick().IsZero() || !Pick(Route{}, Route{}).IsZero() {
		t.Fatal("no layer must be zero")
	}
}

func TestParseLegacy(t *testing.T) {
	for in, want := range map[string]Route{
		"databricks:qa": {Provider: "databricks", Profile: "qa"}, " databricks ": {Provider: "databricks"},
		"qa": {Profile: "qa"}, "": {}, "databricks:": {}, "databricks:a:b": {}, "nope:qa": {},
	} {
		if got := ParseLegacy(in); got != want {
			t.Errorf("ParseLegacy(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	for r, ok := range map[Route]bool{
		{}: true, {Provider: "databricks", Profile: "qa", Identity: "sp"}: true, {Provider: "github", Account: "a"}: true,
		{Profile: "qa"}: false, {Identity: "sp"}: false, {Account: "a"}: false,
		{Provider: "github", Identity: "sp"}: false, {Provider: "databricks", Account: "a"}: false,
		{Provider: "databricks", Host: "h", Remote: "r"}: false, {Host: "h"}: true,
		{Provider: "databricks", Profile: "a:b"}: false, {Provider: "databricks", Profile: "a b"}: false,
	} {
		if err := r.Validate(); (err == nil) != ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", r, err, ok)
		}
	}
}

func TestTargetJSON(t *testing.T) {
	tg := Target{Route{Provider: "databricks", Profile: "qa", Identity: "sp", Host: "h"}, `a/b[1m]"x"`}
	want := `{"provider":"databricks","profile":"qa","identity":"sp","host":"h","model":"a/b[1m]\"x\""}`
	if got := tg.JSON(); got != want {
		t.Fatalf("got %s", got)
	}
	if got := (Target{}).JSON(); got != "{}" {
		t.Fatalf("got %s", got)
	}
}
