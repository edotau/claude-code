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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// Holdings is one read of who holds what in the repo at top; Free is the dirty paths no live leaf holds.
type Holdings struct {
	Free  []string
	top   string
	dirty *dirtyIndex
	files map[string][]string // "<session slug>/<agent id>" → absolute claimed paths
}

const holderLiveRecords = 100

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
	for _, r := range agent.RecentRecords(holderLiveRecords) {
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

// SessionLive reports that session has a live registered Claude Code process or a live in-flight spawn marker.
func SessionLive(session string) bool {
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
