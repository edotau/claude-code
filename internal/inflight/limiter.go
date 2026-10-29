package inflight

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// lockWait bounds the wait for the marker-dir lock; the hook itself is killed at 5 s.
var lockWait = 2 * time.Second

// LegPrefix starts the id of an `ask --batch` leg marker; its owner is the batch process.
const LegPrefix = "leg."

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
		snap = scan(dir, c.Owner, c.Transcript, time.Now(), true)
		ok = c.fits(snap, lim)
		return nil
	})
	if !ran {
		snap = scan(dir, c.Owner, c.Transcript, time.Now(), true)
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
		snap = scan(dir, c.Owner, c.Transcript, time.Now(), true)
		if ok = c.fits(snap, lim); ok {
			writeMarkers(dir, c)
		}
		return nil
	})
	if ran {
		return ok, snap
	}
	writeMarkers(dir, c)
	snap = scan(dir, c.Owner, c.Transcript, time.Now(), true)
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

// Acquire polls TryClaim until it wins or ctx ends; release removes the claim.
func Acquire(ctx context.Context, c Claim, lim Limits, poll time.Duration) (release func(), err error) {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	if c.ID == "" {
		c.ID = LegPrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	for {
		if ok, _ := TryClaim(c, lim); ok {
			return func() { Release(c.Owner, c.ID) }, nil
		}
		select {
		case <-ctx.Done():
			return func() {}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ToolUseID reads the toolUseId Claude Code recorded for agentID; "" when there is no meta.json.
func ToolUseID(subagentDir, agentID string) string {
	if subagentDir == "" || agentID == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(subagentDir, "agent-"+safeID(agentID)+".meta.json"))
	if err != nil {
		return ""
	}
	var m struct {
		ToolUseID string `json:"toolUseId"`
	}
	_ = json.Unmarshal(b, &m)
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
	return scan(Dir(), 0, "", time.Now(), false)
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

func writeMarkers(dir string, c Claim) {
	if c.n() == 1 {
		writeMarker(dir, markerName(c.Owner, c.ID), c.Account)
		return
	}
	for i := 1; i <= c.n(); i++ {
		writeMarker(dir, markerName(c.Owner, c.ID)+"."+strconv.Itoa(i), c.Account)
	}
}

// writeMarker lands the content atomically (temp + rename): a reader never sees a half-written account key.
func writeMarker(dir, name, account string) {
	tmp := filepath.Join(dir, ".tmp-"+name)
	if os.WriteFile(tmp, []byte(account), 0o600) != nil {
		return
	}
	if os.Rename(tmp, filepath.Join(dir, name)) != nil {
		_ = os.Remove(tmp)
	}
}

func cloneCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// scan counts live markers (removing dead ones when prune); self/transcript enable the spawn-marker rules.
func scan(dir string, self int, transcript string, now time.Time, prune bool) Snapshot {
	snap := Snapshot{Per: map[string]int{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return snap
	}
	s := &scanner{self: self, transcript: transcript, subDir: SubagentDir(transcript), now: now, trusted: ProbeTrusted()}
	for _, e := range entries {
		if _, ok := Owner(e.Name()); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if !s.live(e.Name(), fi.ModTime()) {
			if prune {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
			continue
		}
		snap.Total++
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			if acct := strings.TrimSpace(string(b)); acct != "" {
				snap.Per[acct]++
			}
		}
	}
	return snap
}

type scanner struct {
	self        int
	transcript  string
	subDir      string
	now         time.Time
	trusted     bool
	meta        map[string]string // tool_use_id → agent id, read lazily
	session     []byte
	sessionRead bool
}

// live applies the prune rules: others' markers and legs follow Live; self's spawn markers follow the agent transcript.
func (s *scanner) live(name string, mod time.Time) bool {
	owner, _ := Owner(name)
	id := strings.TrimPrefix(name, Prefix+strconv.Itoa(owner)+"-")
	if owner != s.self || s.self <= 0 || strings.HasPrefix(id, LegPrefix) {
		return Live(name, mod, s.now, s.trusted)
	}
	base := id
	if i := strings.LastIndex(id, "."); i >= 0 && isLeafSuffix(id[i:]) {
		base = id[:i]
	}
	if agent, ok := s.agents()[base]; ok {
		if !s.trusted && s.now.Sub(mod) > TTL {
			return false
		}
		fi, err := os.Stat(filepath.Join(s.subDir, "agent-"+agent+".jsonl"))
		missing := os.IsNotExist(err) && s.now.Sub(mod) > Grace
		idle := err == nil && s.now.Sub(fi.ModTime()) > Idle
		return !missing && !idle
	}
	if s.answered(base) {
		return false // refused or failed before an agent existed
	}
	return Live(name, mod, s.now, s.trusted)
}

// agents maps tool_use_id → agent id from the session's agent-*.meta.json files.
func (s *scanner) agents() map[string]string {
	if s.meta != nil {
		return s.meta
	}
	s.meta = map[string]string{}
	if s.subDir == "" {
		return s.meta
	}
	files, _ := filepath.Glob(filepath.Join(s.subDir, "agent-*.meta.json"))
	for _, f := range files {
		agent := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "agent-"), ".meta.json")
		if id := ToolUseID(s.subDir, agent); id != "" {
			s.meta[safeID(id)] = agent
		}
	}
	return s.meta
}

// answered: the session transcript holds a tool_result for this spawn (refused or finished without a PostToolUse).
func (s *scanner) answered(id string) bool {
	if s.transcript == "" || id == "" {
		return false
	}
	if !s.sessionRead {
		s.session, _ = os.ReadFile(s.transcript)
		s.sessionRead = true
	}
	return bytes.Contains(s.session, []byte(`"tool_use_id":"`+id+`"`))
}
