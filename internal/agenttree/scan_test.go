package agenttree

import "testing"

// TestResultStatusMapsTopLevelStatusWhenResultAbsent: result.status wins outright when the script's
// return value carries one; otherwise the manifest's own top-level status maps killed->error,
// completed->finished; anything else (or no status at all) yields "".
func TestResultStatusMapsTopLevelStatusWhenResultAbsent(t *testing.T) {
	cases := []struct {
		name string
		m    workflowManifest
		want string
	}{
		{"result.status wins over a killed top-level status", workflowManifest{Result: []byte(`{"status":"green"}`), Status: "killed"}, "green"},
		{"killed top-level maps to error when result carries no status", workflowManifest{Result: []byte(`null`), Status: "killed"}, statusError},
		{"completed top-level maps to finished when result carries no status", workflowManifest{Result: []byte(`null`), Status: "completed"}, statusFinished},
		{"an unrecognized top-level status maps to nothing", workflowManifest{Result: []byte(`null`), Status: "paused"}, ""},
		{"no result and no top-level status maps to nothing", workflowManifest{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.resultStatus(); got != c.want {
				t.Errorf("resultStatus() = %q, want %q", got, c.want)
			}
		})
	}
}
