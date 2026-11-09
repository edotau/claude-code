package agentcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestAgentsLabelPrintsPrefix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdAgentsLabel([]string{"Plan", "--session-model", "databricks-claude-opus-5-5"})
	})
	if code != 0 || out != "databricks-claude-opus-5-5: \n" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want the session prefix only", code, out, stderr)
	}
}

func TestAgentsLabelEmptyMeansNoPrefix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Chdir(t.TempDir())
	brief := filepath.Join(t.TempDir(), "brief")
	if err := os.WriteFile(brief, []byte("Mode: compare\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"compare", []string{"Explore", "--brief", brief, "--session-model", "sess-x"}},
		{"unknown-explore-session", []string{"Explore"}},
		{"unknown-plan-session", []string{"Plan"}},
	} {
		for _, format := range []string{"text", "json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				args := append(append([]string{}, tc.args...), "--format", format)
				code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsLabel(args) })
				if code != 0 || stderr != "" {
					t.Fatalf("exit = %d, stderr = %q, want success without diagnostics", code, stderr)
				}
				assertEmptyLabelOutput(t, format, out)
			})
		}
	}
}

func assertEmptyLabelOutput(t *testing.T, format, out string) {
	t.Helper()
	if format == "json" {
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &row); err != nil {
			t.Fatal(err)
		}
		if got := string(row["label"]); got != `""` {
			t.Fatalf("JSON label = %s, want an empty string", got)
		}
		return
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no prefix", out)
	}
}

// Source names where the label's model came from: the dispatch's --model, the session, or the pin and route.
func TestLabelSource(t *testing.T) {
	for _, c := range []struct{ dispatch, label, session, want string }{
		{"opus", "opus", "s", "dispatch"}, {"inherit", "s", "s", "session"},
		{"", "s", "s", "session"}, {"", "sonnet", "s", "derived"}, {"", "", "", "derived"},
		{"opus", "databricks-gpt-6-1-sol", "sess-x", "derived"},
		{"opus", "sess-x", "sess-x", "session"},
		{"opus", "", "sess-x", "derived"},
	} {
		if got := labelSource(c.dispatch, c.label, c.session); got != c.want {
			t.Errorf("labelSource(%q,%q,%q) = %q, want %q", c.dispatch, c.label, c.session, got, c.want)
		}
	}
}
