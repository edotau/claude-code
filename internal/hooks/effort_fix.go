package hooks

import (
	"cmp"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
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
	if !changed || openQuote(line) || !plainCommand(strings.TrimPrefix(line, "Run:"), words) {
		return task, note + spanNote(line, changed)
	}
	out := task[:lead] + "Run: " + textutil.ShellJoin(append(slices.Clone(head), fixed...))
	if hasRest {
		out += "\n" + rest
	}
	return out, "dispatch-effort: fixed the brief's Run: line (" + strings.Join(notes, "; ") + ")"
}

func spanNote(line string, changed bool) string {
	if changed && openQuote(line) {
		return " (the command spans lines: left unchanged)"
	}
	return ""
}

// openQuote reports an unbalanced quote in line, so its command continues on the next line.
func openQuote(line string) bool {
	var q rune
	esc := false
	for _, r := range line {
		switch {
		case esc:
			esc = false
		case r == '\\' && q != '\'':
			esc = true
		case q == 0 && (r == '\'' || r == '"'):
			q = r
		case q == r:
			q = 0
		}
	}
	return q != 0
}

// plainCommand is one shell segment, no relay wrapper, no redirect or pipe: re-quoting it keeps its meaning.
func plainCommand(command string, words []string) bool {
	return len(shell.Segments(command)) == 1 && slices.Equal(shell.CommandWords(command)[0], words) &&
		!strings.ContainsAny(command, "<>|`$")
}

func (e *effortPlan) fixRunLine(field, task string) string {
	if e.mode == modeDeferred { // the mod's query makes the fix at spawn
		return task
	}
	fixed, note := fixRunLine(task)
	if fixed != task {
		e.updated[field], e.changed = fixed, true
	}
	e.fix(note)
	e.fix(toolsNote(fixed))
	return fixed
}

// toolMarkers in a brief's body mean the agent must run commands or edit files.
var toolMarkers = []string{"go test", "go build", "rtk ", "gofmt", "you own", "owns:", "pytest", "npm test", "make "}

// toolsNote advises when a brief that runs commands or edits files has an ask Run: line without --tools.
func toolsNote(task string) string {
	words := openingAskWords(task)
	if words == nil || slices.ContainsFunc(words, func(w string) bool {
		return strings.HasPrefix(w, "--tools") || strings.HasPrefix(w, "--permissions")
	}) {
		return ""
	}
	_, body, _ := strings.Cut(strings.TrimSpace(task), "\n")
	body = strings.ToLower(body)
	if !slices.ContainsFunc(toolMarkers, func(m string) bool { return strings.Contains(body, m) }) {
		return ""
	}
	return "dispatch-effort: Run: line has no --tools, so the agent gets no shell or edits; " +
		"add --tools --workdir <dir> if the brief needs them"
}

// RunLine is the brief's `Run:` line for ask argv, each word quoted for a shell.
func RunLine(argv []string) string {
	return "Run: " + textutil.ShellJoin(append([]string{"claude-code", "agents", "ask"}, argv...))
}

// OverlayArgs is the ask argv for a profile and a plain model: one --overlay, or --profile when unknown.
func OverlayArgs(profile, model, identity, account string) []string {
	t := overlayTarget(profile, model)
	switch t.Provider {
	case "databricks":
		t.Identity = identity
	case "github":
		t.Account = account
	case "":
		if t.Profile != "" { // no registry row: ask resolves the name and refuses it if unknown
			return append([]string{"--profile", t.Profile}, overlayFlag(route.Target{Model: t.Model})...)
		}
	}
	return overlayFlag(t)
}

// overlayTarget fills the provider from the profile's registry row; a bare provider key is its pool pick.
func overlayTarget(profile, model string) route.Target {
	reg, _ := providers.Load()
	t := route.Target{Route: route.ParseLegacy(profile), Model: model}
	if t.Provider != "" {
		t.Profile = cmp.Or(t.Profile, agentpkg.AutoModel)
	} else if reg != nil && t.Profile != "" {
		if p, err := reg.Get(t.Profile); err == nil {
			t.Provider = p.Provider
		}
	}
	return t
}

func overlayFlag(t route.Target) []string {
	if t.IsZero() && t.Model == "" {
		return nil
	}
	return []string{"--overlay", t.JSON()}
}
