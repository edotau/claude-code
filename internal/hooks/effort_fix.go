package hooks

import (
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// fixRunLine rewrites the brief's opening ask Run: line where askspec.Fix is certain; note says what changed or why not.
func fixRunLine(task string) (string, string) {
	s, ok := askspec.Current()
	words := openingAskWords(task)
	if !ok || len(words) < 3 {
		return task, ""
	}
	verb := 1
	if words[1] == "--remote" {
		verb = 3
	}
	if len(words) < verb+2 {
		return task, ""
	}
	head, args := words[:verb+2], words[verb+2:]
	fixed, notes, changed := s.Fix(args, agentpkg.RosterNames())
	if len(notes) == 0 {
		return task, ""
	}
	note := "dispatch-effort: Run: line: " + strings.Join(notes, "; ")
	lead := len(task) - len(strings.TrimLeft(task, " \t\r\n"))
	line, rest, hasRest := strings.Cut(task[lead:], "\n")
	if !changed || !plainCommand(strings.TrimPrefix(line, "Run:"), words) {
		return task, note
	}
	out := task[:lead] + "Run: " + textutil.ShellJoin(append(slices.Clone(head), fixed...))
	if hasRest {
		out += "\n" + rest
	}
	return out, "dispatch-effort: fixed the brief's Run: line (" + strings.Join(notes, "; ") + ")"
}

// plainCommand is one shell segment, no relay wrapper, no redirect or pipe: re-quoting it keeps its meaning.
func plainCommand(command string, words []string) bool {
	return len(shell.Segments(command)) == 1 && slices.Equal(shell.CommandWords(command)[0], words) &&
		!strings.ContainsAny(command, "<>|`$")
}

func (e *effortPlan) fixRunLine(field, task string) string {
	fixed, note := fixRunLine(task)
	e.fix(note)
	if fixed != task {
		e.updated[field], e.changed = fixed, true
	}
	return fixed
}
