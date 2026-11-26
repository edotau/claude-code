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

func TestUnitWhole(t *testing.T) {
	db := Route{Provider: "databricks", Profile: "qa"}
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"proxy", true}, {"direct", true}, {"team-a", true}, {"phase:g1", true}, {"phase:batch-20261126t120000.123-abc", true}, {"a:b:c", false}, {":g1", false}, {"phase:", false}, {"https://x.cloud.databricks.com/serving-endpoints", true},
		{"http://127.0.0.1:9", true}, {"http://example.com", false}, {"https://u:p@h/x", false}, {"https://h/x?k=v", false}, {"https://h/x?", false}, {"https://h/x#", false},
		{"Proxy", false}, {"a b", false}, {"https://", false}, {"direct:x", false}, {"proxy:x", false}, {"team.x", false}, {"team.x:g1", false},
	} {
		r := db
		r.Endpoint = tc.endpoint
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("endpoint %q: %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}
	for _, r := range []Route{{Endpoint: "proxy"}, {Endpoint: "proxy", Provider: "databricks"}, {Profile: "qa"}} {
		if r.Validate() == nil {
			t.Errorf("half unit %+v accepted", r)
		}
	}
}

func TestReadDoc(t *testing.T) {
	r, f, err := ReadDoc(`{"endpoint":"direct","provider":"databricks","profile":"qa","model":"x","fence":{"profiles":["qa"]}}`)
	if want := (Route{Endpoint: "direct", Provider: "databricks", Profile: "qa"}); err != nil || r != want || f.Open || !f.Allows("qa") {
		t.Fatalf("got %+v %+v %v", r, f, err)
	}
	if _, f, err := ReadDoc(`{"provider":"github","profile":"p"}`); err != nil || !f.Open {
		t.Fatalf("no fence key: %+v %v", f, err)
	}
	for _, bad := range []string{`{`, `{"profile":"qa"}`, `{"provider":"nope","profile":"x"}`, `{"endpoint":"proxy"}`} {
		if _, _, err := ReadDoc(bad); err == nil {
			t.Errorf("ReadDoc(%s) accepted", bad)
		}
	}
}
