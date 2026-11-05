package memhook

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadOpencodeExportRealBytes(t *testing.T) {
	data, err := os.ReadFile("testdata/opencode-export.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadOpencodeExport(data)
	if err != nil {
		t.Fatal(err)
	}
	// write + edit on one file; the read and the patch parts are not edits
	if got.Edits != 2 || got.FinalText != "DONE" || !strings.HasPrefix(got.ID, "ses_") {
		t.Fatalf("got %+v", got)
	}
	if len(got.EditedPaths) != 1 || !strings.HasSuffix(got.EditedPaths[0], "/note.txt") {
		t.Fatalf("paths %v", got.EditedPaths)
	}
}

func TestLoadOpencodeExportBadJSON(t *testing.T) {
	if _, err := loadOpencodeExport([]byte("{")); err == nil {
		t.Fatal("want error")
	}
}

func TestLoadCopilotEvents(t *testing.T) {
	got := loadCopilotEvents("s1", []copilotEvent{
		{Type: "tool.execution_start", ToolName: "edit", Arguments: map[string]any{"path": "/a"}},
		{Type: "tool.execution_start", ToolName: "view", Arguments: map[string]any{"path": "/b"}},
		{Type: "tool.execution_start", ToolName: "create", Arguments: map[string]any{"path": "/a"}},
		{Type: "tool.execution_start", ToolName: "edit", Arguments: map[string]any{"path": "/sub"}, AgentID: "x"},
		{Type: "assistant.message", Content: "first"},
		{Type: "assistant.message", Content: "last"},
	})
	want := agentTranscript{ID: "s1", Edits: 2, EditedPaths: []string{"/a"}, FinalText: "last"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestLoadOpencodeExportFilters(t *testing.T) {
	doc := `{"info":{"id":"ses_x"},"messages":[
	{"info":{"role":"user"},"parts":[{"type":"text","text":"ask"},{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/u"}}}]},
	{"info":{"role":"assistant"},"parts":[
	{"type":"tool","tool":"edit","state":{"status":"error","input":{"filePath":"/failed"}}},
	{"type":"tool","tool":"write","state":{"status":"completed","input":{"filePath":"/z"}}},
	{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/a"}}},
	{"type":"text","text":"bye"}]}]}`
	got, err := loadOpencodeExport([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := agentTranscript{ID: "ses_x", Edits: 2, EditedPaths: []string{"/a", "/z"}, FinalText: "bye"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
