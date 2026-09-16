package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

func recall(t *testing.T, prompt, cwd string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"prompt": prompt, "cwd": cwd})
	var out bytes.Buffer
	if code := MemoryRecall(bytes.NewReader(raw), &out); code != ExitProceed {
		t.Fatalf("MemoryRecall = %d", code)
	}
	if out.Len() == 0 {
		return ""
	}
	var doc struct {
		H struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not the context envelope: %v %s", err, out.String())
	}
	return doc.H.AdditionalContext
}

func TestMemoryRecall(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	cwd := t.TempDir()
	long := strings.Repeat("alpha bravo charlie delta padding ", 40)
	body := "# Conventions\n\n- alpha bravo charlie delta strong match " + long + "\n- alpha weak\n- unrelated rule entirely\n"
	if err := os.MkdirAll(memory.GlobalDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory.GlobalDir(), "conventions.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "/compact alpha bravo charlie delta", "alpha bravo charlie", "zzz yyy xxx www vvv"} {
		if got := recall(t, p, cwd); got != "" {
			t.Errorf("prompt %q must be silent, got %q", p, got)
		}
	}
	got := recall(t, "alpha bravo charlie delta", cwd)
	if !strings.HasPrefix(got, "<!-- memory-recall:") || !strings.Contains(got, "[global/conventions.md] - alpha bravo charlie delta strong match") {
		t.Fatalf("recall block: %q", got)
	}
	if strings.Contains(got, "alpha weak") || len([]rune(got)) > recallTotalCapChars || !strings.Contains(got, "…") {
		t.Errorf("floor, total cap or per-hit cap not applied (%d chars): %q", len([]rune(got)), got)
	}
	t.Setenv(memory.HarvestChildEnv, "1")
	if recall(t, "alpha bravo charlie delta", cwd) != "" {
		t.Error("a harvest worker's prompts must not recall")
	}
}
