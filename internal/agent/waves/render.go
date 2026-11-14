package waves

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/plans"
)

func render(out io.Writer, s State, jsonMode bool) error {
	if jsonMode {
		return json.NewEncoder(out).Encode(s)
	}
	for _, w := range s.Waves {
		if err := renderWave(out, w, s.Base); err != nil {
			return err
		}
	}
	if s.Next != "" {
		_, err := fmt.Fprintln(out, "next:", s.Next)
		return err
	}
	return nil
}

func renderWave(out io.Writer, w WaveState, base string) error {
	lines := []string{fmt.Sprintf("wave %d %s: %d leaves base %s", w.N, w.Status, len(w.Leaves), base)}
	if w.Status == "dry-run" {
		lines = append(lines, drySummary(w.Packages))
	}
	if len(w.Manual) > 0 {
		lines = append(lines, "  manual: "+strings.Join(w.Manual, ", "))
	}
	if w.Reason != "" {
		lines = append(lines, "  "+strings.ReplaceAll(w.Reason, "\n", " "))
	}
	if failed := failedSummary(w.Leaves); failed != "" {
		lines = append(lines, failed)
	}
	if len(w.Outside) > 0 {
		lines = append(lines, "  outside: "+strings.Join(w.Outside, ", "))
	}
	if len(w.Violations) > 0 {
		lines = append(lines, fmt.Sprintf("  ownership: %v", w.Violations))
	}
	if w.Verifier != nil {
		lines = append(lines, fmt.Sprintf("  verifier exit %d: %s saved %s", w.Verifier.Exit,
			strings.ReplaceAll(w.Verifier.Cmd, "\n", " "), w.Verifier.Saved))
	}
	_, err := fmt.Fprintln(out, strings.Join(lines, "\n"))
	return err
}

func (r *runner) preview(p plans.WavePlan) {
	for _, w := range p.Waves {
		if !r.selected(w.Number) {
			continue
		}
		specs, manual := r.specs(w)
		for _, task := range p.Manual {
			manual = append(manual, task.ID)
		}
		r.state = r.state.WithWave(WaveState{N: w.Number, Status: "dry-run", Packages: specs, Manual: manual})
	}
}

func drySummary(specs []Spec) string {
	var packages []string
	for _, p := range specs {
		packages = append(packages, p.ID+" owns: "+strings.Join(p.Owns, ","))
	}
	return "  launch: " + strings.Join(packages, "; ")
}

func failedSummary(leaves []LeafState) string {
	var failed []string
	for _, l := range leaves {
		if leafFailed(l) {
			failed = append(failed, fmt.Sprintf("%s %s %s saved %s", l.ID, l.Status, l.DispatchID, l.Saved))
		}
	}
	if len(failed) == 0 {
		return ""
	}
	return "  leaves: " + strings.Join(failed, "; ")
}
