package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Operation is the durable record of one agent-mediated conflict resolution: the OIDs pinned before
// the agent ever ran, so a crash-and-resume revalidates against those exact commits, never a fresh
// Preflight's possibly-moved ones.
type Operation struct {
	ID                string
	Repo              string // repo.Main, the canonical main checkout path
	Worktree          string // worktree name, as passed to `worktree merge`
	TargetRef         string // the target branch pinned at resolve start; resume must match both this and TargetOID
	TargetOID         string
	OriginalSourceOID string
	ConflictPaths     []string // paths the agent was handed; finalizeAgentResolution never stages/commits outside these
	Phase             string   // "resolving" | "resolved" | "failed" | "timeout"
	StartedAt         time.Time
	Deadline          time.Time
	Agent             string // "claude" | "codex"
	AgentExitOK       bool   // true only once the agent process itself returned success with no fallback
}

// operationsDir holds one JSON file per repo+worktree under the harness's existing runtime state dir.
func operationsDir() string { return filepath.Join(paths.StateDir(), "worktree-merge-ops") }

// operationPath is deterministic from repo+name, so --resume needs no id lookup — just the same
// repo and worktree name the original `worktree merge --resolve-agent` call used.
func operationPath(repoMain, name string) string {
	slug := paths.Slug(filepath.Base(filepath.Clean(repoMain))+"-"+name, "._-", 0, "op")
	return filepath.Join(operationsDir(), slug+".json")
}

// operationLeasePath is a separate lock file (not operationPath+".lock", to avoid colliding with
// paths.WithFileLock's own ".lock" suffix convention) held for the full duration of one ResolveWithAgent
// call — fresh or resumed — so a concurrent resume can never race the still-running original.
func operationLeasePath(repoMain, name string) string {
	return operationPath(repoMain, name) + ".lease"
}

// SaveOperation atomically writes op, overwriting any prior state saved for the same repo+worktree.
func SaveOperation(op *Operation) error {
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return fmt.Errorf("save operation: %w", err)
	}
	return paths.AtomicWrite(operationPath(op.Repo, op.Worktree), data, 0o600)
}

// LoadOperation reads back whatever --resolve-agent last saved for repo+name; a missing file surfaces
// as the plain *os.PathError from os.ReadFile, so --resume can report "nothing to resume" itself.
func LoadOperation(repo *Repo, name string) (*Operation, error) {
	data, err := os.ReadFile(operationPath(repo.Main, name))
	if err != nil {
		return nil, err
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		return nil, fmt.Errorf("load operation: %w", err)
	}
	return &op, nil
}
