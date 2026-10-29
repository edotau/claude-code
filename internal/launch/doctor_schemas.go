package launch

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// knownStateNames are entries under state/ and state/harness/ that no registry row globs (locks, logs, owner dirs).
var knownStateNames = []string{
	"harness", "plans-scratch", "copilot", "projections", "statusline", "usage", "vault.db*",
	"*.lock", "*.log", "backups", "cache", "locks", "history*", "checkpoint", "agents-inflight", "memory",
	"codex", "opencode", "dispatch", "vendors", "vendor-baseline", "stop-format", "worktree-merge-ops",
	"agentversion.json", "opencode-models-check.json", "router.json", "tailscale-machines.json",
}

// formatsDoctorChecks returns the soft "formats" and "state layout" rows.
func formatsDoctorChecks() []Check {
	return []Check{formatsCheck(), stateLayoutCheck()}
}

func formatsCheck() Check {
	var stale []string
	current := 0
	for _, f := range schemas.All() {
		if f.Kind != schemas.Wire {
			current++
		}
		if f.Kind != schemas.Store || f.Probe == nil {
			continue
		}
		if v, err := f.Probe(); err == nil && v != f.Version && v != 0 {
			stale = append(stale, fmt.Sprintf("%s: schema %d (want %d)", f.Name, v, f.Version))
		}
	}
	for _, s := range schemas.Pending() {
		if !s.Rewrite || s.Err != "" {
			continue // migrate-on-read rows are informational
		}
		stale = append(stale, fmt.Sprintf("%s: 1 file at v%d (stale, install migrates)", s.Format, s.From))
	}
	if len(stale) > 0 {
		return Check{Name: "formats", OK: false, Detail: strings.Join(stale, " · ")}
	}
	return Check{Name: "formats", OK: true, Detail: fmt.Sprintf("%d formats current · %d migrations applied", current, ledgerLines())}
}

func ledgerLines() int {
	data, err := os.ReadFile(schemas.LedgerPath())
	if err != nil {
		return 0
	}
	return bytes.Count(data, []byte("\n"))
}

func stateLayoutCheck() Check {
	root := filepath.Dir(paths.StateDir())
	covered := map[string]bool{}
	for _, f := range schemas.All() {
		if f.Glob == nil {
			continue
		}
		g := f.Glob()
		matches, _ := filepath.Glob(g)
		matches = append(matches, g)
		for _, m := range matches {
			for ; strings.HasPrefix(m, root) && m != root; m = filepath.Dir(m) {
				covered[m] = true
			}
		}
	}
	var strays []string
	for _, dir := range []string{root, paths.StateDir()} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if covered[filepath.Join(dir, e.Name())] || knownStateName(e.Name()) {
				continue
			}
			strays = append(strays, e.Name())
		}
	}
	if len(strays) == 0 {
		return Check{Name: "state layout", OK: true, Detail: "all entries registered"}
	}
	sort.Strings(strays)
	detail := fmt.Sprintf("%d unregistered: ", len(strays))
	for i, s := range strays {
		if len(detail)+len(s) > 70 {
			detail += fmt.Sprintf("+%d", len(strays)-i)
			break
		}
		detail += s
		if i < len(strays)-1 {
			detail += ", "
		}
	}
	return Check{Name: "state layout", OK: false, Detail: detail}
}

func knownStateName(name string) bool {
	for _, pat := range knownStateNames {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}
