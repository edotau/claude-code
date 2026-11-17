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

func TestLoadScanCacheDiscardsOlderSchema(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := scanCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"schema":1,"files":{"/t/agent-a.jsonl":{"size":9,"offset":9}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c := loadScanCache()
	if len(c.Files) != 0 || c.Schema != scanCacheSchema {
		t.Fatalf("v1 cache kept: schema %d, files %v", c.Schema, c.Files)
	}
}
