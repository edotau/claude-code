// Per-session file channels: the claims ledger and notice inboxes, both read by the hooks.

package worktree

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

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
	return paths.AppendLine(inbox(session), line)
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
