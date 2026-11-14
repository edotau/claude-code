package waves

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
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
	var lines []string
	for _, l := range w.Leaves {
		lines = append(lines, leafRow(l, w))
	}
	lines = append(lines, fmt.Sprintf("wave %d %s: %d leaves base %s", w.N, w.Status, len(w.Leaves), base))
	if w.Status == "dry-run" {
		lines = append(lines, drySummary(w.Packages))
	}
	if len(w.Manual) > 0 {
		lines = append(lines, "  manual: "+strings.Join(w.Manual, ", "))
	}
	if w.Reason != "" {
		lines = append(lines, "  "+strings.ReplaceAll(w.Reason, "\n", " "))
	}
	lines = append(lines, findingLines(w)...)
	if w.Verifier != nil {
		lines = append(lines, fmt.Sprintf("  verifier exit %d: %s saved %s", w.Verifier.Exit,
			strings.ReplaceAll(w.Verifier.Cmd, "\n", " "), w.Verifier.Saved))
	}
	_, err := fmt.Fprintln(out, strings.Join(lines, "\n"))
	return err
}

// findingLines are the wave's file findings and its detection-only notice, one line each.
func findingLines(w WaveState) []string {
	var lines []string
	if len(w.Outside) > 0 {
		lines = append(lines, "  outside: "+strings.Join(w.Outside, ", "))
	}
	if len(w.Unattributed) > 0 {
		lines = append(lines, "  unattributed: "+strings.Join(w.Unattributed, ", ")+" (writer unknown)")
	}
	if len(w.Violations) > 0 {
		var rows []string
		for _, v := range w.Violations {
			rows = append(rows, violationText(v))
		}
		lines = append(lines, "  ownership: "+strings.Join(rows, "; "))
	}
	if len(w.Fallback) > 0 {
		lines = append(lines, "  no write record: "+strings.Join(w.Fallback, ",")+" (judged by run window, write time)")
	}
	if ids := detectOnly(w); len(ids) > 0 {
		lines = append(lines, "  scope: "+strings.Join(ids, ",")+" cannot refuse a foreign write; each ran alone, checked after")
	}
	return lines
}

// detectOnly lists up to four ids, then a count, of a multi-leaf wave's leaves launched with no enforced scope.
func detectOnly(w WaveState) []string {
	var ids []string
	for _, l := range w.Leaves {
		if len(w.Leaves) > 1 && !l.Scoped && l.DispatchID != "" {
			ids = append(ids, l.ID)
		}
	}
	if len(ids) > 4 {
		ids = append(ids[:4], fmt.Sprintf("+%d", len(ids)-4))
	}
	return ids
}

func violationText(v CheckViolation) string {
	if len(v.Owners) == 0 {
		return v.Package + " wrote " + v.File + " (no owner)"
	}
	return v.Package + " wrote " + v.File + " (owner " + strings.Join(v.Owners, ",") + ")"
}

// rowWidth caps a package row at one terminal line.
const rowWidth = 100

// leafRow is one package's row: id, agent, ok or FAIL, elapsed and, for a failure, the reason.
func leafRow(l LeafState, w WaveState) string {
	row := fmt.Sprintf("  %-5s %-8s %-4s %6s", l.ID, l.Agent, leafMark(l), leafElapsed(l))
	if leafMark(l) != "FAIL" {
		return row
	}
	return row + "  " + textutil.CapRunes(leafReason(l, w), rowWidth-len(row)-3, "…")
}

// leafMark is ok or FAIL once the leaf is judged, else what it is still doing.
func leafMark(l LeafState) string {
	switch {
	case l.Verdict == "failed" || l.Status == "error" || l.Status == "quota":
		return "FAIL"
	case l.Status == LeafStateOK && l.Verdict == LeafStateOK:
		return "ok"
	case l.Status == "running":
		return "run"
	case l.Status == LeafStateOK:
		return "wait"
	}
	return l.Status
}

func leafElapsed(l LeafState) string {
	if l.ElapsedMs <= 0 {
		return "-"
	}
	return (time.Duration(l.ElapsedMs) * time.Millisecond).Round(time.Second).String()
}

// leafReason says in a few words why a leaf failed, the first cause found in the order the checks run.
func leafReason(l LeafState, w WaveState) string {
	switch {
	case l.DispatchID == "":
		return "no dispatch: the launch failed"
	case l.Status != LeafStateOK:
		return "dispatch " + l.Status
	case l.Mismatch:
		return "routing mismatch"
	}
	for _, v := range w.Violations {
		if v.Package == l.ID {
			return violationText(v)
		}
	}
	switch {
	case l.Verify != nil && l.Verify.Exit != 0:
		return fmt.Sprintf("verify exit %d: %s", l.Verify.Exit, strings.ReplaceAll(l.Verify.Cmd, "\n", " "))
	case len(w.Outside)+len(w.Unattributed) > 0:
		return "files outside its owners changed, see below"
	}
	return "owned-file check failed"
}

func (r *runner) preview(p plans.WavePlan) {
	for _, w := range p.Waves {
		if !r.selected(w.Number) {
			continue
		}
		specs, manual := r.specs(w)
		for i := range specs {
			if _, enforced := scopeFlags(specs[i], LaunchOptions{Wave: specs, Route: r.o.Route}); len(specs) > 1 {
				specs[i].Scope = map[bool]string{true: "scoped", false: "detect-only"}[enforced]
			}
		}
		for _, task := range p.Manual {
			manual = append(manual, task.ID)
		}
		r.state = r.state.WithWave(WaveState{N: w.Number, Status: "dry-run", Packages: specs, Manual: manual})
	}
}

func drySummary(specs []Spec) string {
	var packages []string
	for _, p := range specs {
		packages = append(packages, strings.TrimSpace(p.ID+" owns: "+strings.Join(p.Owns, ",")+" "+p.Scope))
	}
	return "  launch: " + strings.Join(packages, "; ")
}
