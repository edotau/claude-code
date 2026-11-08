package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// autoDispatch is an auto-mode Agent payload for type st with a pinned model and description desc.
func autoDispatch(st, desc string) string {
	return fmt.Sprintf(`{"tool_name":"Agent","permission_mode":"auto","tool_input":{"subagent_type":%q,"model":"sonnet",`+
		`"prompt":"do it","description":%q}}`, st, desc)
}

// In auto mode a missing model prefix is denied once with the exact description to resend (tool input is never
// rewritten there); the resend passes, and outside auto mode the hook rewrites as before.
func TestDispatchLabelAutoModeDeniesOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "", "")
	r := agentEffortResult(core.ParseInput(strings.NewReader(autoDispatch("claude", "fix bug"))))
	if r.Decision != core.Block || r.UpdatedInput != nil || !strings.Contains(r.Reason, `"sonnet: fix bug"`) {
		t.Fatalf("unprefixed auto dispatch = %+v, want a Block naming \"sonnet: fix bug\" and no rewrite", r)
	}
	r = agentEffortResult(core.ParseInput(strings.NewReader(autoDispatch("claude", "sonnet: fix bug"))))
	if r.Decision == core.Block {
		t.Fatalf("resend with the right prefix was blocked: %+v", r)
	}
	manual := strings.Replace(autoDispatch("claude", "fix bug"), `"permission_mode":"auto",`, "", 1)
	r = agentEffortResult(core.ParseInput(strings.NewReader(manual)))
	if d, _ := r.UpdatedInput["description"].(string); r.Decision == core.Block || d != "sonnet: fix bug" {
		t.Fatalf("outside auto mode = %+v, want the rewrite", r)
	}
}

// An agent-named external leaf is labelled with the model its cell resolves to; a Profile: brief on claude with its profile's.
func TestDispatchLabelExternalTypeAndProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"qa"}, true)
	stubLabelModels(t, "gpt-6.1-sol", "databricks-qa-model")
	for st, c := range map[string]struct{ prompt, want string }{
		"copilot": {"do it", "gpt-6.1-sol: fix bug"}, "claude": {"Profile: qa\ndo it", "databricks-qa-model: fix bug"},
	} {
		payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":"fix bug"}}`, st, c.prompt)
		r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
		if d, _ := r.UpdatedInput["description"].(string); d != c.want {
			t.Errorf("%s label = %q, want %q", st, d, c.want)
		}
	}
}

// An `inherit` agent file labels with the session's model, read from the transcript.
func TestDispatchLabelInheritUsesSessionModel(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	stubLabelModels(t, "", "")
	if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "agents", "claude.md"), []byte("---\nname: claude\nmodel: inherit\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	line := `{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(tx, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"tool_name":"Agent","transcript_path":%q,"tool_input":{"subagent_type":"claude","prompt":"do it","description":"fix bug"}}`, tx)
	r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
	if d, _ := r.UpdatedInput["description"].(string); d != "claude-opus-4-7: fix bug" {
		t.Fatalf("description = %q (result %+v), want the session model prefix", d, r)
	}
}
