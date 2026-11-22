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
		err := paths.WithFileLock(plansLock(), func() error {
			c, f, err := archivePlan(d.path, dest, dryRun)
			if c != nil {
				changes = append(changes, *c)
			}
			if f != nil {
				findings = append(findings, *f)
			}
			return err
		})
		if err != nil {
			return changes, findings, err
		}
	}
	return changes, findings, nil
}

// archivePlan requires the plans lock through the eligibility check, move and audit rollback.
func archivePlan(planPath, dest string, dryRun bool) (*Change, *Finding, error) {
	b, err := os.ReadFile(planPath)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	p := parseBytes(planPath, b)
	s, ok := archivable(p)
	if !ok {
		return nil, nil, nil
	}
	to := filepath.Join(dest, filepath.Base(planPath))
	if _, err := os.Lstat(to); err == nil {
		return nil, &Finding{strings.TrimSuffix(filepath.Base(planPath), ".md"), "", "archive target exists", to}, nil
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	c := Change{
		Time: now(), Plan: planPath, Action: "archive", Rule: "all tasks closed",
		Evidence: fmt.Sprintf("%d/%d ticked, %d struck", s.Ticked, len(p.Tasks), s.Dropped),
		Reopen:   fmt.Sprintf("mv %s %s", to, planPath),
	}
	if !dryRun {
		if err := movePlan(c, to); err != nil {
			return nil, nil, err
		}
	}
	return &c, nil, nil
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
