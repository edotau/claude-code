package render

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderHooksFileRejectsNullExistingObject(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "hooks.json")
	const existing = "null\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	merge := func(raw, body []byte) ([]byte, error) {
		next, _, err := MergeHooksTop(raw, body, func(item json.RawMessage) (*json.RawMessage, bool) {
			return &item, false
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(next)
	}
	var plan RenderPlan
	changed, err := RenderHooksFile(path, []byte(`{"hooks":{"SessionStart":[{"command":"claude-code hook SessionStart"}]}}`), merge, RenderOptions{}.Stage(&plan), io.Discard, "codex-hooks")
	if err == nil {
		t.Fatal("existing null hooks file accepted; want non-object error")
	}
	if !strings.Contains(err.Error(), "existing file: not a JSON object") {
		t.Fatalf("error = %v, want existing-file non-object error", err)
	}
	if changed {
		t.Fatal("rejected hooks file reported changed")
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Fatalf("hooks file = %q, want original %q", got, existing)
	}
}
