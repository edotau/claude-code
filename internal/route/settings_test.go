package route

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAgentSettings(t *testing.T) {
	yes := true
	full := AgentSettings{
		Route:         &Route{Provider: "p", Profile: "dev"},
		FallbackRoute: &Route{Host: "h"},
		FallbackMode:  FallbackPin, Model: "auto", Effort: "high", Runner: RunnerPi, Codemode: CodemodeOn,
		Tools: &yes, Timeout: "90s", Verbosity: VerbosityLow, Context: "ctx",
		ConfigRoute: &ConfigRoute{Session: true},
		Budget:      &Budget{USD: 1.5, MaxChildren: 3},
		Extras:      map[string]string{"k": "v"},
	}
	if b, _ := json.Marshal(AgentSettings{}); string(b) != "{}" {
		t.Fatalf("zero value = %s", b)
	}
	for _, cr := range []*ConfigRoute{{Session: true}, {Route: Route{Profile: "qa"}}} {
		in := full
		in.ConfigRoute = cr
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out AgentSettings
		if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
			t.Fatalf("round trip: %v\n%+v\n%+v", err, in, out)
		}
		if in.Validate() != nil {
			t.Fatalf("full value invalid: %v", in.Validate())
		}
	}
	bad := map[string]AgentSettings{
		"fallback_mode": {FallbackMode: "x"}, "effort": {Effort: "x"}, "runner": {Runner: "x"},
		"codemode": {Codemode: "x"}, "verbosity": {Verbosity: "x"}, "timeout": {Timeout: "x"},
		"budget": {Budget: &Budget{USD: -1}},
	}
	for field, s := range bad {
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: error = %v", field, err)
		}
	}
}
