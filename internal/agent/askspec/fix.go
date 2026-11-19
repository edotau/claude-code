package askspec

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// LeadingAgentWord is pos[0] when it is a roster agent name, --agent is untyped and no "--" precedes it.
func LeadingAgentWord(args, pos, roster []string, agentTyped bool) (string, bool) {
	if agentTyped || len(pos) == 0 || !slices.Contains(roster, pos[0]) {
		return "", false
	}
	if i := slices.Index(args, "--"); i >= 0 && len(pos) <= len(args)-i-1 {
		return "", false
	}
	return pos[0], true
}

// widening flags grant tools, permissions, routes or env; Fix never guesses them in or rewrites their values.
var widening = []string{"tools", "permissions", "allow", "ask", "autonomous", "host", "env", "keep-env",
	"overlay", "agents", "direct", "native", "safe-mode"}

// fixer is one Fix pass; firstPos and unresolved are word indexes, -1 for none.
type fixer struct {
	s                    Schema
	out, notes           []string
	agentTyped           bool
	firstPos, unresolved int
}

// Fix corrects ask args where exactly one correction exists; notes name each fix, or why a word stayed.
func (s Schema) Fix(args, roster []string) (fixed, notes []string, changed bool) {
	x := &fixer{s: s, out: slices.Clone(args), firstPos: -1, unresolved: -1}
	for i := 0; i < len(x.out) && x.out[i] != "--"; i++ {
		if w := x.out[i]; len(w) < 2 || w[0] != '-' {
			if x.firstPos < 0 {
				x.firstPos = i
			}
			continue
		}
		i = x.flag(i)
	}
	x.agentWord(roster)
	return x.out, x.notes, !slices.Equal(x.out, args)
}

// flag fixes the flag word at i and returns the index of the last word it consumed.
func (x *fixer) flag(i int) int {
	name, val, hasVal := strings.Cut(strings.TrimLeft(x.out[i], "-"), "=")
	f, ok := x.s.Lookup(name)
	if !ok {
		var note string
		at := i
		x.out, i, note, ok = x.s.fixUnknown(x.out, i, name, val, hasVal)
		if !ok {
			x.unresolved = at
		}
		fixed, _, _ := strings.Cut(strings.TrimLeft(x.out[at], "-"), "=")
		x.agentTyped = x.agentTyped || fixed == "agent"
		x.notes = append(x.notes, note)
		return i
	}
	x.agentTyped = x.agentTyped || name == "agent"
	switch {
	case hasVal:
		x.out[i], x.notes = fixChoice(f, x.out[i], val, true, x.notes)
	case f.Kind != "bool" && i+1 < len(x.out):
		i++
		x.out[i], x.notes = fixChoice(f, x.out[i], x.out[i], false, x.notes)
	}
	return i
}

// agentWord prefixes a leading roster name with --agent unless it may be an unresolved flag's value.
func (x *fixer) agentWord(roster []string) {
	p := x.firstPos
	if p < 0 || x.agentTyped || !slices.Contains(roster, x.out[p]) {
		return
	}
	if x.unresolved >= 0 && p == x.unresolved+1 {
		x.notes = append(x.notes, x.out[p]+" may be the value of "+x.out[x.unresolved]+" (left unchanged)")
		return
	}
	x.notes = append(x.notes, x.out[p]+" → --agent "+x.out[p])
	x.out = slices.Insert(x.out, p, "--agent")
}

// fixUnknown handles an unknown flag at out[i]; ok is false when it stayed unchanged.
func (s Schema) fixUnknown(out []string, i int, name, val string, hasVal bool) ([]string, int, string, bool) {
	if name == "task-file" {
		return fixTaskFile(out, i, val, hasVal)
	}
	n, ok := textutil.Nearest(name, s.Names())
	if !ok {
		return out, i, "unknown --" + name + ": no single near flag (left unchanged)", false
	}
	if slices.Contains(widening, n) {
		return out, i, "--" + name + " is near the permission flag --" + n + " (left unchanged; fix by hand)", false
	}
	out[i] = "--" + n
	if hasVal {
		out[i] += "=" + val
	}
	if f, _ := s.Lookup(n); f.Kind != "bool" && !hasVal {
		i++ // the next word is the corrected flag's value
	}
	return out, i, "--" + name + " → --" + n, true
}

// fixTaskFile turns --task-file - (or --task-file=-) into -; any other file is left for the brief's author.
func fixTaskFile(out []string, i int, val string, hasVal bool) ([]string, int, string, bool) {
	if !hasVal && i+1 < len(out) && out[i+1] == "-" {
		out = slices.Delete(out, i+1, i+2)
		val, hasVal = "-", true
	}
	if !hasVal || val != "-" {
		return out, i, "ask has no --task-file: give the task as - < FILE (left unchanged)", false
	}
	out[i] = "-"
	return out, i, "--task-file - → - (ask reads the task from stdin)", true
}

// fixChoice swaps a value outside f.Choices for its unique near choice; inline means word is --name=value.
func fixChoice(f Field, word, val string, inline bool, notes []string) (string, []string) {
	if f.CheckChoice(val) == nil {
		return word, notes
	}
	if slices.Contains(widening, f.Name) {
		return word, append(notes, fmt.Sprintf("--%s %q is not valid (left unchanged; fix by hand)", f.Name, val))
	}
	n, ok := textutil.Nearest(val, f.Choices)
	if !ok {
		return word, append(notes, fmt.Sprintf("--%s %q is not one of %s (left unchanged)",
			f.Name, val, strings.Join(f.Choices, "|")))
	}
	notes = append(notes, fmt.Sprintf("--%s %s → %s", f.Name, val, n))
	if inline {
		return "--" + f.Name + "=" + n, notes
	}
	return n, notes
}
