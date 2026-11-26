package docs

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// strikeToken names one task by plan slug and exact id, "strikes: <slug> <id> (<reason>)", so words never strike.
var strikeToken = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\bstrikes:\s+(\d{4}-\d{2}-\d{2}-[A-Za-z0-9._-]+?)(?:\.md)?\s+([^();]+?)\s*` +
		`(?:\((` + strings.Join(plans.StrikeReasons, "|") + `)\))?\s*(?:;|\.?\s*$)`)
})

// decidedReason is the strike reason of a decision that names none: the plan no longer applies to the task.
const decidedReason = "obsolete"

// decidedMark is the count of the session's recorded tag events already read for strikes.
func decidedMark(session string) string {
	return filepath.Join(paths.StateDir(), "plans", "decided", paths.Slug(session, "", 80, "session"))
}

// strikeDecided strikes each open task a recorded DECISION names with a strike token, once per event, citing it.
func strikeDecided(in *core.Input) []string {
	if in.SessionID == "" {
		return nil
	}
	events, mark := memory.ReadTagEvents(in.SessionID), decidedMark(in.SessionID)
	raw, _ := os.ReadFile(mark)
	done, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || done > len(events) { // a missing or rewritten log is read from its start
		done = 0
	}
	if done == len(events) {
		return nil
	}
	var notes []string
	for _, e := range events[done:] {
		if e.Event == "decision" {
			notes = append(notes, strikeDecision(in.CWD, in.SessionID, e.Text)...)
		}
	}
	if err := os.MkdirAll(filepath.Dir(mark), 0o700); err == nil {
		_ = paths.AtomicWrite(mark, []byte(strconv.Itoa(len(events))+"\n"), 0o600)
	}
	return notes
}

// strikeDecision strikes every task one decision's tokens name; each skip is a note, never an error.
func strikeDecision(cwd, session, text string) []string {
	var notes []string
	for _, m := range strikeToken().FindAllStringSubmatch(text, -1) {
		slug, id, reason := m[1], strings.Join(strings.Fields(m[2]), " "), cmp.Or(strings.ToLower(m[3]), decidedReason)
		if note := strikeNamed(cwd, slug, id, reason, "note:"+memory.TagEventsPath(session)); note != "" {
			notes = append(notes, note)
		}
	}
	return notes
}

// strikeNamed strikes the one open task of plan slug whose id is exactly id; "" when nothing needs saying.
func strikeNamed(cwd, slug, id, reason, evidence string) string {
	top, main := paths.CheckoutOf(cwd)
	path, err := plans.ResolvePlan(cmp.Or(main, top), slug)
	if err != nil {
		return fmt.Sprintf("plans: decision strikes %s %s: %v; struck nothing", slug, id, err)
	}
	p, err := plans.Parse(path)
	if err != nil {
		return fmt.Sprintf("plans: decision strikes %s %s: %v; struck nothing", slug, id, err)
	}
	var hits []plans.Task
	for _, t := range p.Tasks {
		if t.ID == id {
			hits = append(hits, t)
		}
	}
	switch {
	case len(hits) != 1:
		return fmt.Sprintf("plans: decision strikes %s %s: %d tasks have that exact id; struck nothing", slug, id, len(hits))
	case hits[0].Dropped:
		return ""
	}
	wrote, err := plans.StrikeWith(path, id, reason, evidence)
	switch {
	case err != nil:
		return fmt.Sprintf("plans: decision strikes %s %s: %v", slug, id, err)
	case wrote:
		return "plans: struck " + id + " in " + slug + " (" + reason + "), as a recorded decision retires it"
	}
	return ""
}
