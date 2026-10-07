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

func TestMemoryRecallSkipsWhatIsInContext(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	cwd := t.TempDir()
	for name, body := range map[string]string{
		"activeContext.md": "# Active\n\n- alpha bravo charlie delta already in context\n",
		"conventions.md":   "# C\n\n- alpha bravo charlie delta echo fresh\n- echo only one shared term\n",
	} {
		if err := os.MkdirAll(memory.GlobalDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(memory.GlobalDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]string{"cwd": cwd, "session_id": "s"})
	if SessionStart(bytes.NewReader(raw), &bytes.Buffer{}) != ExitProceed {
		t.Fatal("SessionStart failed")
	}
	got := recallAs(t, "s", "alpha bravo charlie delta echo", cwd)
	if !strings.Contains(got, "echo fresh") || strings.Contains(got, "already in context") || strings.Contains(got, "one shared term") {
		t.Fatalf("want only the fresh, multi-term block: %q", got)
	}
	if again := recallAs(t, "s", "alpha bravo charlie delta echo", cwd); again != "" {
		t.Errorf("a block recalled once must not be re-injected: %q", again)
	}
	if other := recallAs(t, "t", "alpha bravo charlie delta echo", cwd); !strings.Contains(other, "already in context") {
		t.Errorf("another session's context is not this one's: %q", other)
	}
}

func TestRelevantHitsGatesOnMatchedTerms(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ranked := []memory.SearchHit{
		{Score: 20, Matched: 1, Block: "one shared word, top BM25"},
		{Score: 9, Matched: 2, Block: "covered"},
		{Score: 5, Matched: 2, Block: "covered, within floor"},
		{Score: 4, Matched: 3, Block: "under half the best covered hit"},
	}
	got := relevantHits("", "alpha bravo charlie delta", ranked)
	if len(got) != 2 || got[0].Block != "covered" || got[1].Block != "covered, within floor" {
		t.Fatalf("want the two covered hits within the floor; got %+v", got)
	}
	if long := relevantHits("", "a1 b2 c3 d4 e5 f6 g7 h8 i9 j10 k11 l12 m13", ranked); len(long) != 1 || long[0].Block != "under half the best covered hit" {
		t.Errorf("a 13-term query needs 3 matched terms; got %+v", long)
	}
}
