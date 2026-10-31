package worktree

import (
	"os"
	"strings"
	"testing"
	"time"
)

// resolveFixtureOpEnv isolates paths.StateDir() (and therefore operationsDir) per test, same pattern
// as mergeFixtureLockEnv in merge_test.go.
func resolveFixtureOpEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestSaveOperationThenLoadOperationRoundTrip(t *testing.T) {
	resolveFixtureOpEnv(t)
	started := time.Now().UTC().Truncate(time.Second)
	op := &Operation{
		ID:                "op-abc123",
		Repo:              "/repo/main",
		Worktree:          "feature",
		TargetOID:         "deadbeef",
		OriginalSourceOID: "cafef00d",
		Phase:             "resolving",
		StartedAt:         started,
		Deadline:          started.Add(10 * time.Minute),
		Agent:             "claude",
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	repo := &Repo{Main: "/repo/main"}
	got, err := LoadOperation(repo, "feature")
	if err != nil {
		t.Fatalf("LoadOperation: %v", err)
	}
	if got.ID != op.ID || got.Repo != op.Repo || got.Worktree != op.Worktree ||
		got.TargetOID != op.TargetOID || got.OriginalSourceOID != op.OriginalSourceOID ||
		got.Phase != op.Phase || got.Agent != op.Agent {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, op)
	}
	if !got.StartedAt.Equal(op.StartedAt) || !got.Deadline.Equal(op.Deadline) {
		t.Fatalf("time fields mismatch: got started=%v deadline=%v, want started=%v deadline=%v",
			got.StartedAt, got.Deadline, op.StartedAt, op.Deadline)
	}
}

func TestSaveOperationOverwritesPriorStateForSameRepoWorktree(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	first := &Operation{Repo: "/repo/main", Worktree: "feature", Phase: "resolving", Agent: "claude"}
	if err := SaveOperation(first); err != nil {
		t.Fatalf("SaveOperation (first): %v", err)
	}
	second := &Operation{Repo: "/repo/main", Worktree: "feature", Phase: "resolved", Agent: "claude"}
	if err := SaveOperation(second); err != nil {
		t.Fatalf("SaveOperation (second): %v", err)
	}
	got, err := LoadOperation(repo, "feature")
	if err != nil {
		t.Fatalf("LoadOperation: %v", err)
	}
	if got.Phase != "resolved" {
		t.Fatalf("Phase = %q, want the overwritten %q (same repo+worktree key)", got.Phase, "resolved")
	}
}

// TestLoadOperationMissingFileSurfacesNotExist exercises LoadOperation's documented contract: a
// missing file is the plain *os.PathError from os.ReadFile, so a caller can os.IsNotExist(err) it —
// e.g. --resume reporting "nothing to resume" instead of a generic failure.
func TestLoadOperationMissingFileSurfacesNotExist(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	_, err := LoadOperation(repo, "never-started")
	if err == nil {
		t.Fatal("LoadOperation: want an error for a never-saved operation")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("LoadOperation err = %v (%T), want os.IsNotExist true", err, err)
	}
}

// TestLoadOperationCorruptFileReturnsError writes garbage at the exact path SaveOperation would have
// used, proving LoadOperation neither panics nor silently returns a zero-value Operation on bad JSON.
func TestLoadOperationCorruptFileReturnsError(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	path := operationPath(repo.Main, "feature")
	if err := os.MkdirAll(operationsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOperation(repo, "feature")
	if err == nil {
		t.Fatal("LoadOperation: want an error for corrupt JSON")
	}
	if os.IsNotExist(err) {
		t.Fatalf("LoadOperation err = %v, want a decode error, not not-exist", err)
	}
	if !strings.Contains(err.Error(), "load operation") {
		t.Fatalf("err = %q, want it to name the failing step", err)
	}
}
