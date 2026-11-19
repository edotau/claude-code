package inspect

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

func renderDiff(o DiffOptions, rep DiffReport, scoped bool) {
	if len(rep.Files) == 0 && len(rep.Gates) == 0 && len(rep.Outside) == 0 {
		fmt.Fprintln(o.Out, "agents diff: no changes")
		renderVerify(o.Out, rep.Verify)
		return
	}
	if rep.noScope {
		renderVerify(o.Out, rep.Verify)
		fmt.Fprintf(o.Out, "scope not checked: the brief names no owned files and no --path was given; %d files changed"+
			" (details: --format json or --path)\n", len(rep.Files))
		fmt.Fprintln(o.Out, diffSummary(rep, scoped))
		return
	}
	if !rep.fromBrief && !rep.all {
		renderFiles(o.Out, rep.Files)
	}
	renderNewDecls(o.Out, rep.newDecls)
	renderOutside(o.Out, rep)
	renderGates(o, rep.Gates)
	renderVerify(o.Out, rep.Verify)
	fmt.Fprintln(o.Out, diffSummary(rep, scoped))
}

func renderFiles(w io.Writer, files []DiffFile) {
	for _, f := range files {
		counts, tag := fmt.Sprintf("+%d -%d", f.Added, f.Deleted), ""
		if f.Binary {
			counts = "bin"
		}
		if f.Test {
			tag = " (test)"
		}
		name := f.Path
		if f.From != "" {
			name = f.From + " -> " + f.Path
		}
		fmt.Fprintf(w, "%s %-12s %s%s\n", f.Status, counts, name, tag)
	}
}

func renderNewDecls(w io.Writer, rows []string) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "new tests and helpers (%d, approve or cut):\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "  %s\n", r)
	}
}

// renderOutside lists the files no scope covers: one per line, or for a brief's Owned line the first few on one line.
func renderOutside(w io.Writer, rep DiffReport) {
	if len(rep.Outside) == 0 {
		return
	}
	if !rep.fromBrief {
		fmt.Fprintf(w, "outside --path (%d):\n", len(rep.Outside))
		for _, p := range rep.Outside {
			renderOutsidePath(w, rep, p)
		}
		return
	}
	shown := rep.Outside[:min(len(rep.Outside), outsideShown)]
	more := ""
	if n := len(rep.Outside) - len(shown); n > 0 {
		more = fmt.Sprintf(" ... %d more", n)
	}
	fmt.Fprintf(w, "outside the brief's Owned line (%d): %s%s\n", len(rep.Outside), strings.Join(shown, ", "), more)
	for _, p := range rep.PreDirtyOutside {
		renderOutsidePath(w, rep, p)
	}
}

func renderOutsidePath(w io.Writer, rep DiffReport, p string) {
	if slices.Contains(rep.PreDirtyOutside, p) {
		fmt.Fprintf(w, "  %s: was already modified before dispatch %s started and is outside --path\n", p, rep.dispatch)
		return
	}
	fmt.Fprintf(w, "  %s\n", p)
}

func renderGates(o DiffOptions, gates []GateResult) {
	codes, color := map[string]string{"FAIL": ansi.Red, "skip": ansi.Dim}, ansi.Enabled(o.Out)
	for _, g := range gates {
		if g.Status == "ok" {
			continue
		}
		status := ansi.Style(color, codes[g.Status], g.Status)
		if g.Status == "skip" {
			fmt.Fprintf(o.Out, "%s %s (%s)\n", status, g.Name, g.Reason)
		} else {
			fmt.Fprintf(o.Out, "%s %s (%.1fs)\n", status, g.Name, g.Seconds)
		}
		for _, f := range g.Findings {
			fmt.Fprintln(o.Out, f)
		}
	}
}

func renderVerify(w io.Writer, v *VerifyReport) {
	if v == nil {
		return
	}
	ws := v.Workspace
	if v.Route != "" {
		ws += " (" + v.Route + ")"
	}
	fmt.Fprintf(w, "verify %s:\n  ran on: %s, workspace %s, model %s\n  fallback: %s\n", v.ID, v.Agent, ws, v.Model, v.Fallback)
	took := fmt.Sprintf("  took %s (%s..%s), status %s", (time.Duration(v.DurationMs) * time.Millisecond).Round(time.Second),
		v.Start, v.End, v.Status)
	if v.Tokens != nil {
		took += fmt.Sprintf(", tokens in=%d out=%d", v.Tokens.In, v.Tokens.Out)
	}
	fmt.Fprintln(w, took)
	if v.Wrote != nil {
		fmt.Fprintf(w, "  wrote: %d files%s\n", countFiles(*v.Wrote), truncatedNote(*v.Wrote))
	}
	if v.PreDirty != nil && len(*v.PreDirty) > 0 {
		fmt.Fprintf(w, "  ADVISORY: %d file(s) had uncommitted changes before the run and differ now;"+
			" if they were not yours to edit, check them:\n", countFiles(*v.PreDirty))
		for _, f := range *v.PreDirty {
			if f != agent.WroteTruncated {
				fmt.Fprintf(w, "    %s\n", f)
			}
		}
		if note := truncatedNote(*v.PreDirty); note != "" {
			fmt.Fprintf(w, "   %s\n", note)
		}
	}
	p := v.Proxy
	fmt.Fprintf(w, "  proxy: %d requests; %s\n", p.Requests, p.Note)
	if p.Match != "none" {
		fmt.Fprintf(w, "    via remote host: %d (%s); stream=broken: %d; non-200: %s\n", p.Remote,
			cmp.Or(strings.Join(p.Hosts, ", "), "none"), p.Broken, nonOKText(p.NonOK))
	}
	for _, m := range v.Mismatches {
		fmt.Fprintf(w, "  MISMATCH %s\n", m)
	}
}

func nonOKText(m map[string]int) string {
	if len(m) == 0 {
		return "0"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s x%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func scopeName(rep DiffReport) string {
	if rep.fromBrief {
		return "Owned"
	}
	return "--path"
}

func diffSummary(rep DiffReport, scoped bool) string {
	t := rep.Totals
	s := fmt.Sprintf("agents diff: %d files +%d -%d (prod +%d -%d, test +%d -%d)", t.Files, t.Added, t.Deleted,
		t.Prod.Added, t.Prod.Deleted, t.Test.Added, t.Test.Deleted)
	if rep.all {
		s = "agents diff --all"
	}
	switch {
	case len(rep.Outside) > 0:
		s += fmt.Sprintf("; %d outside %s", len(rep.Outside), scopeName(rep))
	case scoped:
		s += "; " + scopeName(rep) + " ok"
	}
	if len(rep.Gates) == 0 {
		return s
	}
	n := map[string]int{}
	for _, g := range rep.Gates {
		n[g.Status]++
	}
	s += fmt.Sprintf("; gates %d ok", n["ok"])
	if n["FAIL"] > 0 {
		s += fmt.Sprintf(", %d FAIL", n["FAIL"])
	}
	if n["skip"] > 0 {
		s += fmt.Sprintf(", %d skip", n["skip"])
	}
	return s
}

// countFiles is the number of real paths in list, leaving out the cap marker.
func countFiles(list []string) int {
	if n := len(list); n > 0 && list[n-1] == agent.WroteTruncated {
		return n - 1
	}
	return len(list)
}

// truncatedNote is the trailing note for a list cut at the record's cap, empty otherwise.
func truncatedNote(list []string) string {
	if len(list) > countFiles(list) {
		return " (list truncated at the cap)"
	}
	return ""
}
