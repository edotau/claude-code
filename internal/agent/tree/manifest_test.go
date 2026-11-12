package tree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/codex"
)

// result.status wins when the script returns one; else the manifest status maps killed->error, completed->finished.
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

// Runs with identical Updated timestamps tie-break by ID ascending, not map-iteration order.
func TestBuildWorkflowRunsTieBreaksByID(t *testing.T) {
	sessionDir := t.TempDir()
	workflowsDir := filepath.Join(sessionDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := "2026-10-18T03:00:00Z"
	for _, id := range []string{"run-zzz", "run-aaa"} {
		manifest := map[string]string{"runId": id, "timestamp": ts, "status": "completed"}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workflowsDir, "wf_"+id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	runs := buildWorkflowRuns(sessionDir, now, "")
	if len(runs) != 2 {
		t.Fatalf("got %d workflow runs, want 2", len(runs))
	}
	if runs[0].ID != "run-aaa" || runs[1].ID != "run-zzz" {
		t.Errorf("got order [%s, %s], want [run-aaa, run-zzz] (ID-ascending tie-break on equal Updated)", runs[0].ID, runs[1].ID)
	}
}
