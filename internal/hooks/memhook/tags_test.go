package memhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordTags(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	rec := func(role, text string) string {
		return `{"type":"` + role + `","message":{"role":"` + role + `","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	first := rec("assistant", "hi\\nDECISION: use A\\n- BUG: leaks\\nplain") + rec("user", "NOTE: not mine")
	if err := os.WriteFile(tr, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	recordTags("s1", tr, now)
	recordTags("s1", tr, now) // the offset makes a repeat Stop a no-op
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(rec("assistant", "NEXT: ship\\nDECISION: use A"))
	_ = f.Close()
	recordTags("s1", tr, now)
	b, err := os.ReadFile(tagEventsPath("s1"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"event":"decision"`) || !strings.Contains(lines[1], `"event":"bug"`) || !strings.Contains(lines[2], `"text":"ship"`) {
		t.Fatalf("want decision, bug, next once each, got %q", lines)
	}
	multi := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"NOTE: a"},{"type":"tool_use"},{"type":"text","text":"BUG: b"}]}}` + "\n"
	f, _ = os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(multi + `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"NOTE: torn`)
	_ = f.Close()
	recordTags("s1", tr, now)
	f, _ = os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`"}]}}` + "\n")
	_ = f.Close()
	recordTags("s1", tr, now)
	b, _ = os.ReadFile(tagEventsPath("s1"))
	got := string(b)
	for _, want := range []string{`"text":"a"`, `"text":"b"`, `"text":"torn"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}
