package notices

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Claim is one line of the hooks' claims ledger (written by internal/hooks/wthook/claims.go): a path or a commit.
type Claim struct {
	Path   string `json:"path,omitempty"`
	Commit string `json:"commit,omitempty"`
	Agent  string `json:"agent_id,omitempty"`
	TS     int64  `json:"ts"`
}

// claimTail is how much of each claims file a scan reads: the newest lines are the ones that matter.
const claimTail = 1 << 20

// ClaimsDir holds the claims ledger, one <owner pid>-<session slug>.jsonl per session.
func ClaimsDir() string { return filepath.Join(paths.StateDir(), "hooks", "claims") }

// ScanClaims calls fn for each claim in a live owner's file (alive probes the pid); a dead owner's file is removed.
// A live file that fails to open or scan is joined into the returned error; the other files are still scanned.
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
		if json.Unmarshal(sc.Bytes(), &c) == nil && (c.Path != "" || c.Commit != "") {
			fn(c) // the first line after a tail seek is usually torn and fails to decode
		}
	}
	return sc.Err()
}

// Holders maps each claimed path in paths to "session <slug> (pid N)" or "leaf <id> of session …" labels.
// On a scan error it still returns the holders it read; the error has no channel to the caller.
func Holders(alive func(int) bool, want []string) map[string][]string {
	set := map[string]map[string]bool{}
	for _, p := range want {
		set[p] = map[string]bool{}
	}
	_ = ScanClaims(alive, func(pid int, slug string, c Claim) {
		if who, ok := set[c.Path]; ok {
			who[Label(pid, slug, c.Agent)] = true
		}
	})
	out := map[string][]string{}
	for p, who := range set {
		for w := range who {
			out[p] = append(out[p], w)
		}
	}
	return out
}

// Label names a claimant from its claims file name and the line's agent id.
func Label(pid int, slug, agent string) string {
	s := "session " + slug[:min(8, len(slug))] + " (pid " + strconv.Itoa(pid) + ")"
	if agent != "" {
		return "leaf " + agent[:min(8, len(agent))] + " of " + s
	}
	return s
}
