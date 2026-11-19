package inflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// lockWait bounds the wait for the marker-dir lock; the hook itself is killed at 5 s.
var lockWait = 2 * time.Second

// DispatchPrefix starts the id of an `ask --batch` dispatch marker; its owner is the batch process.
const DispatchPrefix = "dispatch."

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

// Fits prunes and counts under the lock (unlocked and pruning too when the lock times out); writes nothing; fails open.
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

// TryClaim counts/writes under one lock
func TryClaim(c Claim, lim Limits) (bool, Snapshot) {
	dir := Dir()
	if c.Owner <= 0 || c.ID == "" || os.MkdirAll(dir, 0o700) != nil {
		return true, Snapshot{} // an unidentifiable owner or id writes nothing: no release could match it
	}
	var snap Snapshot
	ok := true
	ran, _ := paths.TryFileLock(filepath.Join(dir, ".lock"), lockWait, func() error {
		snap = scan(dir, time.Now(), true)
		if ok = c.fits(snap, lim); ok && !writeMarkers(dir, c) {
			Release(c.Owner, c.ID) // a failed write claims nothing: drop partial markers, fail open
		}
		return nil
	})
	if ran {
		return ok, snap
	}
	return claimWithoutLock(dir, c, lim)
}

func claimWithoutLock(dir string, c Claim, lim Limits) (bool, Snapshot) {
	if !writeMarkers(dir, c) {
		Release(c.Owner, c.ID)
		return true, Snapshot{} // nothing claimed: fail open like every other infra error
	}
	snap := scan(dir, time.Now(), true)
	snap.Total -= c.n()
	if c.Account != "" {
		snap.Per = maps.Clone(snap.Per)
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

func isLeafSuffix(s string) bool {
	d, ok := strings.CutPrefix(s, ".")
	if !ok || d == "" {
		return false
	}
	_, err := strconv.Atoi(d)
	return err == nil
}

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
		_ = os.Remove(tmp) // drop a partial temp file
		return false
	}
	if os.Rename(tmp, filepath.Join(dir, name)) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// readMarker splits a marker file into its account and session transcript; ok=false when it cannot be read.
func readMarker(dir, name string) (account, transcript string, mod time.Time, ok bool) {
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		return "", "", time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", "", time.Time{}, false
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", "", time.Time{}, false
	}
	account, transcript, _ = strings.Cut(string(b), "\n") // cut before trimming: the account line may be empty
	return strings.TrimSpace(account), strings.TrimSpace(transcript), fi.ModTime(), true
}

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
		account, transcript, mod, ok := readMarker(dir, e.Name())
		if !ok {
			continue // released between ReadDir and now
		}
		if !s.live(e.Name(), mod, transcript) {
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

// Sweep removes every dead marker under the lock and names them in ReadDir order; dryRun only names them, unlocked.
func Sweep(dryRun bool) ([]string, error) {
	dir := Dir()
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no marker dir yet: nothing was ever dispatched
		}
		return nil, fmt.Errorf("marker dir: %w", err)
	}
	if dryRun {
		return sweep(dir, true)
	}
	var removed []string
	ran, err := paths.TryFileLock(filepath.Join(dir, ".lock"), lockWait, func() error {
		var err error
		removed, err = sweep(dir, false)
		return err
	})
	if err == nil && !ran {
		err = errors.New("agent-inflight marker dir is locked; markers skipped")
	}
	return removed, err
}

// sweep is Sweep's pass: unlike scan it also removes a marker it cannot read, and reports a failed remove.
func sweep(dir string, dryRun bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	live := liveness(time.Now())
	var removed []string
	var errs []error
	for _, e := range entries {
		if _, ok := Owner(e.Name()); !ok || live(e.Name()) {
			continue
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, fmt.Errorf("marker %s: %w", e.Name(), err))
				}
				continue
			}
		}
		removed = append(removed, e.Name())
	}
	return removed, errors.Join(errs...)
}

type scanner struct {
	now     time.Time
	trusted bool
	meta    map[string]map[string]metaAgent // session transcript → tool_use_id → agent
	answers map[string]map[string]bool      // transcript → tool_use_ids it answered
}

func newScanner(now time.Time) *scanner {
	return &scanner{now: now, trusted: probeTrusted(), meta: map[string]map[string]metaAgent{}, answers: map[string]map[string]bool{}}
}

// live checks dispatch/sessionless markers by owner; spawn markers by agent metadata and transcript.
func (s *scanner) live(name string, mod time.Time, transcript string) bool {
	owner, _ := Owner(name)
	id := strings.TrimPrefix(name, Prefix+strconv.Itoa(owner)+"-")
	if transcript == "" || strings.HasPrefix(id, DispatchPrefix) {
		return ownerLive(name, mod, s.now, s.trusted)
	}
	if s.trusted && owner > 0 && !proc.Alive(owner) {
		return false
	}
	base := spawnBase(id)
	if a, ok := s.agents(transcript)[base]; ok {
		return s.agentLive(a, mod, transcript)
	}
	if s.now.Sub(mod) > settle && s.answered(transcript, base) {
		return false // refused or failed before an agent existed; settle covers a meta.json not yet written
	}
	return ownerLive(name, mod, s.now, s.trusted)
}

// spawnBase strips a trailing leaf suffix from a spawn marker id, leaving the tool_use_id.
func spawnBase(id string) string {
	if i := strings.LastIndex(id, "."); i >= 0 && isLeafSuffix(id[i:]) {
		return id[:i]
	}
	return id
}

// agentLive: a spawn marker with a meta.json lives while its agent is unfinished and its transcript is fresh.
func (s *scanner) agentLive(a metaAgent, mod time.Time, transcript string) bool {
	if a.finished || (!s.trusted && s.now.Sub(mod) > ttl) {
		return false // Claude Code marks a done agent's meta finished, SubagentStop or not
	}
	fi, err := os.Stat(filepath.Join(SubagentDir(transcript), "agent-"+a.id+".jsonl"))
	missing := os.IsNotExist(err) && s.now.Sub(mod) > grace
	idle := err == nil && s.now.Sub(fi.ModTime()) > Idle
	return !missing && !idle
}

// metaAgent is one agent-*.meta.json: the agent id and whether Claude Code marked it finished.
type metaAgent struct {
	id       string
	finished bool
}

func (s *scanner) agents(transcript string) map[string]metaAgent {
	if m, ok := s.meta[transcript]; ok {
		return m
	}
	m := map[string]metaAgent{}
	files, _ := filepath.Glob(filepath.Join(SubagentDir(transcript), "agent-*.meta.json"))
	mds, _ := parallel.Map(context.Background(), files, metaReaders, func(_ context.Context, f string) (meta, error) {
		if md, ok := readMeta(f); ok {
			return md, nil
		}
		return meta{}, nil
	})
	for i, md := range mds {
		if md.ToolUseID != "" {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(files[i]), "agent-"), ".meta.json")
			m[safeID(md.ToolUseID)] = metaAgent{id: id, finished: md.Finished}
		}
	}
	s.meta[transcript] = m
	return m
}

// metaReaders bounds the concurrent meta.json reads of one transcript's agents.
const metaReaders = 8

type meta struct {
	ToolUseID string `json:"toolUseId"`
	Finished  bool   `json:"finished"`
}

func readMeta(path string) (meta, bool) {
	var m meta
	b, err := os.ReadFile(path)
	return m, err == nil && json.Unmarshal(b, &m) == nil
}

// scanChunk is the read size of answeredIDs: memory is bounded by it plus one id, not by the transcript size.
const scanChunk = 64 << 10

// resultKey precedes every tool_result's id in a transcript line.
var resultKey = []byte(`"tool_use_id":"`)

// openTranscript opens a session transcript; tests count its calls.
var openTranscript = os.Open

// answered reports a tool_result for id in transcript; the file is read once per scanner for every marker on it.
func (s *scanner) answered(transcript, id string) bool {
	if id == "" {
		return false
	}
	ids, ok := s.answers[transcript]
	if !ok {
		ids = answeredIDs(transcript)
		s.answers[transcript] = ids
	}
	return ids[id]
}

// answeredIDs is every tool_use_id value in the file at path, read in bounded chunks; nil when it cannot be opened.
func answeredIDs(path string) map[string]bool {
	f, err := openTranscript(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	ids := map[string]bool{}
	buf := make([]byte, scanChunk)
	var pending []byte // the unprocessed tail: a partial key or a value still waiting for its closing quote
	for {
		n, err := f.Read(buf)
		pending = append(pending, buf[:n]...)
		pending = collectIDs(pending, ids)
		if err != nil {
			return ids
		}
	}
}

// collectIDs adds each complete key+value in b to ids and returns the tail that may still complete in the next chunk.
func collectIDs(b []byte, ids map[string]bool) []byte {
	for {
		i := bytes.Index(b, resultKey)
		if i < 0 {
			return b[max(len(b)-(len(resultKey)-1), 0):] // a key cut by the chunk edge starts inside this tail
		}
		end := bytes.IndexByte(b[i+len(resultKey):], '"')
		if end < 0 {
			return b[i:]
		}
		start := i + len(resultKey)
		ids[string(b[start:start+end])] = true
		b = b[start+end:] // the closing quote may open the next key
	}
}

// Base is the per-account cap on concurrent agents unless a profile's `concurrency` overrides it.
const Base = 24

// Limits are the caps: Total across the machine, Override per account key (else Base).
type Limits struct {
	Total    int
	Override map[string]int
}

// For is the cap of one account: its override, else Base.
func (l Limits) For(account string) int {
	if n := l.Override[account]; n > 0 {
		return n
	}
	return Base
}

// ComputeLimits: Total = min(Base × max(len(available), 1), 4 × cpus); pure.
func ComputeLimits(available []string, override map[string]int, cpus int) Limits {
	total := Base * max(len(available), 1)
	if cpus > 0 {
		total = min(total, 4*cpus)
	}
	return Limits{Total: total, Override: override}
}
