package worktree

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// resolveFixtureOpEnv isolates paths.StateDir() (and so operationsDir) per test, like mergeFixtureLockEnv.
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
		TargetRef:         "main",
		TargetOID:         "deadbeef",
		OriginalSourceOID: "cafef00d",
		ConflictPaths:     []string{"f.txt"},
		Phase:             "resolving",
		StartedAt:         started,
		Deadline:          started.Add(10 * time.Minute),
		Agent:             "claude",
		AgentExitOK:       true,
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	repo := &Repo{Main: "/repo/main"}
	got, err := loadOperation(repo, "feature")
	if err != nil {
		t.Fatalf("loadOperation: %v", err)
	}
	if !reflect.DeepEqual(got, op) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, op)
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
	got, err := loadOperation(repo, "feature")
	if err != nil {
		t.Fatalf("loadOperation: %v", err)
	}
	if got.Phase != "resolved" {
		t.Fatalf("Phase = %q, want the overwritten %q (same repo+worktree key)", got.Phase, "resolved")
	}
}

// A missing operation file surfaces the plain os.ReadFile error so callers can use os.IsNotExist.
func TestLoadOperationMissingFileSurfacesNotExist(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	_, err := loadOperation(repo, "never-started")
	if err == nil {
		t.Fatal("loadOperation: want an error for a never-saved operation")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("loadOperation err = %v (%T), want os.IsNotExist true", err, err)
	}
}

// Garbage JSON at the operation path must yield an error, never a panic or a zero-value Operation.
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
	_, err := loadOperation(repo, "feature")
	if err == nil {
		t.Fatal("loadOperation: want an error for corrupt JSON")
	}
	if os.IsNotExist(err) {
		t.Fatalf("loadOperation err = %v, want a decode error, not not-exist", err)
	}
	if !strings.Contains(err.Error(), "load operation") {
		t.Fatalf("err = %q, want it to name the failing step", err)
	}
}

func TestOperationPathDistinguishesCollidingSlugs(t *testing.T) {
	a, b := operationPath("/r/x", "a-b"), operationPath("/r/x-a", "b")
	if a == b || operationPath("/r/x", "Fix") == operationPath("/r/x", "fix") {
		t.Errorf("operation paths collide: %s %s", a, b)
	}
}
