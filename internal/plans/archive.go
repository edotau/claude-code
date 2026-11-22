package plans

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Archive moves every dated plan in root's home whose tasks are all ticked or struck into the memory store.
func Archive(ctx context.Context, root string, dryRun bool) ([]Change, []Finding, error) {
	dated, _, err := auditPlans(Home(root), true)
	if err != nil {
		return nil, nil, err
	}
	dest := archiveDir(root)
	changes, findings := []Change{}, []Finding{}
	for _, d := range dated {
		if err := ctx.Err(); err != nil {
			return changes, findings, err
		}
		s, ok := archivable(d.plan)
		if !ok {
			continue
		}
		to := filepath.Join(dest, filepath.Base(d.path))
		if _, err := os.Lstat(to); err == nil {
			findings = append(findings, Finding{strings.TrimSuffix(filepath.Base(d.path), ".md"), "", "archive target exists", to})
			continue
		} else if !os.IsNotExist(err) {
			return changes, findings, err
		}
		c := Change{
			Time: now(), Plan: d.path, Action: "archive", Rule: "all tasks closed",
			Evidence: fmt.Sprintf("%d/%d ticked, %d struck", s.Ticked, len(d.plan.Tasks), s.Dropped),
			Reopen:   fmt.Sprintf("mv %s %s", to, d.path),
		}
		if !dryRun {
			if err := movePlan(c, to); err != nil {
				return changes, findings, err
			}
		}
		changes = append(changes, c)
	}
	return changes, findings, nil
}

// archivable is a plan with tasks, none open and none still claimed.
func archivable(p Plan) (Summary, bool) {
	s := summarize(p)
	return s, s.Ticked == s.Total && s.Total+s.Dropped > 0 && !slices.ContainsFunc(p.Tasks, func(t Task) bool { return t.Claim != "" })
}

func archiveDir(root string) string {
	slug := paths.RepoSlug(root)
	if slug == "" {
		slug = "claude"
	}
	return filepath.Join(paths.MemoryStoreDir(), "archive", "plans", slug)
}

// movePlan renames c.Plan to to and logs c; a failed log moves the plan back, so every move has its Reopen line.
func movePlan(c Change, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	if err := os.Rename(c.Plan, to); err != nil {
		return err
	}
	if err := appendAuditLog(c); err != nil {
		return errors.Join(err, os.Rename(to, c.Plan))
	}
	return nil
}
