package launch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc/inflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

// strayClaudeJSON flags ~/.claude/.claude.json: claude reads it instead of ~/.claude.json under CLAUDE_CONFIG_DIR=~/.claude.
func strayClaudeJSON() (ok bool, detail string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return true, "home unresolved"
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if _, err := os.Stat(stub); err != nil {
		return true, "none (~/.claude.json is the only one)"
	}
	return false, "stray " + stub + "; move it aside (mv " + stub + "{,.bak})"
}

// inflightDoctorDetail is the live agent count against the machine cap, then each busy account against its own.
func inflightDoctorDetail() string {
	lim := inflight.CurrentLimits()
	snap := inflight.Count()
	var parts []string
	for _, acct := range slices.Sorted(func(yield func(string) bool) {
		for k, n := range snap.Per {
			if n > 0 && !yield(k) {
				return
			}
		}
	}) {
		parts = append(parts, fmt.Sprintf("%s %d/%d", acct, snap.Per[acct], lim.For(acct)))
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	return fmt.Sprintf("%d/%d (%s)", snap.Total, lim.Total, strings.Join(parts, ", "))
}

// quotaDoctorRows is one row per exhausted or low account in route's quota.json, with its reset and the manual fix.
func quotaDoctorRows(now time.Time) []Check {
	st := router.LoadState()
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var rows []Check
	for _, k := range keys {
		e := st[k]
		s := e.Status(now)
		if s != router.StateExhausted && s != router.StateLow {
			continue
		}
		d := fmt.Sprintf("%s at %.0f%%", s, e.UsedPct)
		if u := e.Until(now); !u.IsZero() {
			d += ", resets " + u.Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, Check{Name: "quota " + k, Detail: d + "; " + quotaFix(k)})
	}
	rows = append(rows, Check{Name: "quota gemini", OK: true,
		Detail: "failure-driven only (Antigravity's quota endpoint denies access)"})
	return rows
}

// quotaFix is the manual remedy; the router itself never changes accounts.
func quotaFix(key string) string {
	switch {
	case strings.HasPrefix(key, "github·"):
		return "routed around; `claude-code configure github` is the only way to change the Copilot account"
	case strings.HasPrefix(key, "opencode·"):
		return "routed around; `opencode auth login` changes opencode's login"
	}
	return "routed around until the reset; `claude-code gateway router` shows the pick"
}

// knownStateNames are entries under state/ and state/harness/ that no registry row globs (locks, logs, owner dirs).
var knownStateNames = []string{
	"harness", "plans-scratch", "copilot", "projections", "statusline", "usage", "vault.db*",
	"*.lock", "*.log", "backups", "cache", "locks", "history*", "checkpoint", "agents-inflight", "memory",
	"codex", "opencode", "preflight", "dispatch", "vendors", "vendor-baseline", "stop-format", "worktree-merge-ops", "worktree-busy",
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

func vaultDoctorChecks() []Check {
	row := Check{Name: "vault", OK: true}
	retired := 0
	_, err := os.Stat(vault.Path())
	if errors.Is(err, fs.ErrNotExist) {
		row.Detail = "not created yet"
	} else {
		if err == nil {
			err = vault.View(func(t *vault.Tx) error {
				retired = len(vault.Retired(t))
				return nil
			})
		}
		count, source := 0, "not needed"
		if err == nil {
			// Shares the "vault master key" row's resolution instead of re-resolving the master key here.
			var kerr error
			count, source, kerr = secrets.MasterKeyStatus()
			err = kerr
		}
		row.Detail = fmt.Sprintf("%s · key %s · %d secret(s) · %d retired", vault.Path(), source, count, retired)
		if err != nil {
			row.OK = false
			row.Detail += ": " + err.Error()
		}
	}
	// "vault master key" repeats this row's own count/source/error; drop it here, where "vault" already
	// shows it, but keep it in secretsDoctorChecks for VaultDoctor (doctor --secrets never prints "vault").
	checks, _ := secretsDoctorChecks()
	checks = slices.DeleteFunc(checks, func(c Check) bool { return c.Name == "vault master key" })
	return append([]Check{row}, checks...)
}

// secretsDoctorChecks adapts secrets.DoctorChecks to soft doctor rows; gateOK is false when a gating row failed.
func secretsDoctorChecks() (checks []Check, gateOK bool) {
	reg, _ := providers.Load() // a load error is reported by the provider registry row; nil skips name checks
	gateOK = true
	for _, c := range secrets.DoctorChecks(reg) {
		checks = append(checks, Check{Name: c.Name, OK: c.OK, Detail: c.Detail})
		if c.Gate && !c.OK {
			gateOK = false
		}
	}
	return checks, gateOK
}

// VaultDoctor prints the vault rows as ✓/✗ lines for doctor --secrets; false when a gating row failed.
func VaultDoctor(w io.Writer) bool {
	checks, ok := secretsDoctorChecks()
	fmt.Fprintln(w, "== vault checks ==")
	printChecks(w, checks)
	return ok
}
