package dispatchhook

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/contract"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// reportFailedJobs broadcasts each failed job and contract the result names; it returns one note per failed dispatch.
func reportFailedJobs(in *core.Input) string {
	if !core.SpawnTools[in.ToolName] && in.ToolName != "Workflow" {
		return ""
	}
	var all []string
	for _, text := range []string{cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText()), in.Nested("error")} {
		all = append(all, allIDs(text)...)
	}
	var notes []string
	for _, id := range slices.Compact(slices.Sorted(slices.Values(all))) {
		if id != "" {
			notes = append(notes, reportFailedJob(id))
		}
	}
	return core.JoinNonEmpty("\n", slices.Compact(slices.Sorted(slices.Values(notes)))...)
}

// allIDs is every job id a result names on a launch, exit or envelope line, not only its own head and exit.
func allIDs(text string) []string {
	var ids []string
	for l := range strings.SplitSeq(ansiRe().ReplaceAllString(text, ""), "\n") {
		if id := headID(strings.TrimSpace(l)); id != "" {
			ids = append(ids, id)
		}
	}
	return append(ids, ownIDs(text)...)
}

func reportFailedJob(id string) (note string) {
	rec, err := loadDispatch(id)
	if err == nil && (rec.Failed() || rec.Outcome() == "lost") && rec.FailKind != agent.FailCancelled {
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{
			Kind: worktree.FailDispatch, Subject: id, Summary: cmp.Or(rec.Error, rec.Cutoff, rec.Outcome()),
			Log: filepath.Join(paths.DispatchDir(), id+".json"), Repo: rec.Repo,
		})
		note = "dispatch " + id + " failed: " + agent.ResumeHint(rec) // record id and what it left; the session sends a narrower task
	}
	if c, err := contract.Load(id); err == nil && c.Failed != "" && c.Failed != contract.ReasonCancelled {
		log, _ := contract.Path(id)
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{
			Kind: worktree.FailContract, Subject: id, Summary: string(c.Failed), Log: log, Repo: rec.Repo,
		})
	}
	return note
}
