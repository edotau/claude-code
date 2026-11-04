package notices

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Claim is one line of the hooks' claims ledger (written by internal/hooks/claims.go): a path or a commit.
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
func ScanClaims(alive func(int) bool, fn func(pid int, slug string, c Claim)) error {
	entries, err := os.ReadDir(ClaimsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
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
		scanFile(file, func(c Claim) { fn(pid, slug, c) })
	}
	return nil
}

// claimsOwner parses <pid>-<slug>.jsonl.
func claimsOwner(name string) (int, string, bool) {
	base, isLog := strings.CutSuffix(name, ".jsonl")
	pidText, slug, ok := strings.Cut(base, "-")
	pid, err := strconv.Atoi(pidText)
	return pid, slug, isLog && ok && err == nil
}

// scanFile decodes the last claimTail bytes of file line by line.
func scanFile(file string, fn func(Claim)) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > claimTail {
		_, _ = f.Seek(-claimTail, io.SeekEnd)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), claimTail)
	for sc.Scan() {
		var c Claim
		if json.Unmarshal(sc.Bytes(), &c) == nil && (c.Path != "" || c.Commit != "") {
			fn(c) // the first line after a tail seek is usually torn and fails to decode
		}
	}
}

// Holders maps each claimed path in paths to "session <slug> (pid N)" or "leaf <id> of session …" labels.
func Holders(alive func(int) bool, want []string) map[string][]string {
	set := map[string]map[string]bool{}
	for _, p := range want {
		set[p] = map[string]bool{}
	}
	err := ScanClaims(alive, func(pid int, slug string, c Claim) {
		if who, ok := set[c.Path]; ok {
			who[Label(pid, slug, c.Agent)] = true
		}
	})
	out := map[string][]string{}
	if err != nil {
		return out
	}
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
