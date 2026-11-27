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

// decidedMark holds "<count> <size>": the session's tag events already read for strikes and the log's size then.
func decidedMark(session string) string {
	return filepath.Join(paths.StateDir(), "plans", "decided", paths.Slug(session, "", 80, "session"))
}

// strikeDecided strikes each open task a recorded DECISION names with a strike token, once per event, citing it.
func strikeDecided(in *core.Input) []string {
	if in.SessionID == "" {
		return nil
	}
	mark := decidedMark(in.SessionID)
	raw, _ := os.ReadFile(mark)
	fields, size := strings.Fields(string(raw)), tagLogSize(in.SessionID)
	if len(fields) == 2 && fields[1] == strconv.FormatInt(size, 10) {
		return nil // the log has not changed since the mark was written
	}
	events := memory.ReadTagEvents(in.SessionID)
	done := markedCount(fields, len(events))
	var notes []string
	for _, e := range events[done:] {
		if e.Event == "decision" {
			notes = append(notes, strikeDecision(in.CWD, in.SessionID, e.Text)...)
		}
	}
	if line := strconv.Itoa(len(events)) + " " + strconv.FormatInt(size, 10) + "\n"; line != string(raw) {
		if os.MkdirAll(filepath.Dir(mark), 0o700) == nil {
			_ = paths.AtomicWrite(mark, []byte(line), 0o600)
		}
	}
	return notes
}

// tagLogSize is the session's tag log size in bytes, -1 when it does not exist.
func tagLogSize(session string) int64 {
	if st, err := os.Stat(memory.TagEventsPath(session)); err == nil {
		return st.Size()
	}
	return -1
}

// markedCount is the event count a mark holds; 0 (read from the start) when absent, bad or past the log's end.
func markedCount(fields []string, have int) int {
	if len(fields) == 0 {
		return 0
	}
	if n, err := strconv.Atoi(fields[0]); err == nil && n <= have {
		return n
	}
	return 0
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
