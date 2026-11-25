package memory

import (
	"slices"
	"strings"
)

const storeVersion = 4 // the "flat N" this binary writes

// StoreVersion is the layout version this binary writes.
func StoreVersion() int { return storeVersion }

// upgradeStep takes a store from From to From+1; Plan is pure, uses no model and must be idempotent.
type upgradeStep struct {
	From int
	Name string
	Plan func(d Doc) Batch // ops for one file; an empty Batch means the file is already in shape
}

var upgradeSteps = []upgradeStep{{From: 3, Name: "normalise", Plan: planNormalise}} // register by appending

// upgradeOps are step-only ops: applyUpgrade runs them, Batch.Validate must reject them from a model.
var upgradeOps = []string{"merge", "identify", "date"}

// planNormalise merges repeated headings, dates undated entries, then gives id files their markers.
func planNormalise(d Doc) Batch {
	var b Batch
	b.Ops = append(b.Ops, planMerge(d)...)
	work, _ := applyUpgrade(d, b, Env{})
	dates := planDate(work)
	b.Ops = append(b.Ops, dates...)
	work, _ = applyUpgrade(work, Batch{Ops: dates}, Env{})
	b.Ops = append(b.Ops, planIdentify(work)...)
	return b
}

func planMerge(d Doc) []Op {
	if d.Grammar == GrammarDated {
		return nil
	}
	var ops []Op
	seen := map[string]int{}
	for _, e := range d.Entries {
		if e.Kind != KindHeading {
			continue
		}
		h := strings.TrimRight(e.Lines[0], " \t")
		if seen[h]++; seen[h] == 2 {
			ops = append(ops, Op{Op: "merge", Title: h})
		}
	}
	return ops
}

func planDate(d Doc) []Op {
	if d.Grammar != GrammarDated {
		return nil
	}
	var ops []Op
	for i, e := range d.Entries {
		if e.Kind != KindHeading || e.Date != "" {
			continue
		}
		if date := nearestDate(d.Entries, i); date != "" {
			ops = append(ops, Op{Op: "date", ID: e.refOf(), Text: date})
		}
	}
	return dedupOps(ops)
}

// nearestDate is the date of the closest dated entry above i, else below; "" when the file has none.
func nearestDate(es []DocEntry, i int) string {
	for j := i - 1; j >= 0; j-- {
		if es[j].Date != "" {
			return es[j].Date
		}
	}
	for j := i + 1; j < len(es); j++ {
		if es[j].Date != "" {
			return es[j].Date
		}
	}
	return ""
}

func planIdentify(d Doc) []Op {
	prefix := idFiles[d.Name]
	if prefix == "" {
		return nil
	}
	var ops []Op
	for _, e := range d.Entries {
		if e.ID == "" && strings.HasPrefix(e.Lines[0], prefix) {
			ops = append(ops, Op{Op: "identify", ID: e.refOf()})
		}
	}
	return dedupOps(ops)
}

func dedupOps(ops []Op) []Op {
	return slices.CompactFunc(ops, func(a, b Op) bool { return a == b })
}

// applyUpgrade runs a batch: step-only ops here, every other op through Apply; d is never modified.
func applyUpgrade(d Doc, b Batch, env Env) (Doc, []OpResult) {
	res := make([]OpResult, 0, len(b.Ops))
	for _, o := range b.Ops {
		var r []OpResult
		if slices.Contains(upgradeOps, o.Op) {
			d, r = upgradeOp(d, o, env)
		} else {
			d, r = Apply(d, Batch{Ops: []Op{o}}, env)
		}
		res = append(res, r...)
	}
	return d, res
}

func upgradeOp(d Doc, o Op, env Env) (Doc, []OpResult) {
	ents := slices.Clone(d.Entries)
	switch o.Op {
	case "merge":
		ents = mergeHeading(ents, o.Title)
	case "date":
		for i, e := range ents {
			if e.refOf() == o.ID {
				ents[i] = withLines(e, i, dateHeading(e.Lines, o.Text))
			}
		}
	case "identify":
		for i, e := range ents {
			if e.refOf() == o.ID {
				ents[i] = withLines(e, i, identified(d.Name, i, e.Lines, env))
			}
		}
	}
	d.Entries = ents
	return d, []OpResult{{Op: o, Outcome: DreamOK}}
}

func withLines(e DocEntry, i int, lines []string) DocEntry {
	n := docEntry(lines)
	n.Seq = i
	return n
}

func dateHeading(lines []string, date string) []string {
	out := slices.Clone(lines)
	out[0] = "## " + date + " — " + strings.TrimPrefix(lines[0], "## ")
	return out
}

func identified(name string, idx int, lines []string, env Env) []string {
	if _, _, total := lastMarker(lines, fencedLines(lines)); env.NewID == nil || total > 0 {
		out, _ := settleBlockID(name, idx, lines, env.Now)
		return out
	}
	out := slices.Clone(lines)
	out[0] += " <!-- id:" + env.NewID(idx, lines[0]) + " -->"
	return out
}

// mergeHeading folds each later section under heading into the first one, keeping every body line.
func mergeHeading(ents []DocEntry, heading string) []DocEntry {
	groups := sections(ents)
	var res [][]DocEntry
	first := -1
	var endBlanks []string
	for gi, g := range groups {
		switch {
		case g[0].Kind != KindHeading || strings.TrimRight(g[0].Lines[0], " \t") != heading:
			res = append(res, g)
		case first < 0:
			first = len(res)
			res = append(res, g)
		default:
			res[first] = foldSection(res[first], g)
			if gi == len(groups)-1 {
				_, endBlanks = splitBlanks(g[len(g)-1].Lines)
			}
		}
	}
	out := slices.Concat(res...)
	if endBlanks != nil {
		out[len(out)-1] = withBlanks(out[len(out)-1], endBlanks)
	}
	for i := range out {
		out[i].Seq = i
	}
	return out
}

// sections groups entries: each heading entry with the entries under it.
func sections(ents []DocEntry) [][]DocEntry {
	var groups [][]DocEntry
	for _, e := range ents {
		if e.Kind == KindHeading || len(groups) == 0 {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], e)
	}
	return groups
}

// foldSection appends g's body to section a; a keeps its own trailing blanks.
func foldSection(a, g []DocEntry) []DocEntry {
	moved := slices.Clone(g[1:])
	if body, _ := splitBlanks(g[0].Lines[1:]); len(body) > 0 {
		moved = slices.Insert(moved, 0, DocEntry{Kind: KindBullet, Lines: slices.Clone(body)})
	}
	if len(moved) == 0 {
		return a
	}
	_, blanks := splitBlanks(a[len(a)-1].Lines)
	out := slices.Clone(a)
	out[len(out)-1] = withBlanks(out[len(out)-1], nil)
	out = append(out, moved...)
	out[len(out)-1] = withBlanks(out[len(out)-1], blanks)
	return out
}

func withBlanks(e DocEntry, blanks []string) DocEntry {
	body, _ := splitBlanks(e.Lines)
	e.Lines = slices.Concat(body, blanks)
	return e
}

// splitBlanks cuts the trailing blank lines off lines.
func splitBlanks(lines []string) (body, blanks []string) {
	n := len(lines)
	for n > 0 && strings.TrimSpace(lines[n-1]) == "" {
		n--
	}
	return lines[:n], lines[n:]
}
