package wthook

import (
	"bytes"
	"cmp"
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
	Name: "claims", Owner: "internal/hooks/wthook", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(worktree.ClaimsDir(), "*.jsonl") },
	New:     func() any { return &claim{} },
	Version: claimsSchema, Keep: "per-session edit/commit claims; the claim check removes a dead owner pid's file",
})

// claimOwner is the session's Claude Code pid; a var seam so tests pick the owner.
var claimOwner = inflight.SessionOwner

// sessionSlug is the session id as the file-name slug every per-session state file uses.
func sessionSlug(session string) string { return paths.Slug(session, "", 80, "session") }

// claimsFile is <claims>/<owner pid>-<session slug>.jsonl; a dead owner pid retires the whole file.
func claimsFile(owner int, session string) string {
	return filepath.Join(worktree.ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", owner, sessionSlug(session)))
}

// Claim records the written path, each sha a landed `git commit` printed, and each path `git add` staged.
func Claim(in *core.Input) core.Result {
	if in.SessionID == "" {
		return core.Result{}
	}
	p, cmd := editPath(in), in.ToolCommand()
	bash := in.ToolName == "Bash" && (core.GitCommitCmd.MatchString(cmd) || gitStageCmd().MatchString(cmd))
	if p == "" && !bash {
		return core.Result{} // records nothing: skip the owner lookup, which spawns ps on macOS
	}
	owner := claimOwner()
	if owner == 0 {
		return core.Result{}
	}
	now := time.Now().Unix()
	var lines []claim
	if p != "" {
		lines = append(lines, claim{Path: p, Agent: in.AgentID, TS: now})
	} else {
		lines = bashClaims(in, now)
	}
	appendClaims(claimsFile(owner, in.SessionID), lines)
	return core.Result{}
}

// bashClaims are each sha a landed `git commit` printed and each path a `git add` left staged.
func bashClaims(in *core.Input, now int64) []claim {
	var lines []claim
	if core.GitCommitCmd.MatchString(in.ToolCommand()) {
		out := cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText())
		for _, m := range core.CommitLine.FindAllStringSubmatch(out, -1) {
			lines = append(lines, claim{Commit: m[1], Agent: in.AgentID, TS: now})
		}
	}
	if gitStageCmd().MatchString(in.ToolCommand()) {
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
	tops, ok, err := core.Git(ctx, target, "rev-parse", "--show-toplevel")
	if err != nil || !ok {
		return nil
	}
	top := core.CanonPath(tops[0])
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
func appendClaims(file string, lines []claim) {
	if len(lines) == 0 {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, c := range lines {
		c.Schema = claimsSchema
		if enc.Encode(c) != nil {
			return
		}
	}
	_ = paths.AppendLine(file, buf.Bytes())
}
