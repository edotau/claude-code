package usage

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkflowTextReportsWriteFailure(t *testing.T) {
	dir := isolatedUsageDir(t)
	writeWorkflowAgentFixture(t, dir, "project", "session", "wf_recent", "a", "Build", time.Minute,
		usageRecordLine("model", 7, 3, recentTS(time.Minute)))
	w := &partialReportWriter{err: errors.New("workflow destination closed")}
	var errw bytes.Buffer
	code := Run([]string{"--workflows", "--session", "session"}, dir, w, &errw)
	if code != 1 || !strings.Contains(errw.String(), "usage: write output: workflow destination closed") {
		t.Fatalf("partial workflow report exited successfully or hid write error: exit=%d stderr=%s", code, &errw)
	}
}

func TestWorkflowDaysFiltersResponseHistory(t *testing.T) {
	dir := isolatedUsageDir(t)
	old := usageRecordLine("expired-model", 123456, 7890, recentTS(10*24*time.Hour))
	writeWorkflowAgentFixture(t, dir, "project", "session", "wf_expired", "a", "Build", time.Minute, old)
	writeWorkflowAgentFixture(t, dir, "project", "session", "wf_mixed", "b", "Build", time.Minute, old,
		usageRecordLine("recent-model", 7, 3, recentTS(time.Minute)))
	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--days", "1"}, dir, &out, &errw); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &errw)
	}
	text := out.String()
	if strings.Contains(text, "wf_expired") || strings.Contains(text, "expired-model") {
		t.Fatalf("recent filesystem mtimes included expired response history: %s", text)
	}
	if !strings.Contains(text, "recent-model") || !strings.Contains(text, "workflow runs: 1   agent transcripts scanned: 1") {
		t.Fatalf("in-window history was lost: %s", text)
	}
	out.Reset()
	if code := Run([]string{"--workflows", "--session", "session"}, dir, &out, &errw); code != 0 {
		t.Fatalf("session exit=%d stderr=%s", code, &errw)
	}
	if !strings.Contains(out.String(), "expired-model") || !strings.Contains(out.String(), "wf_expired") {
		t.Fatalf("explicit session unexpectedly applied a days cutoff: %s", &out)
	}
}

// TestScanWorkflowNamesSurvivesHugeLine: a transcript line over 10MB must not drop the records after it.
func TestScanWorkflowNamesSurvivesHugeLine(t *testing.T) {
	use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","input":{"name":"wfname"}}]}}`
	res := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"Run ID: wf_abc123"}]}}`
	huge := `{"type":"x","pad":"` + strings.Repeat("a", 11<<20) + `"}`
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, huge+"\n"+use+"\n"+res+"\n")
	if got, _ := scanWorkflowNames(path); got["wf_abc123"] != "wfname" {
		t.Fatalf("names = %v; want wf_abc123 -> wfname", got)
	}
}
