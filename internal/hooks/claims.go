package hooks

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/notices"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
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

// worktreeClaim records the written path, or each sha a landed `git commit` printed, for this session; errors drop.
func worktreeClaim(in *Input) Result {
	owner := claimOwner()
	if owner == 0 || in.SessionID == "" {
		return Result{}
	}
	now := time.Now().Unix()
	var lines []claim
	if p := in.nested("tool_input", "file_path"); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(in.CWD, p)
		}
		lines = append(lines, claim{Path: canonPath(p), Agent: in.AgentID, TS: now})
	} else if in.ToolName == "Bash" && gitCommitCmd.MatchString(in.ToolCommand()) {
		out := cmp.Or(in.nested("tool_response", "stdout"), in.ToolResponseText())
		for _, m := range commitLine.FindAllStringSubmatch(out, -1) {
			lines = append(lines, claim{Commit: m[1], Agent: in.AgentID, TS: now})
		}
	}
	appendClaims(claimsFile(owner, in.SessionID), lines)
	return Result{}
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
