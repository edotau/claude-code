package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageReportCountsUnused(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	listing := `{"attachment":{"type":"skill_listing","content":"- alpha: does a\n- beta: does b"}}
{"attachment":{"type":"agent_listing_delta","addedLines":["- coder: writes\n (Tools: Bash)","- idle: nothing"]}}
{"attachment":{"type":"mcp_instructions_delta","addedNames":["srv"],"addedBlocks":["## srv\nblock"]}}
`
	use := `{"message":{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"alpha"}},` +
		`{"type":"tool_use","name":"Agent","input":{"subagent_type":"coder"}}]}}` + "\n"
	for name, body := range map[string]string{"a.jsonl": listing + use, "b.jsonl": listing} {
		if err := os.WriteFile(filepath.Join(proj, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := strings.Join(usageReport(dir, time.Now(), time.Second), "\n")
	for _, want := range []string{"2 sessions", "skill: 1 never used of 2", "beta", "agent: 1 never used of 2", "idle", "mcp: 1 never used of 1", "srv"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "alpha") || strings.Contains(out, "coder") {
		t.Errorf("used item listed as unused:\n%s", out)
	}
	for l := range strings.SplitSeq(out, "\n") {
		if len(l) > 80 {
			t.Errorf("line over 80 cols: %q", l)
		}
	}
}
