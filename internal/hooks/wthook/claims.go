package wthook

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/schemas"
	"github.com/edotau/claude-code/internal/platform/shell"
	"github.com/edotau/claude-code/internal/routing/inflight"
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

// ownerStart reads the owner's start token each claim line carries; a var seam beside claimOwner.
var ownerStart = proc.StartTime

// claimsFile is <claims>/<owner pid>-<session slug>.jsonl; a dead owner pid retires the whole file.
func claimsFile(owner int, session string) string {
	return filepath.Join(worktree.ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", owner, worktree.SessionSlug(session)))
}

// claimAgent names the leaf behind a claim: a subagent's id, else a dispatched non-Claude agent's dispatch id.
func claimAgent(in *core.Input) string { return cmp.Or(in.AgentID, hookDispatch(in)) }

// hookDispatch is the dispatch id in a non-Claude hook's environment; Claude's own leaves carry agent_id instead.
func hookDispatch(in *core.Input) string {
	if in.Agent() == core.AgentClaude {
		return ""
	}
	return os.Getenv(proc.DispatchIDEnv)
}

// runningDispatches lists running dispatch records, read concurrently; a var seam so tests supply them.
var runningDispatches = func() ([]agent.Record, error) { return agent.RunningRecent(agent.AllRecords) }

// ownerHops bounds the ancestor walk from a claim's owner pid to a dispatch's job process.
const ownerHops = 8

// recordDispatch is the id of the running dispatch whose job process is owner or an ancestor of it; "" when none.
func recordDispatch(owner int) string {
	recs, err := runningDispatches()
	if err != nil || len(recs) == 0 || owner <= 1 {
		return ""
	}
	// An owner match needs no ancestor probes, but record order still decides the winner.
	chain := sync.OnceValue(func() map[int]bool { return ownerChain(owner) })
	for _, r := range recs {
		if r.PID > 1 && (r.PID == owner || chain()[r.PID]) && r.Alive() {
			return r.ID
		}
	}
	return ""
}

func ownerChain(owner int) map[int]bool {
	chain := map[int]bool{}
	for pid := owner; pid > 1 && len(chain) < ownerHops && !chain[pid]; pid = proc.Parent(pid) {
		chain[pid] = true
	}
	return chain
}

// Claim records the written path (a Bash command's sed -i, redirect or tee targets too) and each path `git add` staged.
func Claim(in *core.Input) core.Result {
	if in.SessionID == "" {
		return core.Result{}
	}
	staged := false
	if isBash(in) {
		staged = gitStageCmd.MatchString(in.ToolCommand())
		if !staged && !core.MayWrite(in.ToolCommand()) {
			return core.Result{}
		}
	} else if in.ToolFailed() {
		return core.Result{} // a failed edit changed nothing
	}
	edits := editPaths(in)
	markLaneRepos(in, edits)
	if len(edits) > 0 || staged {
		if owner := claimOwner(); owner != 0 {
			appendClaims(claimsFile(owner, in.SessionID), collectClaims(in, owner, edits, staged))
		}
	}
	return core.Result{}
}

func collectClaims(in *core.Input, owner int, edits []string, staged bool) []worktree.Claim {
	now := time.Now().Unix()
	stamp := claimAgent(in)
	if stamp == "" && in.Agent() != core.AgentClaude {
		stamp = recordDispatch(owner) // the hook's environment lacks the id: the running record names it
	}
	var lines []worktree.Claim
	for _, p := range edits {
		lines = append(lines, worktree.Claim{Path: p, Agent: stamp, TS: now})
	}
	if staged {
		lines = append(lines, stagedClaims(in, now, stamp)...) // external-agent edits are claimed when staged
	}
	if isBash(in) {
		lines = filterDirty(lines, !staged)
	}
	if len(lines) == 0 {
		return lines
	}
	start, _ := ownerStart(owner) // "" on failure: the line then reads as live while the pid is
	for i := range lines {
		lines[i].OwnerStart = start
	}
	return lines
}

// filterDirty drops claims on paths the command left clean (it committed them); outside a repo or on error all stay.
func filterDirty(lines []worktree.Claim, scoped bool) []worktree.Claim {
	if len(lines) == 0 {
		return nil
	}
	ctx, cancel := core.HookContext("worktree-claim")
	defer cancel()
	v := claimView{ctx: ctx, status: map[string]dirtyState{}} // one git status per toplevel, not per dir
	if scoped {
		edits := make([]string, len(lines))
		for i, c := range lines {
			edits[i] = c.Path
		}
		v.scopePaths(edits)
	}
	return slices.DeleteFunc(lines, func(c worktree.Claim) bool {
		var top string
		var dirty map[string]bool
		var err error
		if scoped {
			top, dirty, err = v.dirtyPath(c.Path)
		} else {
			top, dirty, err = v.dirtyAt(filepath.Dir(c.Path))
		}
		return top != "" && err == nil && !dirty[c.Path] // unknown (outside a repo, or an error) keeps the claim
	})
}

// stagedClaims claims, for the stager, each path the command's git add invocations left staged.
func stagedClaims(in *core.Input, now int64, stamp string) []worktree.Claim {
	ctx, cancel := core.HookContext("worktree-claim")
	defer cancel()
	var out []worktree.Claim
	shell.EachGit(in.ToolCommand(), in.CWD, func(target, verb string, args []string) bool {
		if verb == "add" {
			for _, p := range addedPaths(ctx, target, args) {
				out = append(out, worktree.Claim{Path: p, Agent: stamp, TS: now})
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
	top = paths.Real(top)
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
	lines = compactClaims(file, lines)
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

func compactClaims(file string, lines []worktree.Claim) []worktree.Claim {
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) == 0 {
		return lines
	}
	last := map[string]int64{}
	for l := range bytes.SplitSeq(raw, []byte("\n")) {
		var prev claim
		if json.Unmarshal(l, &prev) == nil && prev.Path != "" {
			last[prev.Path+"\x00"+prev.Agent] = prev.TS
		}
	}
	return slices.DeleteFunc(lines, func(c worktree.Claim) bool {
		ts, ok := last[c.Path+"\x00"+c.Agent]
		return ok && c.TS >= ts && c.TS-ts < 60
	})
}
