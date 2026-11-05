package hooks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/notices"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/shell"
)

// claim is one claims-ledger line: a file this session or leaf wrote, or a commit it made.
type claim struct {
	Schema int    `json:"schema"`
	Path   string `json:"path,omitempty"`
	Commit string `json:"commit,omitempty"`
	Agent  string `json:"agent_id,omitempty"`
	TS     int64  `json:"ts"`
}

// claimsSchema is the claims ledger's current schema version.
const claimsSchema = 1

var _ = schemas.Register(schemas.Format{
	Name: "claims", Owner: "internal/hooks", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(claimsDir(), "*.jsonl") },
	New:     func() any { return &claim{} },
	Version: claimsSchema, Keep: "per-session edit/commit claims; the claim check removes a dead owner pid's file",
})

// claimOwner is the session's Claude Code pid; a var seam so tests pick the owner.
var claimOwner = inflight.SessionOwner

func claimsDir() string { return notices.ClaimsDir() } // one home for the path: the reader lives in notices

// claimsFile is <claims>/<owner pid>-<session slug>.jsonl; a dead owner pid retires the whole file.
func claimsFile(owner int, session string) string {
	return filepath.Join(claimsDir(), fmt.Sprintf("%d-%s.jsonl", owner, paths.Slug(session, "", 80, "session")))
}

// worktreeClaim records the written path, each sha a landed `git commit` printed, and each path `git add` staged.
func worktreeClaim(in *core.Input) core.Result {
	owner := claimOwner()
	if owner == 0 || in.SessionID == "" {
		return core.Result{}
	}
	now := time.Now().Unix()
	var lines []claim
	if p := in.Nested("tool_input", "file_path"); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(in.CWD, p)
		}
		lines = append(lines, claim{Path: canonPath(p), Agent: in.AgentID, TS: now})
	} else if in.ToolName == "Bash" {
		lines = bashClaims(in, now)
	}
	appendClaims(claimsFile(owner, in.SessionID), lines)
	return core.Result{}
}

// bashClaims are each sha a landed `git commit` printed and each path a `git add` left staged.
func bashClaims(in *core.Input, now int64) []claim {
	var lines []claim
	if gitCommitCmd.MatchString(in.ToolCommand()) {
		out := cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText())
		for _, m := range commitLine.FindAllStringSubmatch(out, -1) {
			lines = append(lines, claim{Commit: m[1], Agent: in.AgentID, TS: now})
		}
	}
	if gitStageCmd.MatchString(in.ToolCommand()) {
		lines = append(lines, stagedClaims(in, now)...) // Bash and external-agent edits are claimed when staged
	}
	return lines
}

// stagedClaims claims, for the stager, each path the command's git add invocations left staged.
func stagedClaims(in *core.Input, now int64) []claim {
	ctx, cancel := core.HookContext("worktree-claim")
	defer cancel()
	var out []claim
	shell.EachGit(in.ToolCommand(), in.CWD, func(target, verb string, args []string) bool {
		if verb == "add" {
			for _, p := range addedPaths(ctx, target, args) {
				out = append(out, claim{Path: p, Agent: in.AgentID, TS: now})
			}
		}
		return false
	})
	return out
}

// addedPaths are the staged paths (absolute) under one git add's pathspecs; any git error yields none.
func addedPaths(ctx context.Context, target string, args []string) []string {
	tops, ok, err := gitOut(ctx, target, "rev-parse", "--show-toplevel")
	if err != nil || !ok {
		return nil
	}
	top := canonPath(tops[0])
	scopes, _ := stageScopes(ctx, top, target, "add", args) // add never runs git here, so never errors
	staged, _, _ := gitOut(ctx, top, "diff", "--cached", "--name-only")
	var out []string
	for _, s := range staged {
		if p := filepath.Join(top, s); s != "" && underAny(p, scopes) {
			out = append(out, p)
		}
	}
	return out
}

// appendClaims appends lines to file under its lock; any error drops them (the ledger is advisory).
func appendClaims(file string, lines []claim) {
	if len(lines) == 0 || os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	_ = paths.WithFileLock(file, func() error {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		for _, c := range lines {
			c.Schema = claimsSchema
			if err := enc.Encode(c); err != nil {
				return errors.Join(err, f.Close())
			}
		}
		return f.Close()
	})
}
