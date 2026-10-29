package memory

import (
	"regexp"
	"strings"
)

// Entry is one discrete memory item (the ADK MemoryEntry shape): a bank block with id, date and type metadata.
type Entry struct {
	ID         string
	Date       string // YYYY-MM-DD from the title, "" when the block carries none
	Type       string // decision | convention | progress | session | context | topic | knowledge
	Title      string
	Body       string // the whole block, id markers stripped
	Bank, File string
	Archived   bool
	Superseded bool
}

var (
	entryDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	anyIDMark = regexp.MustCompile(`\s*<!-- id:[^>]*-->`) // persisted 8-hex ids and alias ids alike
	// entryTypes maps a typed bank file to its entry type.
	entryTypes = map[string]string{
		"decisionLog.md": "decision", "conventions.md": "convention", "progress.md": "progress",
		"sessionHistory.md": "session", "activeContext.md": "context", "projectContext.md": "context",
	}
)

// EntryFromHit lifts a search hit to an Entry; a block without a persisted id gets its ContentRef.
func EntryFromHit(h SearchHit) Entry {
	first, _, _ := strings.Cut(h.Block, "\n")
	id := blockMarkerID(h.Block)
	if id == "" {
		if id = h.ID; id == "" {
			id = ContentRef(h.Block)
		}
	}
	typ := entryTypes[logicalName(h.File)]
	switch {
	case typ != "":
	case h.Bank == "knowledge":
		typ = "knowledge"
	default:
		typ = "topic"
	}
	title := strings.TrimLeft(anyIDMark.ReplaceAllString(first, ""), "#- ")
	return Entry{
		ID: id, Date: entryDate.FindString(title), Type: typ, Title: truncateLine(title),
		Body: strings.TrimSpace(anyIDMark.ReplaceAllString(h.Block, "")), Bank: h.Bank, File: h.File,
		Archived: h.Bank == "archive", Superseded: strings.Contains(title, "~~") || strings.Contains(title, "SUPERSEDED"),
	}
}

// blockMarkerID is the oldest persisted id marker outside code fences (settle stacks newer ones above it), "" if none.
func blockMarkerID(block string) string {
	lines := strings.Split(block, "\n")
	fenced := fencedLines(lines)
	id := ""
	for i, l := range lines {
		if fenced[i] {
			continue
		}
		if m := idMarker.FindAllStringSubmatch(l, -1); len(m) > 0 {
			id = m[len(m)-1][1]
		}
	}
	return id
}

// Pointer is the one-line reference `memory search <id>` expands: id · date · title.
func (e Entry) Pointer() string {
	date := e.Date
	if date == "" {
		date = "undated"
	}
	return e.ID + " · " + date + " · " + e.Title
}

// TypeRank orders entry types for recall: decisions and conventions first, session narrative last.
func (e Entry) TypeRank() int {
	switch e.Type {
	case "decision", "convention":
		return 0
	case "session":
		return 2
	}
	return 1
}
