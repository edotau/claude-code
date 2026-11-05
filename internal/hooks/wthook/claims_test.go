package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// claimInput is a parsed hook payload for the claims modules.
func claimInput(t *testing.T, doc map[string]any) *core.Input {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return core.ParseInput(strings.NewReader(string(b)))
}

// useClaimOwner pins the session owner pid the claims ledger files under.
func useClaimOwner(t *testing.T, pid int) {
	t.Helper()
	orig := claimOwner
	claimOwner = func() int { return pid }
	t.Cleanup(func() { claimOwner = orig })
}

// liveOwner and deadOwner are the fake session owner pids the claims tests file under.
const liveOwner, deadOwner = 100, 200

// TestWorktreeClaimRecordsEditsAndCommits: an Edit appends its path, a commit its sha; no session writes nothing.
func TestWorktreeClaimRecordsEditsAndCommits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	useClaimOwner(t, liveOwner)
	dir := core.CanonPath(t.TempDir())
	worktreeClaim(claimInput(t, map[string]any{"session_id": "", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	if _, err := os.Stat(claimsDir()); !os.IsNotExist(err) {
		t.Fatalf("no session must write no claims dir, stat err = %v", err)
	}
	worktreeClaim(claimInput(t, map[string]any{"session_id": "S1", "agent_id": "leaf1", "cwd": dir,
		"tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	worktreeClaim(claimInput(t, map[string]any{"session_id": "S1", "cwd": dir, "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "git commit -m x"},
		"tool_response": map[string]any{"stdout": "[main 1a2b3c4] x\n 1 file changed\n"}}))
	got := readClaims(t, claimsFile(liveOwner, "S1"))
	want := []claim{{Schema: claimsSchema, Path: filepath.Join(dir, "a.txt"), Agent: "leaf1"},
		{Schema: claimsSchema, Commit: "1a2b3c4"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("claims = %+v; want %+v (ts aside)", got, want)
	}
}

// readClaims decodes a claims file with each ts zeroed.
func readClaims(t *testing.T, file string) []claim {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []claim
	for line := range strings.Lines(string(b)) {
		var c claim
		if err := json.Unmarshal([]byte(line), &c); err != nil || c.TS == 0 {
			t.Fatalf("bad claim line %q: %v", line, err)
		}
		c.TS = 0
		out = append(out, c)
	}
	return out
}
