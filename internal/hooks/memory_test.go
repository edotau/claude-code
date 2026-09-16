package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

func writeEdits(t *testing.T, path string, n int) {
	t.Helper()
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, n)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stubSpawn(t *testing.T, ok bool) *int {
	t.Helper()
	calls := 0
	orig := spawnHarvest
	spawnHarvest = func(string, string, string) bool { calls++; return ok }
	t.Cleanup(func() { spawnHarvest = orig })
	return &calls
}

func harvestPayload(tr, cwd string) string {
	return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q}`, cwd, tr)
}

func TestSessionHarvestTriggerAndReharvest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	calls := stubSpawn(t, true)
	run := func() int { return SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &bytes.Buffer{}) }

	writeEdits(t, tr, 2)
	if run() != ExitProceed || *calls != 0 {
		t.Fatal("under the edit floor must not spawn")
	}
	writeEdits(t, tr, 3)
	if run() != ExitProceed || *calls != 1 {
		t.Fatalf("3 edits must spawn once, calls=%d", *calls)
	}
	writeEdits(t, tr, 20)
	if run(); *calls != 1 {
		t.Error("re-harvest inside the cooldown")
	}
	stamp := stampPath("s1")
	writeStamp(stamp, time.Now().Add(-16*time.Minute), "bg", 3)
	writeEdits(t, tr, 12)
	if run(); *calls != 1 {
		t.Error("re-harvest with fewer than 10 new edits")
	}
	writeEdits(t, tr, 13)
	if run(); *calls != 2 {
		t.Error("10 new edits after the cooldown must re-harvest")
	}
	if st, _ := readStamp(stamp); !st.bg || st.edits != 13 {
		t.Errorf("stamp %+v", st)
	}

	// SessionEnd: nothing new since the last dispatch → no spawn; new edits → final harvest.
	if SessionHarvestEnd(strings.NewReader(harvestPayload(tr, cwd))); *calls != 2 {
		t.Error("session end with no new edits spawned")
	}
	writeEdits(t, tr, 14)
	if SessionHarvestEnd(strings.NewReader(harvestPayload(tr, cwd))); *calls != 3 {
		t.Error("session end with new edits must spawn")
	}

	t.Setenv(memory.HarvestChildEnv, "1")
	_ = os.Remove(stamp)
	if run(); *calls != 3 {
		t.Error("a harvest child must never spawn another")
	}
}

func TestSessionHarvestSpawnFailureBlocksThrice(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := filepath.Join(t.TempDir(), "proj")
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
	codes := []int{}
	for range 5 {
		var errb bytes.Buffer
		codes = append(codes, SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &errb))
		if codes[len(codes)-1] == ExitBlock && !strings.Contains(errb.String(), "/memory:end") {
			t.Errorf("block reason %q", errb.String())
		}
	}
	if fmt.Sprint(codes) != fmt.Sprint([]int{ExitBlock, ExitBlock, ExitBlock, ExitProceed, ExitProceed}) {
		t.Errorf("codes %v", codes)
	}

	// Evidence releases the gate: a bank write after the stamp.
	_ = os.Remove(stampPath("s1"))
	SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &bytes.Buffer{})
	writeStamp(stampPath("s1"), time.Now().Add(-time.Minute), "1", 5)
	if _, err := memory.Init(cwd); err != nil {
		t.Fatal(err)
	}
	if got := SessionHarvest(strings.NewReader(harvestPayload(tr, cwd)), &bytes.Buffer{}); got != ExitProceed {
		t.Error("a bank write after the prompt must release the gate")
	}
}

func TestSessionStartAutoInitAndNotice(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv(memory.HarvestChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	start := func(session, cwd string) string {
		var out bytes.Buffer
		SessionStart(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":%q}`, session, cwd)), &out)
		if out.Len() == 0 {
			return ""
		}
		var doc struct {
			H struct{ AdditionalContext string } `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.H.AdditionalContext
	}
	if got := start("s0", sub); got != "" || memory.HasBank(sub) {
		t.Errorf("a repo subdir must not be scaffolded: %q", got)
	}
	got := start("s1", repo)
	if !strings.Contains(got, "Memory bank scaffolded at") || !strings.Contains(got, "<!-- activeContext.md -->") {
		t.Fatalf("auto-init surface: %q", got)
	}
	_ = memory.WriteHarvestResult("s1", memory.HarvestResult{Status: memory.HarvestFailed, Time: time.Now(), Log: "/x.log"})
	got = start("s2", repo)
	if strings.Contains(got, "scaffolded") || !strings.Contains(got, "Previous session id: s1") || !strings.Contains(got, "did not complete (failed) (log: /x.log)") {
		t.Errorf("notice: %q", got[:min(len(got), 500)])
	}
	t.Setenv(memory.HarvestChildEnv, "1")
	start("child", repo)
	if b, _ := os.ReadFile(filepath.Join(memory.StateDir(), "last-session")); strings.TrimSpace(string(b)) != "s2" {
		t.Errorf("a harvest child advanced the session chain: %q", b)
	}
}
