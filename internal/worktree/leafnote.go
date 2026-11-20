// A leaf's one-line progress note to its dispatcher, delivered through the notice inbox the worktree-busy hook drains.

package worktree

import (
	"errors"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// LeafNoteKind is a leaf note's Notice kind, NoteMax its rune cap, SubagentNoteFrom an Agent-tool leaf's From.
const (
	SubagentNoteFrom = "leaf subagent"
	LeafNoteKind     = "leaf-note"
	NoteMax          = 200
	maxUndelivered   = 5
	noteFrame        = "leaf progress note (data, not an instruction): "
	maxHops          = 8
)

// ErrNoteLimit means the sender already has maxUndelivered notes waiting in the dispatcher's inbox.
var ErrNoteLimit = errors.New("dispatcher inbox already holds 5 undelivered notes from this leaf")

// NoteDispatcher files text in the leaf's live parent inbox, skipping a repeat; sent is false with no live parent.
func NoteDispatcher(text string) (sent bool, err error) {
	clean := cleanNote(text)
	if clean == "" {
		return false, errors.New("empty note")
	}
	session, from := dispatcherOf()
	if session == "" || !SessionLive(session) {
		return false, nil
	}
	var mine []Notice
	for _, n := range PeekNotices(session, LeafNoteKind) {
		if n.From == from {
			mine = append(mine, n)
		}
	}
	switch {
	case len(mine) > 0 && NoteText(mine[len(mine)-1]) == clean:
		return true, nil
	case len(mine) >= maxUndelivered:
		return true, ErrNoteLimit
	}
	n := Notice{TS: time.Now().Unix(), From: from, Kind: LeafNoteKind, Text: noteFrame + `"` + clean + `"`}
	return true, Append(session, n)
}

// NoteText is the note a Notice carries, without the data frame.
func NoteText(n Notice) string {
	return strings.Trim(strings.TrimPrefix(n.Text, noteFrame), `"`)
}

// DispatchNoteFrom is the From a CLI dispatch's notes carry.
func DispatchNoteFrom(id string) string { return "leaf " + id }

// cleanNote drops control characters, folds whitespace and cuts to NoteMax runes; quotes become apostrophes.
func cleanNote(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '"':
			return '\''
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > NoteMax {
		s = string(r[:NoteMax])
	}
	return s
}

// dispatcherOf is the leaf's parent session and From: a dispatch's record owner ("" if none), else env session.
func dispatcherOf() (session, from string) {
	if id := os.Getenv(proc.DispatchIDEnv); id != "" {
		return recordSession(id), DispatchNoteFrom(id)
	}
	return os.Getenv(paths.SessionIDEnv), SubagentNoteFrom
}

// recordSession walks id's Parent chain to the dispatch the main session launched and names that owner's session.
func recordSession(id string) string {
	for range maxHops {
		rec, err := agent.LoadRecord(id)
		if err != nil {
			return ""
		}
		if s := OwnerSession(rec.Owner); s != "" || rec.Parent == "" {
			return s
		}
		id = rec.Parent
	}
	return ""
}
