package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastUsageAndEdits(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	big := `{"type":"user","message":{"content":"` + strings.Repeat("z", tailBytes+10) + `"}}`
	lines := []string{
		`{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":40}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/b.go"}},{"type":"tool_use","name":"Read","input":{"file_path":"/r/skip.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/r/a.go"}}]}}`,
		big,
	}
	_ = os.WriteFile(tr, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	u, ok := LastUsage(tr)
	if !ok || u.Tokens != 100 || u.Model != "claude-opus-5" {
		t.Errorf("full-scan fallback: %+v %v", u, ok)
	}
	if got := EditedFiles(tr, 10); strings.Join(got, ",") != "/r/a.go,/r/b.go" {
		t.Errorf("edits %v", got)
	}
	if _, ok := LastUsageTokens(filepath.Join(t.TempDir(), "missing")); ok {
		t.Error("missing transcript reported usage")
	}
}

func TestWindow(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-5[1m]")
	for _, c := range []struct {
		model string
		used  int
		want  int
	}{{"claude-opus-5", 10, 1_000_000}, {"claude-haiku-4-5", 10, 200_000}, {"claude-haiku-4-5", 300_000, 1_000_000}, {"x[1m]", 1, 1_000_000}} {
		if got := Window(c.model, c.used); got != c.want {
			t.Errorf("Window(%q,%d)=%d want %d", c.model, c.used, got, c.want)
		}
	}
}
