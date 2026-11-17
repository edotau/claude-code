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
	if len(rep.Files) == 0 {
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
	if !rep.fromBrief {
		renderFiles(o.Out, rep.Files)
	}
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

// renderOutside lists the files no scope covers: one per line, or for a brief's Owned line the first few on one line.
func renderOutside(w io.Writer, rep DiffReport) {
	if len(rep.Outside) == 0 {
		return
	}
	if !rep.fromBrief {
		fmt.Fprintf(w, "outside --path (%d):\n", len(rep.Outside))
		for _, p := range rep.Outside {
			fmt.Fprintf(w, "  %s\n", p)
		}
		return
	}
	shown := rep.Outside[:min(len(rep.Outside), outsideShown)]
	more := ""
	if n := len(rep.Outside) - len(shown); n > 0 {
		more = fmt.Sprintf(" ... %d more", n)
	}
	fmt.Fprintf(w, "outside the brief's Owned line (%d): %s%s\n", len(rep.Outside), strings.Join(shown, ", "), more)
}

func renderGates(o DiffOptions, gates []GateResult) {
	codes, color := map[string]string{"ok": ansi.Green, "FAIL": ansi.Red, "skip": ansi.Dim}, ansi.Enabled(o.Out)
	for _, g := range gates {
		status := ansi.Style(color, codes[g.Status], g.Status)
		if g.Status == "skip" {
			fmt.Fprintf(o.Out, "%s %s (%s)\n", status, g.Name, g.Reason)
		} else {
			fmt.Fprintf(o.Out, "%s %s (%.1fs)\n", status, g.Name, g.Seconds)
		}
		if g.OutputHead == "" {
			continue
		}
		for l := range strings.SplitSeq(g.OutputHead, "\n") {
			fmt.Fprintf(o.Out, "    %s\n", l)
		}
		if g.CutLines > 0 {
			fmt.Fprintf(o.Out, "    ... %d more lines cut\n", g.CutLines)
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
		n, more := len(*v.Wrote), ""
		if n > 0 && (*v.Wrote)[n-1] == agent.WroteTruncated {
			n, more = n-1, "+"
		}
		fmt.Fprintf(w, "  wrote: %d%s files\n", n, more)
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
