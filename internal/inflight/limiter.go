package inflight

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// lockWait bounds the wait for the marker-dir lock; the hook itself is killed at 5 s.
var lockWait = 2 * time.Second

// DispatchPrefix starts the id of an `ask --batch` dispatch marker; its owner is the batch process.
const DispatchPrefix = "leg."

// Claim asks for N slots (default 1) on Account for one id owned by Owner; Transcript feeds the prune.
type Claim struct {
	Owner       int
	ID, Account string
	N           int
	Transcript  string
}

// Snapshot is a live count: Total markers, and Per account (by marker content).
type Snapshot struct {
	Total int
	Per   map[string]int
}

func (c Claim) n() int { return max(c.N, 1) }

func (c Claim) fits(s Snapshot, lim Limits) bool {
	if s.Total+c.n() > lim.Total {
		return false
	}
	return c.Account == "" || s.Per[c.Account]+c.n() <= lim.For(c.Account)
}

// Fits prunes and counts under the lock and writes nothing; true also on any infra error (fail open).
func Fits(c Claim, lim Limits) (bool, Snapshot) {
	dir := Dir()
	if os.MkdirAll(dir, 0o700) != nil {
		return true, Snapshot{}
	}
	var snap Snapshot
	ok := true
	ran, _ := paths.TryFileLock(filepath.Join(dir, ".lock"), lockWait, func() error {
		snap = scan(dir, time.Now(), true)
		ok = c.fits(snap, lim)
		return nil
	})
	if !ran {
		snap = scan(dir, time.Now(), true)
		ok = c.fits(snap, lim)
	}
	return ok, snap
}

// TryClaim prunes, counts and writes the markers in ONE locked section; on lock contention it writes first,
// counts second and removes its own markers when over cap. The Snapshot excludes this claim.
func TryClaim(c Claim, lim Limits) (bool, Snapshot) {
	dir := Dir()
	if c.Owner <= 0 || os.MkdirAll(dir, 0o700) != nil {
		return true, Snapshot{} // an unidentifiable owner writes nothing and retires nothing
	}
	if c.ID == "" {
		c.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	var snap Snapshot
	ok := true
	ran, _ := paths.TryFileLock(filepath.Join(dir, ".lock"), lockWait, func() error {
		snap = scan(dir, time.Now(), true)
		if ok = c.fits(snap, lim); ok {
			_ = writeMarkers(dir, c) // a failed write claims nothing: fail open
		}
		return nil
	})
	if ran {
		return ok, snap
	}
	if !writeMarkers(dir, c) {
		Release(c.Owner, c.ID)
		return true, Snapshot{} // nothing claimed: fail open like every other infra error
	}
	snap = scan(dir, time.Now(), true)
	snap.Total -= c.n()
	if c.Account != "" {
		snap.Per = cloneCounts(snap.Per)
		snap.Per[c.Account] -= c.n()
	}
	if !c.fits(snap, lim) {
		Release(c.Owner, c.ID)
		return false, snap
	}
	return true, snap
}

// Release removes the id's marker and its `.<i>` siblings.
func Release(owner int, id string) {
	entries, err := os.ReadDir(Dir())
	if err != nil || id == "" {
		return
	}
	base := markerName(owner, id)
	for _, e := range entries {
		if rest, ok := strings.CutPrefix(e.Name(), base); ok && (rest == "" || isLeafSuffix(rest)) {
			_ = os.Remove(filepath.Join(Dir(), e.Name()))
		}
	}
}

// ToolUseID reads the toolUseId Claude Code recorded for agentID; "" when there is no meta.json.
func ToolUseID(subagentDir, agentID string) string {
	if subagentDir == "" || agentID == "" {
		return ""
	}
	m, _ := readMeta(filepath.Join(subagentDir, "agent-"+safeID(agentID)+".meta.json"))
	return m.ToolUseID
}

// RetireAgent releases the slot of the stopping agent; no meta.json retires nothing.
func RetireAgent(owner int, subagentDir, agentID string) {
	if id := ToolUseID(subagentDir, agentID); id != "" {
		Release(owner, id)
	}
}

// Count is the read-only live count (nothing is pruned), for doctor.
func Count() Snapshot {
	return scan(Dir(), time.Now(), false)
}

func markerName(owner int, id string) string {
	return Prefix + strconv.Itoa(owner) + "-" + safeID(id)
}

// safeID keeps [A-Za-z0-9_.-]: the id is model-supplied and lands in a path, where `../` escapes the dir.
func safeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_', r == '.', r == '-':
			return r
		}
		return '_'
	}, id)
}

// isLeafSuffix is ".<digits>": a Workflow leaf index.
func isLeafSuffix(s string) bool {
	d, ok := strings.CutPrefix(s, ".")
	if !ok || d == "" {
		return false
	}
	_, err := strconv.Atoi(d)
	return err == nil
}

// writeMarkers lands every marker of the claim; false when any write failed.
func writeMarkers(dir string, c Claim) bool {
	body := c.Account
	if c.Transcript != "" {
		body += "\n" + c.Transcript // any scanner can then apply the spawn rules to this marker
	}
	if c.n() == 1 {
		return writeMarker(dir, markerName(c.Owner, c.ID), body)
	}
	ok := true
	for i := 1; i <= c.n(); i++ {
		ok = writeMarker(dir, markerName(c.Owner, c.ID)+"."+strconv.Itoa(i), body) && ok
	}
	return ok
}

// writeMarker lands the content atomically (temp + rename): a reader never sees a half-written body.
func writeMarker(dir, name, body string) bool {
	tmp := filepath.Join(dir, ".tmp-"+name)
	if os.WriteFile(tmp, []byte(body), 0o600) != nil {
		return false
	}
	if os.Rename(tmp, filepath.Join(dir, name)) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

func cloneCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m)+1)
	maps.Copy(out, m)
	return out
}

// scan counts live markers, removing dead ones when prune.
func scan(dir string, now time.Time, prune bool) Snapshot {
	snap := Snapshot{Per: map[string]int{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return snap
	}
	s := newScanner(now)
	for _, e := range entries {
		if _, ok := Owner(e.Name()); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // released between ReadDir and now
		}
		account, transcript, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
		if !s.live(e.Name(), fi.ModTime(), transcript) {
			if prune {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
			continue
		}
		snap.Total++
		if account != "" {
			snap.Per[account]++
		}
	}
	return snap
}

// scanner caches each session's meta.json map and transcript across one scan.
type scanner struct {
	now      time.Time
	trusted  bool
	meta     map[string]map[string]metaAgent // session transcript → tool_use_id → agent
	sessions map[string][]byte
}

func newScanner(now time.Time) *scanner {
	return &scanner{now: now, trusted: ProbeTrusted(), meta: map[string]map[string]metaAgent{}, sessions: map[string][]byte{}}
}

// live: dispatches and markers without a session follow ownerLive; a spawn marker follows its agent's meta.json and transcript.
func (s *scanner) live(name string, mod time.Time, transcript string) bool {
	owner, _ := Owner(name)
	id := strings.TrimPrefix(name, Prefix+strconv.Itoa(owner)+"-")
	if transcript == "" || strings.HasPrefix(id, DispatchPrefix) {
		return ownerLive(name, mod, s.now, s.trusted)
	}
	if s.trusted && owner > 0 && !proc.Alive(owner) {
		return false
	}
	base := id
	if i := strings.LastIndex(id, "."); i >= 0 && isLeafSuffix(id[i:]) {
		base = id[:i]
	}
	if a, ok := s.agents(transcript)[base]; ok {
		if a.finished || (!s.trusted && s.now.Sub(mod) > TTL) {
			return false // Claude Code marks a done agent's meta finished, SubagentStop or not
		}
		fi, err := os.Stat(filepath.Join(SubagentDir(transcript), "agent-"+a.id+".jsonl"))
		missing := os.IsNotExist(err) && s.now.Sub(mod) > Grace
		idle := err == nil && s.now.Sub(fi.ModTime()) > Idle
		return !missing && !idle
	}
	if s.now.Sub(mod) > Grace && s.answered(transcript, base) {
		return false // refused or failed before an agent existed; Grace covers a meta.json not yet written
	}
	return ownerLive(name, mod, s.now, s.trusted)
}

// metaAgent is one agent-*.meta.json: the agent id and whether Claude Code marked it finished.
type metaAgent struct {
	id       string
	finished bool
}

// agents maps tool_use_id → agent from one session's agent-*.meta.json files.
func (s *scanner) agents(transcript string) map[string]metaAgent {
	if m, ok := s.meta[transcript]; ok {
		return m
	}
	m := map[string]metaAgent{}
	files, _ := filepath.Glob(filepath.Join(SubagentDir(transcript), "agent-*.meta.json"))
	for _, f := range files {
		if md, ok := readMeta(f); ok && md.ToolUseID != "" {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "agent-"), ".meta.json")
			m[safeID(md.ToolUseID)] = metaAgent{id: id, finished: md.Finished}
		}
	}
	s.meta[transcript] = m
	return m
}

type meta struct {
	ToolUseID string `json:"toolUseId"`
	Finished  bool   `json:"finished"`
}

func readMeta(path string) (meta, bool) {
	var m meta
	b, err := os.ReadFile(path)
	return m, err == nil && json.Unmarshal(b, &m) == nil
}

// answered: the session transcript holds a tool_result for this spawn (refused or finished without a PostToolUse).
func (s *scanner) answered(transcript, id string) bool {
	b, ok := s.sessions[transcript]
	if !ok {
		b, _ = os.ReadFile(transcript)
		s.sessions[transcript] = b
	}
	return id != "" && bytes.Contains(b, []byte(`"tool_use_id":"`+id+`"`))
}
