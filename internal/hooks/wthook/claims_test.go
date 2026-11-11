package wthook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/worktree"
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

// liveOwner and deadOwner are the fake session owner pids the claims tests file under; checkerOwner runs the check.
const liveOwner, deadOwner, checkerOwner = 100, 200, 300

// TestWorktreeClaimRecordsEdits: an Edit appends its path; no session writes nothing.
func TestWorktreeClaimRecordsEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	useClaimOwner(t, liveOwner)
	dir := core.CanonPath(t.TempDir())
	Claim(claimInput(t, map[string]any{"session_id": "", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	if _, err := os.Stat(worktree.ClaimsDir()); !os.IsNotExist(err) {
		t.Fatalf("no session must write no claims dir, stat err = %v", err)
	}
	Claim(claimInput(t, map[string]any{"session_id": "S1", "agent_id": "leaf1", "cwd": dir,
		"tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	got := readClaims(t, claimsFile(liveOwner, "S1"))
	want := []claim{{Schema: claimsSchema, Path: filepath.Join(dir, "a.txt"), Agent: "leaf1"}}
	if len(got) != len(want) || got[0] != want[0] {
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

// TestClaimSkipsOwnerLookupWhenNothingToRecord: only an edit path or a git add/commit looks up the session owner.
func TestClaimSkipsOwnerLookupWhenNothingToRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	lookups, orig := 0, claimOwner
	claimOwner = func() int { lookups++; return liveOwner }
	t.Cleanup(func() { claimOwner = orig })
	dir := core.CanonPath(t.TempDir())
	for _, cmd := range []string{"ls", "git status", "grep add x"} {
		Claim(claimInput(t, map[string]any{"session_id": "S1", "cwd": dir, "tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd}}))
	}
	if lookups != 0 {
		t.Fatalf("owner looked up %d times for calls that record nothing; want 0", lookups)
	}
	Claim(claimInput(t, map[string]any{"session_id": "S1", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	got := readClaims(t, claimsFile(liveOwner, "S1"))
	if lookups != 1 || len(got) != 1 || got[0].Path != filepath.Join(dir, "a.txt") {
		t.Fatalf("edit: %d lookups, claims %+v; want one lookup and the a.txt claim", lookups, got)
	}
}
