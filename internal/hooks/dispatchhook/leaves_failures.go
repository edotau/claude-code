package dispatchhook

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// reportFailedJobs broadcasts each failed job and failed contract the call's result names; it never alters Release.
func reportFailedJobs(in *core.Input) {
	if !core.SpawnTools[in.ToolName] && in.ToolName != "Workflow" {
		return
	}
	var all []string
	for _, text := range []string{cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText()), in.Nested("error")} {
		all = append(all, allIDs(text)...)
	}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(all))) {
		if id != "" {
			reportFailedJob(id)
		}
	}
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

func reportFailedJob(id string) {
	rec, err := loadDispatch(id)
	if err == nil && (rec.Failed() || rec.Outcome() == "lost") && rec.FailKind != agent.FailCancelled {
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{
			Kind: worktree.FailDispatch, Subject: id, Summary: cmp.Or(rec.Error, rec.Cutoff, rec.Outcome()),
			Log: filepath.Join(paths.DispatchDir(), id+".json"), Repo: rec.Repo,
		})
	}
	if c, err := contract.Load(id); err == nil && c.Failed != "" && c.Failed != contract.ReasonCancelled {
		log, _ := contract.Path(id)
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{
			Kind: worktree.FailContract, Subject: id, Summary: string(c.Failed), Log: log, Repo: rec.Repo,
		})
	}
}
