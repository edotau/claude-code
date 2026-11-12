package wthook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/worktree"
)

// claim is one claims-ledger line: the reader's worktree.Claim stamped with the schema version.
type claim struct {
	Schema int `json:"schema"`
	worktree.Claim
}

// claimsSchema is the claims ledger's current schema version.
const claimsSchema = 1

var _ = schemas.Register(schemas.Format{
	Name: "claims", Owner: "internal/hooks/wthook", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(worktree.ClaimsDir(), "*.jsonl") },
	New:     func() any { return &claim{} },
	Version: claimsSchema, Keep: "per-session edit/stage claims; the claim check removes a dead owner pid's file",
})

// claimOwner is the session's Claude Code pid; a var seam so tests pick the owner.
var claimOwner = inflight.SessionOwner

// sessionSlug is the session id as the file-name slug every per-session state file uses.
func sessionSlug(session string) string { return paths.Slug(session, "", 80, "session") }

// claimsFile is <claims>/<owner pid>-<session slug>.jsonl; a dead owner pid retires the whole file.
func claimsFile(owner int, session string) string {
	return filepath.Join(worktree.ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", owner, sessionSlug(session)))
}

// Claim records the written path (a Bash command's sed -i, redirect or tee targets too) and each path `git add` staged.
func Claim(in *core.Input) core.Result {
	if in.SessionID == "" {
		return core.Result{}
	}
	edits := editPaths(in)
	staged := in.ToolName == "Bash" && gitStageCmd().MatchString(in.ToolCommand())
	if len(edits) == 0 && !staged {
		return core.Result{} // records nothing: skip the owner lookup, which spawns ps on macOS
	}
	owner := claimOwner()
	if owner == 0 {
		return core.Result{}
	}
	now := time.Now().Unix()
	var lines []worktree.Claim
	for _, p := range edits {
		lines = append(lines, worktree.Claim{Path: p, Agent: in.AgentID, TS: now})
	}
	if staged {
		lines = append(lines, stagedClaims(in, now)...) // external-agent edits are claimed when staged
	}
	appendClaims(claimsFile(owner, in.SessionID), lines)
	return core.Result{}
}

// stagedClaims claims, for the stager, each path the command's git add invocations left staged.
func stagedClaims(in *core.Input, now int64) []worktree.Claim {
	ctx, cancel := core.HookContext("worktree-claim")
	defer cancel()
	var out []worktree.Claim
	shell.EachGit(in.ToolCommand(), in.CWD, func(target, verb string, args []string) bool {
		if verb == "add" {
			for _, p := range addedPaths(ctx, target, args) {
				out = append(out, worktree.Claim{Path: p, Agent: in.AgentID, TS: now})
			}
		}
		return false
	})
	return out
}

// addedPaths are the staged paths (absolute) under one git add's pathspecs; any git error yields none.
func addedPaths(ctx context.Context, target string, args []string) []string {
	top, _ := paths.CheckoutOf(target)
	if top == "" {
		return nil
	}
	top = core.CanonPath(top)
	scopes, err := stageScopes(ctx, top, target, "add", args)
	if err != nil {
		return nil
	}
	staged, err := gitPaths(ctx, top, top, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range staged {
		if underAny(p, scopes) {
			out = append(out, p)
		}
	}
	return out
}

// appendClaims appends lines to file under its lock; any error drops them (the ledger is advisory).
func appendClaims(file string, lines []worktree.Claim) {
	if len(lines) == 0 {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, c := range lines {
		if enc.Encode(claim{claimsSchema, c}) != nil {
			return
		}
	}
	_ = paths.AppendLine(file, buf.Bytes())
}
