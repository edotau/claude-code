// Holdings: the files each leaf wrote and still holds dirty, joined from the claims ledger, markers and dispatch records.

package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/inflight"
)

// Holdings is one read of who holds what in the repo at top; Free is the dirty paths no live leaf holds.
type Holdings struct {
	Free  []string
	top   string
	dirty *dirtyIndex
	files map[string][]string // "<session slug>/<agent id>" → absolute claimed paths
}

// ReadHoldings reads claims, markers and records and runs one git status of top ("" skips it); err joins failures.
func ReadHoldings(top string, now time.Time) (Holdings, error) {
	h := Holdings{top: top, files: map[string][]string{}}
	scanErr := ScanClaims(proc.Alive, func(_ int, slug string, c Claim) {
		if k := leafKey(slug, c.Agent); c.Agent != "" && !slices.Contains(h.files[k], c.Path) {
			h.files[k] = append(h.files[k], c.Path)
		}
	})
	var gitErr error
	h.dirty, gitErr = readDirty(top)
	held := map[string]bool{}
	for _, l := range inflight.LiveLeaves(now) {
		k := leafKey(transcriptSlug(l.Transcript), l.AgentID)
		for _, f := range h.files[k] {
			held[f] = true
		}
	}
	running, _ := agent.RunningRecent(agent.AllRecords) // every running dispatch holds its files, however old
	for _, r := range running {
		if r.Alive() && r.Wrote != nil {
			for _, f := range *r.Wrote {
				held[f] = true
			}
		}
	}
	h.Free = h.freeFiles(held)
	return h, errors.Join(scanErr, gitErr)
}

func leafKey(sessionSlug, agentID string) string { return sessionSlug + "/" + agentID }

func transcriptSlug(transcript string) string {
	return SessionSlug(strings.TrimSuffix(filepath.Base(transcript), ".jsonl"))
}

// SessionSlug is the file-name form of a session id that names its claims file and inbox.
func SessionSlug(session string) string { return paths.Slug(session, "", 80, "session") }

// Subagent is the still-dirty files subagent agentID of session wrote, repo-relative.
func (h Holdings) Subagent(session, agentID string) []string {
	return h.Keep(h.files[leafKey(SessionSlug(session), agentID)])
}

// Keep is the paths under top, repo-relative and, when the dirty set is known, only those still dirty; sorted.
func (h Holdings) Keep(files []string) []string {
	var out []string
	for _, f := range files {
		rel, inside := relTo(h.top, f)
		if h.top != "" && !inside {
			continue // another repo's file: this view is of top
		}
		if h.dirty == nil || h.dirty.has(rel) {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// freeFiles is the dirty paths no live leaf's claim covers; a dirty directory entry is free when none under it is held.
func (h Holdings) freeFiles(held map[string]bool) []string {
	if h.dirty == nil {
		return nil
	}
	claimed := map[string]bool{}
	for f := range held {
		rel, _ := relTo(h.top, f)
		claimed[rel] = true
	}
	var free []string
	for p := range h.dirty.files {
		if !claimed[p] {
			free = append(free, p)
		}
	}
	for _, d := range h.dirty.dirs {
		if !underAnyKey(claimed, d) {
			free = append(free, d)
		}
	}
	slices.Sort(free)
	return free
}

func underAnyKey(set map[string]bool, dir string) bool {
	for k := range set {
		if strings.HasPrefix(k, dir) {
			return true
		}
	}
	return false
}

// dirtyIndex is the working tree's dirty paths, repo-relative; a directory entry ends in "/".
type dirtyIndex struct {
	files map[string]bool
	dirs  []string
}

func (d *dirtyIndex) has(rel string) bool {
	return d.files[rel] || slices.ContainsFunc(d.dirs, func(p string) bool { return strings.HasPrefix(rel, p) })
}

// readDirty is one `git status` of top; nil (with the error) when it cannot run, which keeps every claim.
func readDirty(top string) (*dirtyIndex, error) {
	if top == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := proc.RunGitRaw(ctx, top, "status", "--porcelain", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	d := &dirtyIndex{files: map[string]bool{}}
	for e := range strings.SplitSeq(out, "\x00") {
		if len(e) < 4 {
			continue
		}
		if p := e[3:]; strings.HasSuffix(p, "/") {
			d.dirs = append(d.dirs, p)
		} else {
			d.files[p] = true
		}
	}
	return d, nil
}

// relTo is p repo-relative when it lies under top (symlinks resolved), else p unchanged.
func relTo(top, p string) (string, bool) {
	if top == "" || !filepath.IsAbs(p) {
		return p, false
	}
	for _, c := range []string{p, paths.Real(p)} {
		for _, t := range []string{top, paths.Real(top)} {
			if rel, err := filepath.Rel(t, c); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(rel), true
			}
		}
	}
	return p, false
}

// PeekNotices is session's undelivered notices of kind, oldest first, read without draining the inbox.
func PeekNotices(session, kind string) []Notice {
	raw, err := os.ReadFile(inbox(session))
	if err != nil {
		return nil
	}
	var out []Notice
	for line := range strings.SplitSeq(string(raw), "\n") {
		var n Notice
		if json.Unmarshal([]byte(line), &n) == nil && n.Kind == kind && n.Text != "" {
			out = append(out, n)
		}
	}
	return out
}

// OwnerSession is the slugged session id of Claude Code process pid, from its sessions/<pid>.json or a live marker.
func OwnerSession(pid int) string {
	if pid <= 0 {
		return ""
	}
	var reg struct {
		SessionID string `json:"sessionId"`
	}
	if b, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "sessions", strconv.Itoa(pid)+".json")); err == nil {
		_ = json.Unmarshal(b, &reg)
	}
	if reg.SessionID == "" {
		reg.SessionID = inflight.MarkerSession(pid)
	}
	if reg.SessionID == "" {
		return ""
	}
	return SessionSlug(reg.SessionID)
}

// sessionLive reports that session has a live registered Claude Code process or a live in-flight spawn marker.
func sessionLive(session string) bool {
	slug := SessionSlug(session)
	files, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "sessions", "*.json"))
	for _, f := range files {
		var e struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
			ProcStart string `json:"procStart"`
		}
		b, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(b, &e) != nil || e.SessionID == "" || e.PID <= 0 || SessionSlug(e.SessionID) != slug {
			continue
		}
		if proc.Alive(e.PID) && !startChanged(e.PID, e.ProcStart) {
			return true
		}
	}
	return slices.ContainsFunc(inflight.LiveLeaves(time.Now()), func(l inflight.Leaf) bool {
		return transcriptSlug(l.Transcript) == slug
	})
}

// startChanged reports that pid now runs a process other than the one started at want (a reused pid); unknown is false.
func startChanged(pid int, want string) bool {
	got, err := claimStart(pid)
	return want != "" && err == nil && strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(want), " ")
}

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
	if session == "" || !sessionLive(session) {
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
