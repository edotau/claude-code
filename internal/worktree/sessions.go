// Per-session file channels: the claims ledger and notice inboxes, both read by the hooks.

package worktree

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Claim is one line of the hooks' claims ledger (written by internal/hooks/wthook/claims.go): a path.
type Claim struct {
	Path  string `json:"path,omitempty"`
	Agent string `json:"agent_id,omitempty"`
	TS    int64  `json:"ts"`
}

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
		if err := scanFile(file, func(c Claim) { fn(pid, slug, c) }); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
			fn(c) // the first line after a tail seek is usually torn and fails to decode
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

// Drain returns and removes session's notices (at most drainMax, newest last); errors yield none.
func Drain(session string) []Notice {
	if session == "" {
		return nil
	}
	file := inbox(session)
	if _, err := os.Stat(file); err != nil {
		return nil // the common case: no inbox, no lock taken
	}
	var raw []byte
	err := paths.WithFileLock(file, func() error {
		var err error
		if raw, err = os.ReadFile(file); err != nil {
			return err
		}
		return os.Remove(file)
	})
	if err != nil {
		return nil
	}
	var out []Notice
	for line := range bytes.SplitSeq(raw, []byte("\n")) { // no line-size cap, unlike bufio.Scanner
		var n Notice
		if json.Unmarshal(line, &n) == nil && n.Text != "" {
			out = append(out, n)
		}
	}
	return out[max(0, len(out)-drainMax):]
}
