package usage

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestScanWorkflowNamesSurvivesHugeLine: a transcript line over 10MB must not drop the records after it.
func TestScanWorkflowNamesSurvivesHugeLine(t *testing.T) {
	use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","input":{"name":"wfname"}}]}}`
	res := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"Run ID: wf_abc123"}]}}`
	huge := `{"type":"x","pad":"` + strings.Repeat("a", 11<<20) + `"}`
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, huge+"\n"+use+"\n"+res+"\n")
	if got := scanWorkflowNamesFromTranscript(path); got["wf_abc123"] != "wfname" {
		t.Fatalf("names = %v; want wf_abc123 -> wfname", got)
	}
}
