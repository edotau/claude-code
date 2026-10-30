package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLastActivityLineBoundaries(t *testing.T) {
	const records = `not JSON
{"type":"assistant","timestamp":"2026-10-18T03:00:00Z","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/repo/old.go"}},{"type":"text","text":"old"}]}}
{"type":"user","message":{"content":[{"type":"text","text":"ignore"}]}}

{"type":"assistant","timestamp":"2026-10-18T03:01:00Z","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/new.go"}},{"type":"text","text":"new"}]}}`
	for _, ending := range []string{"", "\n", "\r\n"} {
		t.Run("ending="+ending, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.jsonl")
			if err := os.WriteFile(path, []byte(records+ending), 0o600); err != nil {
				t.Fatal(err)
			}
			act := lastActivity(path, "/repo")
			want := Activity{Tool: "Edit", Arg: "new.go", Text: "new", At: time.Date(2026, 10, 18, 3, 1, 0, 0, time.UTC)}
			if act != want {
				t.Fatalf("activity = %+v, want %+v", act, want)
			}
		})
	}
}

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

func TestUserTurnTextConcatenatesTextBlocks(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"first "},{"type":"tool_use","name":"X"},{"type":"text","text":"second"}]}`)
	if got := userTurnText(raw); got != "first second" {
		t.Errorf("userTurnText = %q, want %q", got, "first second")
	}
}
