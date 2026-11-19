// Per-session file channels: the claims ledger and notice inboxes, both read by the hooks.

package worktree

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

// Claim is one line of the hooks' claims ledger (written by internal/hooks/wthook/claims.go): a path.
type Claim struct {
	Path       string `json:"path,omitempty"`
	Agent      string `json:"agent_id,omitempty"`
	TS         int64  `json:"ts"`
	OwnerStart string `json:"owner_start,omitempty"` // proc.StartTime of the owner pid: tells a reused pid apart
}

// claimStart reads a pid's start token; a var seam so tests play a reused, vanished or unreadable owner.
var claimStart = proc.StartTime

// retireWait bounds a scan's wait on a stale claims file's lock; a busy lock leaves the file for the next scan.
const retireWait = 100 * time.Millisecond

// claimTail is how much of each claims file a scan reads: the newest lines are the ones that matter.
const claimTail = 1 << 20

// ClaimsDir holds the claims ledger, one <owner pid>-<session slug>.jsonl per session.
func ClaimsDir() string { return filepath.Join(paths.StateDir(), "hooks", "claims") }

// ScanClaims calls fn per claim of a live owner (a dead owner's file is removed); scan errors are joined.
func ScanClaims(alive func(int) bool, fn func(pid int, slug string, c Claim)) error {
	entries, err := os.ReadDir(ClaimsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		pid, slug, ok := claimsOwner(e.Name())
		if !ok {
			continue
		}
		file := filepath.Join(ClaimsDir(), e.Name())
		if !alive(pid) {
			_ = os.Remove(file)
			_ = os.Remove(file + ".lock")
			continue
		}
		claims, stale, err := ownedClaims(file, pid)
		if err != nil {
			errs = append(errs, err)
		}
		if stale {
			retire(file, pid)
		}
		for _, c := range claims {
			fn(pid, slug, c)
		}
	}
	return errors.Join(errs...)
}

// ownedClaims is file's claims minus those a dead process at a now-reused pid wrote; stale means none are left.
func ownedClaims(file string, pid int) ([]Claim, bool, error) {
	var all []Claim
	if err := scanFile(file, func(c Claim) { all = append(all, c) }); err != nil {
		return all, false, err // a partial read may miss the newest token
	}
	newest := ""
	for _, c := range all {
		if c.OwnerStart != "" {
			newest = c.OwnerStart
		}
	}
	if newest == "" || !reused(pid, newest) {
		return all, false, nil // the newest writer still holds pid: every older line stays too
	}
	kept := slices.DeleteFunc(all, func(c Claim) bool { return c.OwnerStart != "" }) // a line with no token stays
	return kept, len(kept) == 0, nil
}

// reused reports that pid is gone or now another process than the one started at token; any probe error is false.
func reused(pid int, token string) bool {
	got, err := claimStart(pid)
	return errors.Is(err, proc.ErrNoProcess) || (err == nil && got != token)
}

// retire removes a stale claims file under its lock, re-checked there so a just-appended live claim survives.
func retire(file string, pid int) {
	removed := false
	_ = paths.WithFileLockWithin(file, retireWait, func() error {
		if _, stale, err := ownedClaims(file, pid); err != nil || !stale {
			return err
		}
		removed = os.Remove(file) == nil
		return nil
	})
	if removed {
		_ = os.Remove(file + ".lock")
	}
}

// claimsOwner parses <pid>-<slug>.jsonl.
func claimsOwner(name string) (int, string, bool) {
	base, isLog := strings.CutSuffix(name, ".jsonl")
	pidText, slug, ok := strings.Cut(base, "-")
	pid, err := strconv.Atoi(pidText)
	return pid, slug, isLog && ok && err == nil
}

// scanFile decodes the last claimTail bytes of file line by line; a file already removed is no error.
func scanFile(file string, fn func(Claim)) error {
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > claimTail {
		if _, err := f.Seek(-claimTail, io.SeekEnd); err != nil {
			return err
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), claimTail)
	for sc.Scan() {
		var c Claim
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.Path != "" {
			// the first line after a tail seek is usually torn and fails to decode
			fn(c)
		}
	}
	return sc.Err()
}

// ClaimHit is one live claim on a path by someone other than the caller: who, and their newest edit's time.
type ClaimHit struct {
	Who string
	TS  int64
}

// ClaimSource is LiveClaims's caller-specific inputs: liveness probe, own session slug and pid, leaf and label namers.
type ClaimSource struct {
	Alive func(int) bool
	Self  string
	Owner func() int
	Leaf  func(Claim) string
	Label func(pid int) string
	// Dispatch is a dispatched CLI leaf's id: a claim stamped with it, or filed by its own CLI process, is its own.
	Dispatch string
}

// LiveClaims maps each path other live parties claim to its claimants; a dead owner pid's file is removed.
func LiveClaims(src ClaimSource) (map[string][]ClaimHit, error) {
	own := sync.OnceValue(src.Owner) // looked up once, and only when another session's file is seen
	labels := map[int]string{}
	byPath := map[string]map[string]int64{}
	err := ScanClaims(src.Alive, func(pid int, slug string, c Claim) {
		if c.Path == "" || src.Dispatch != "" && (c.Agent == src.Dispatch || ownLine(pid, c, own)) {
			return
		}
		w := src.Leaf(c) // own session: only a sibling leaf's claim counts
		// our own pid under another slug is /clear or a resume, not a peer
		if slug != src.Self && pid != own() {
			if labels[pid] == "" {
				labels[pid] = src.Label(pid)
			}
			w = labels[pid] // another session: every claim counts while its owner lives
		}
		if w == "" {
			return
		}
		if byPath[c.Path] == nil {
			byPath[c.Path] = map[string]int64{}
		}
		byPath[c.Path][w] = max(byPath[c.Path][w], c.TS)
	})
	return flattenClaims(byPath), err
}

// ownLine: an unstamped claim the CLI process this caller runs under filed, still holding that pid (not a reuse).
func ownLine(pid int, c Claim, own func() int) bool {
	return c.Agent == "" && pid == own() && (c.OwnerStart == "" || !reused(pid, c.OwnerStart))
}

func flattenClaims(byPath map[string]map[string]int64) map[string][]ClaimHit {
	out := make(map[string][]ClaimHit, len(byPath))
	for p, whos := range byPath {
		for w, ts := range whos {
			out[p] = append(out[p], ClaimHit{Who: w, TS: ts})
		}
		slices.SortFunc(out[p], func(a, b ClaimHit) int { return strings.Compare(a.Who, b.Who) })
	}
	return out
}

// ReadDirty is top's dirty paths (absolute, untracked included; a rename source too), limited to scope when given.
func ReadDirty(ctx context.Context, top string, scope ...string) (string, map[string]bool, error) {
	args := append([]string{"status", "--porcelain", "-z", "--untracked-files=all"}, scope...)
	raw, err := proc.RunGit(ctx, top, args...)
	if err != nil {
		return "", nil, fmt.Errorf("git status in %s: %w", top, err)
	}
	return top, DirtyMap(top, proc.StatusEntries(raw)), nil
}

// DirtyMap is the entries' absolute paths; a rename or copy source is dirty too.
func DirtyMap(top string, entries []proc.StatusEntry) map[string]bool {
	dirty := map[string]bool{}
	for _, e := range entries {
		dirty[filepath.Join(top, e.Path)] = true
		if e.From != "" {
			dirty[filepath.Join(top, e.From)] = true
		}
	}
	return dirty
}

// FreshHits keeps p's claims when p is dirty, dropping those older than p's last commit or the clean mark cleanAt.
func FreshHits(ctx context.Context, top, p string, dirty map[string]bool, claims []ClaimHit, cleanAt int64) ([]ClaimHit, error) {
	if top == "" || !dirty[p] {
		return nil, nil
	}
	out, _, err := core.Git(ctx, top, "log", "-1", "--format=%ct", "--", p)
	if err != nil {
		return nil, err
	}
	var landed int64
	if len(out) > 0 {
		landed, _ = strconv.ParseInt(out[0], 10, 64)
	}
	landed = max(landed, cleanAt)
	return slices.DeleteFunc(slices.Clone(claims), func(h ClaimHit) bool { return h.TS < landed }), nil
}

// holders maps each claimed path to its session or leaf label; on a scan error it still returns what it read.
func holders(alive func(int) bool, want []string) (map[string][]string, error) {
	set := map[string]map[string]bool{}
	for _, p := range want {
		set[p] = map[string]bool{}
	}
	err := ScanClaims(alive, func(pid int, slug string, c Claim) {
		if who, ok := set[c.Path]; ok {
			who[label(pid, slug, c.Agent)] = true
		}
	})
	out := map[string][]string{}
	for p, who := range set {
		for w := range who {
			out[p] = append(out[p], w)
		}
	}
	return out, err
}

// label names a claimant from its claims file name and the line's agent id.
func label(pid int, slug, agent string) string {
	s := "session " + slug[:min(8, len(slug))] + " (pid " + strconv.Itoa(pid) + ")"
	if agent != "" {
		return "leaf " + agent[:min(8, len(agent))] + " of " + s
	}
	return s
}

// Notice is one inbox line: when, who sent it, its kind (only "conflict" today) and the text the reader sees.
type Notice struct {
	Schema int    `json:"schema"`
	TS     int64  `json:"ts"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
}

// noticeSchema is the inbox format's current version.
const noticeSchema = 1

// drainMax caps how many notices one drain delivers; older overflow is dropped.
const drainMax = 5

var _ = schemas.Register(schemas.Format{
	Name: "notices", Owner: "internal/worktree", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(noticesDir(), "*.jsonl") },
	New:     func() any { return &Notice{} },
	Version: noticeSchema, Keep: "per-session notice inboxes; Drain removes what it delivers",
})

// noticesDir holds one inbox per session.
func noticesDir() string { return filepath.Join(paths.StateDir(), "notices") }

// inbox is session's inbox file.
func inbox(session string) string {
	return filepath.Join(noticesDir(), paths.Slug(session, "", 80, "session")+".jsonl")
}

// cliSession is the session a lane made from a shell records; it is no reader, so such a lane keys its own inbox.
const cliSession = "cli"

// lanePrefix starts every lane inbox key; no session id has it.
const lanePrefix = "lane-"

// LaneInbox is a lane's own inbox key: its name plus a hash of its canonical path, so equal names never share.
func LaneInbox(lane string) string {
	sum := sha256.Sum256([]byte(paths.Canon(lane)))
	return lanePrefix + paths.Slug(filepath.Base(lane), "", 40, "lane") + "-" + hex.EncodeToString(sum[:4])
}

// inboxFor is the key for a notice to a lane whose ledger names session: a real session, else the lane itself.
func inboxFor(session, lane string) string {
	if session == cliSession {
		return LaneInbox(lane)
	}
	return session
}

// DrainFor drains session's inbox and the inbox of the lane laneTop names; laneTop runs only when a lane inbox exists.
func DrainFor(session string, laneTop func() string) ([]Notice, error) {
	notices, err := DrainErr(session)
	ents, _ := os.ReadDir(noticesDir())
	if !slices.ContainsFunc(ents, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), lanePrefix) }) {
		return notices, err
	}
	top := laneTop()
	if top == "" {
		return notices, err
	}
	more, laneErr := DrainErr(LaneInbox(top))
	return append(notices, more...), errors.Join(err, laneErr)
}

// Append adds n to session's inbox under its file lock.
func Append(session string, n Notice) error {
	if session == "" {
		return errors.New("notices: no session")
	}
	n.Schema = noticeSchema
	line, err := json.Marshal(n)
	if err != nil {
		return err
	}
	file := inbox(session)
	if err := paths.AppendLine(file, line); err != nil {
		return err
	}
	return capInbox(file)
}

// inboxKeep is how many of the newest notices an inbox keeps; a lane nobody drains stays small.
const inboxKeep = 50

// capInbox rewrites file to its newest inboxKeep lines when it holds more.
func capInbox(file string) error {
	return paths.WithFileLock(file, func() error {
		raw, err := os.ReadFile(file)
		if os.IsNotExist(err) { // drained between the append and here
			return nil
		}
		if err != nil {
			return err
		}
		lines := bytes.SplitAfter(bytes.TrimRight(raw, "\n"), []byte("\n"))
		if len(lines) <= inboxKeep {
			return nil
		}
		keep := bytes.Join(lines[len(lines)-inboxKeep:], nil)
		return paths.AtomicWrite(file, append(keep, '\n'), 0o600)
	})
}

// DrainErr returns and removes session's notices (≤ drainMax, newest last); an error still yields what was read.
func DrainErr(session string) ([]Notice, error) {
	if session == "" {
		return nil, nil
	}
	file := inbox(session)
	if _, err := os.Stat(file); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("drain notices %s: %w", file, err)
	}
	raw, ok, err := takeInbox(file)
	if !ok {
		return nil, err
	}
	var out []Notice
	// no line-size cap, unlike bufio.Scanner
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		var n Notice
		if json.Unmarshal(line, &n) == nil && n.Text != "" {
			out = append(out, n)
		}
	}
	return out[max(0, len(out)-drainMax):], err
}

// takeInbox reads and removes file under its lock; ok is false when nothing was read (err says why).
func takeInbox(file string) (raw []byte, ok bool, err error) {
	var readErr error
	err = paths.WithFileLock(file, func() error {
		if raw, readErr = os.ReadFile(file); readErr != nil {
			return readErr
		}
		ok = true
		return os.Remove(file)
	})
	switch {
	case err == nil:
	case !ok && errors.Is(readErr, os.ErrNotExist):
		// another drain took it between the stat and the lock
		return nil, false, nil
	case !ok:
		return nil, false, fmt.Errorf("drain notices %s: %w", file, err)
	default:
		err = fmt.Errorf("drain notices %s: read but not removed, they may repeat: %w", file, err)
	}
	return raw, ok, err
}
