package tree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanRecordsNewestModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-a.jsonl")
	lines := `{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":1}}}
{"type":"assistant","message":{"id":"m2","model":"databricks-claude-sonnet-5-5","usage":{"input_tokens":1}}}
{"type":"assistant","message":{"id":"m3","model":"<synthetic>","usage":{"input_tokens":1}}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (*scanCache)(nil).scan(path).Model; got != "databricks-claude-sonnet-5-5" {
		t.Errorf("model = %q, want the newest real one", got)
	}
}
