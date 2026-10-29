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
	return recallAs(t, "", prompt, cwd)
}

func recallAs(t *testing.T, session, prompt, cwd string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"prompt": prompt, "cwd": cwd, "session_id": session})
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
	if err := os.MkdirAll(memory.HomeBankDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory.HomeBankDir(), "conventions.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "/compact alpha bravo charlie delta", "alpha bravo charlie", "zzz yyy xxx www vvv"} {
		if got := recall(t, p, cwd); got != "" {
			t.Errorf("prompt %q must be silent, got %q", p, got)
		}
	}
	got := recall(t, "alpha bravo charlie delta", cwd)
	if !strings.HasPrefix(got, "<!-- memory-recall:") || !strings.Contains(got, "[home/conventions.md] z") || !strings.Contains(got, " · alpha bravo charlie delta strong match") {
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

func TestMemoryRecallSkipsWhatIsInContext(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
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
	if SessionStart(bytes.NewReader(raw), &bytes.Buffer{}) != ExitProceed {
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

// The access boost breaks keyword ties toward the returned-to entry, but a returned-to entry the
// keyword/semantic gate rejects is still not admitted.
func TestPickTierAccessBoost(t *testing.T) {
	hit := func(id string, score float64, matched int) memory.SearchHit {
		return memory.SearchHit{Score: score, Matched: matched, Bank: "home", File: "decisionLog.md",
			Block: "## 2026-10-01 — Zebra quota " + id + " <!-- id:" + id + " -->\n\nzebra quota limiter fallback"}
	}
	ranked := []memory.SearchHit{hit("aa000001", 10, 3), hit("bb000002", 10, 3)}
	picks, passed := pickTier(ranked, false, 3, nil, nil, map[string]int{"bb000002": 3})
	if !passed || len(picks) != 2 || picks[0].e.ID != "bb000002" {
		t.Fatalf("the returned-to entry must rank first: %+v (passed %v)", picks, passed)
	}
	picks, _ = pickTier(ranked, false, 3, nil, nil, nil)
	if len(picks) != 2 || picks[0].e.ID != "aa000001" {
		t.Fatalf("without returns the stable keyword order must hold: %+v", picks)
	}

	weak := []memory.SearchHit{hit("aa000001", 10, 3), hit("cc000003", 2, 1)}
	picks, _ = pickTier(weak, false, 3, nil, nil, map[string]int{"cc000003": recallAccessCap})
	if len(picks) != 1 || picks[0].e.ID != "aa000001" {
		t.Fatalf("a returned-to entry failing the gate must not be admitted: %+v", picks)
	}
}

// TestMemoryRecallHandlesCwdGracefully: MemoryRecall must handle missing or invalid CWD
// by using os.Getwd as a fallback. Regression test for defect where os.Getwd error was
// swallowed with _, leaving root empty and degrading recall accuracy.
func TestMemoryRecallHandlesCwdGracefully(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	cwd := t.TempDir()

	// Create a memory bank with searchable content.
	for name, body := range map[string]string{
		"conventions.md": "# C\n\n- alpha bravo charlie delta strong\n",
	} {
		if err := os.MkdirAll(memory.HomeBankDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(memory.HomeBankDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Test 1: With explicit CWD provided, recall works.
	raw, _ := json.Marshal(map[string]string{"prompt": "alpha bravo charlie delta", "cwd": cwd})
	var out1 bytes.Buffer
	code1 := MemoryRecall(bytes.NewReader(raw), &out1)
	if code1 != ExitProceed {
		t.Fatalf("MemoryRecall with explicit CWD failed: code=%d", code1)
	}

	// Test 2: With empty CWD, fallback to Getwd should work (on most systems).
	raw2, _ := json.Marshal(map[string]string{"prompt": "alpha bravo charlie delta", "cwd": ""})
	var out2 bytes.Buffer
	code2 := MemoryRecall(bytes.NewReader(raw2), &out2)
	if code2 != ExitProceed {
		t.Fatalf("MemoryRecall with empty CWD failed: code=%d", code2)
	}
}
