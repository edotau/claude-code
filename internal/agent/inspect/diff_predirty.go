package inspect

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
)

func preDirtyOutside(root string, sel []string, v *VerifyReport) ([]string, string) {
	if len(sel) == 0 {
		return nil, ""
	}
	var touched *[]string
	var id string
	if v != nil {
		touched, id = v.PreDirty, v.ID
	} else if r, err := agent.LoadRecord(os.Getenv(proc.DispatchIDEnv)); err == nil {
		touched, id = r.PreDirtyTouched, r.ID
	}
	if touched == nil {
		return nil, id
	}
	var out []string
	for _, p := range *touched {
		if !filepath.IsAbs(p) {
			continue
		}
		rel, err := cleanPaths(root, []string{p})
		if err == nil && !inPaths(sel, rel[0]) {
			out = append(out, rel[0])
		}
	}
	slices.Sort(out)
	return slices.Compact(out), id
}

func mergeOutside(outside, touched []string) []string {
	if len(touched) == 0 {
		return outside
	}
	out := append(slices.Clone(outside), touched...)
	slices.Sort(out)
	return slices.Compact(out)
}
