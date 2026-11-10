package memhook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func recall(t *testing.T, prompt, cwd string) string {
	t.Helper()
	return recallAs(t, "", prompt, cwd)
}

func recallAs(t *testing.T, session, prompt, cwd string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"prompt": prompt, "cwd": cwd, "session_id": session})
	res := Recall(core.ParseInput(bytes.NewReader(raw)))
	if res.Decision == core.Block {
		t.Fatalf("Recall blocked: %q", res.Reason)
	}
	return res.Context
}

func TestMemoryRecall(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cwd := t.TempDir()
	long := strings.Repeat("alpha bravo charlie delta padding ", 40)
	body := "# Conventions\n\n- alpha bravo charlie delta strong match " + long + "\n- alpha weak\n- unrelated rule entirely\n"
	writeBank(t, "conventions.md", body)
	for _, p := range []string{"", "/compact alpha bravo charlie delta", "alpha bravo charlie", "zzz yyy xxx www vvv"} {
		if got := recall(t, p, cwd); got != "" {
			t.Errorf("prompt %q must be silent, got %q", p, got)
		}
	}
	got := recall(t, "alpha bravo charlie delta", cwd)
	if !strings.HasPrefix(got, "<!-- memory-recall:") || !strings.Contains(got, "[home/conventions.md] z") || !strings.Contains(got, " · alpha bravo charlie delta strong match") {
		t.Fatalf("recall block: %q", got)
	}
	if strings.Contains(got, "alpha weak") || len([]rune(got)) > 1500 || !strings.Contains(got, "…") {
		t.Errorf("floor, total cap or per-hit cap not applied (%d chars): %q", len([]rune(got)), got)
	}
	t.Setenv(memory.UpdateChildEnv, "1")
	if recall(t, "alpha bravo charlie delta", cwd) != "" {
		t.Error("an update worker's prompts must not recall")
	}
}

func TestMemoryRecallSkipsWhatIsInContext(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cwd := t.TempDir()
	for name, body := range map[string]string{
		"activeContext.md": "# Active\n\n## Current Focus\n\n- alpha bravo charlie delta already in context\n",
		"conventions.md":   "# C\n\n- alpha bravo charlie delta echo fresh\n- echo only one shared term\n",
	} {
		if err := os.MkdirAll(memory.HomeBankDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(memory.HomeBankDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// session_id must be UUID-shaped: memoryContext's ResetSeen (which seeds the dedup set this test
	// exercises) only fires for a real session id, not a bare probe/test token (memory.LooksLikeSessionID).
	const sessionS = "44444444-4444-4444-8444-444444444444"
	const sessionT = "55555555-5555-4555-8555-555555555555"
	raw, _ := json.Marshal(map[string]string{"cwd": cwd, "session_id": sessionS})
	if Start(core.ParseInput(bytes.NewReader(raw))).Decision == core.Block {
		t.Fatal("SessionStart failed")
	}
	got := recallAs(t, sessionS, "alpha bravo charlie delta echo", cwd)
	if !strings.Contains(got, "echo fresh") || strings.Contains(got, "already in context") || strings.Contains(got, "one shared term") {
		t.Fatalf("want only the fresh, multi-term block: %q", got)
	}
	if again := recallAs(t, sessionS, "alpha bravo charlie delta echo", cwd); again != "" {
		t.Errorf("a block recalled once must not be re-injected: %q", again)
	}
	if other := recallAs(t, sessionT, "alpha bravo charlie delta echo", cwd); !strings.Contains(other, "already in context") {
		t.Errorf("another session's context is not this one's: %q", other)
	}
}

func writeBank(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(memory.HomeBankDir(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
