package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/setup"
)

// check is one doctor line: Hard failures make the run fail; soft ones only report.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Hard   bool   `json:"hard,omitempty"`
	Detail string `json:"detail"`
}

// doctorSection is one independent group of doctor rows, in order, and whether its hard checks passed.
type doctorSection func(ctx context.Context) ([]check, bool)

// doctorSections run concurrently and print in this order; a var so tests can swap in fakes.
var doctorSections = []doctorSection{
	collect(checkBinaryHealth), collect(checkProviderHealth), collect(checkToolHealth), collect(checkHostHealth),
	func(context.Context) ([]check, bool) { return vaultDoctorChecks(), true },
	func(context.Context) ([]check, bool) { return []check{formatsCheck()}, true },
}

// doctorWorkers caps the doctor sections probing at once.
const doctorWorkers = 4

// fanOut runs fn(0..n-1) on at most workers goroutines and returns the results in index order.
func fanOut[T any](n, workers int, fn func(i int) T) []T {
	out := make([]T, n)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(i)
		}()
	}
	wg.Wait()
	return out
}

// sectionRows is one doctorSection's result.
type sectionRows struct {
	rows []check
	ok   bool
}

// runChecks runs every read-only health check; ok is false when any hard check failed.
func runChecks(ctx context.Context) (checks []check, ok bool) {
	ok = true
	for _, r := range fanOut(len(doctorSections), doctorWorkers, func(i int) sectionRows {
		rows, sok := doctorSections[i](ctx)
		return sectionRows{rows, sok}
	}) {
		checks = append(checks, r.rows...)
		ok = ok && r.ok
	}
	return checks, ok
}

// collect runs a check-callback section into its own rows; ok is false when one of its hard checks failed.
func collect(run func(ctx context.Context, check func(bool, bool, string, string))) doctorSection {
	return func(ctx context.Context) (checks []check, ok bool) {
		ok = true
		run(ctx, func(pass, hard bool, name, detail string) {
			checks = append(checks, check{Name: name, OK: pass, Hard: hard, Detail: detail})
			if !pass && hard {
				ok = false
			}
		})
		return checks, ok
	}
}

func checkBinaryHealth(_ context.Context, check func(bool, bool, string, string)) {
	bin, err := claude.ClaudeBinary()
	check(err == nil, true, "claude binary", orErr(bin, err))
	for _, c := range codex.CodexConfigChecks() {
		check(c.State == "ok", false, c.Name, c.Detail)
	}
}

func checkProviderHealth(ctx context.Context, check func(bool, bool, string, string)) {
	reg, err := providers.Load()
	check(err == nil, true, "provider registry", orErr("vault overlay/providers", err))
	if reg != nil {
		sel, serr := providers.Select(reg, "")
		for _, c := range providerCredentialChecks(ctx, reg, sel, serr, reg.Names()) {
			check(c.OK, c.Hard, c.Name, c.Detail)
		}
		check(true, false, "opencode route", routeDetail(ctx, "opencode"))
		staticOK, staticDetail := opencodeStaticKeysDetail(reg)
		check(staticOK, false, "opencode auth.json", staticDetail)
		check(true, false, "copilot route", routeDetail(ctx, "copilot"))
		check(true, false, "codex route", routeDetail(ctx, "codex"))
		check(true, false, "gemini route", routeDetail(ctx, "gemini"))
		for _, pin := range preflight.Check(ctx, reg, time.Now()) {
			check(pin.Status == preflight.StatusOK, false, "pin "+pin.Agent, pin.Line())
		}
	}
}

func checkToolHealth(ctx context.Context, check func(bool, bool, string, string)) {
	for _, s := range tools.StatusAll() {
		check(toolDoctorOK(s), false, "tool "+s.Name, toolDoctorDetail(ctx, s))
	}
	for _, spec := range agent.Roster() {
		pass, detail := agentVersionDetail(spec)
		check(pass, false, "agent version "+spec.Name, detail)
	}
	for _, q := range quotaDoctorRows(time.Now()) {
		check(q.OK, false, q.Name, q.Detail)
	}
}

func checkHostHealth(ctx context.Context, check func(bool, bool, string, string)) {
	check(true, false, "in-flight", inflightDoctorDetail())
	proxyDetail, proxyBad := proxyStatus(ctx)
	check(!proxyBad, false, "proxy", proxyDetail)
	rtkOK, rtkDetail := rtkDoctorStatus()
	check(rtkOK, false, "rtk compressor", rtkDetail)
	tsOK, tsDetail := tailscale.Doctor(ctx)
	check(tsOK, false, "tailscale", tsDetail)
	settingsErr := settings.CheckFile(settings.SettingsFile())
	nested := setup.NestedConfig()
	check(nested.Dir == "" || onlyWorktrees(nested), false, "nested config", nestedDetail(nested))
	strayOK, strayDetail := strayClaudeJSON()
	check(strayOK, false, "claude.json", strayDetail)
	auditOK, auditDetail := auditSummary()
	check(auditOK, false, "audit (7d)", auditDetail)
	setupOK, setupDetail := setupDoctorDetail()
	check(setupOK, false, "setup", setupDetail)
	check(true, false, "browser", browserDoctorDetail())
	clipOK, clipDetail := clipboard.DoctorDetail()
	check(clipOK, false, "wl-paste proxy", clipDetail)
	check(settingsErr == nil, false, "settings.json", orErr("valid", settingsErr))
}

// routeDetail is the "<agent> route" row; the caller loaded the registry already, so a miss is not a doctor failure.
func routeDetail(ctx context.Context, name string) string {
	detail, _ := agent.RouteDetail(ctx, name)
	return detail
}

// onlyWorktrees reports a nested dir holding linked worktrees and nothing but the disposable local settings file.
func onlyWorktrees(n setup.Nested) bool {
	n.Files = slices.DeleteFunc(slices.Clone(n.Files), func(f string) bool { return f == "settings.local.json" })
	return n.OnlyWorktrees()
}

// nestedDetail says whether <config dir>/.claude exists and what keeps clean from removing it.
func nestedDetail(n setup.Nested) string {
	keep := slices.DeleteFunc(slices.Clone(n.Keep), func(k string) bool { return k == setup.KeepWorktrees })
	switch {
	case n.Dir == "":
		return "none"
	case onlyWorktrees(n):
		return n.Dir + " holds only linked worktrees"
	case len(keep) > 0:
		return n.Dir + " layers over user settings; move " + strings.Join(keep, ", ") + " into ~/.claude/settings.json first"
	default:
		return n.Dir + " is redundant (claude-code clean)"
	}
}

// auditSummary counts the last week's audited actions ("2 hook_blocked, 1 provider_switch"); a read error fails the row
func auditSummary() (ok bool, detail string) {
	ev, err := audit.Read(time.Now().AddDate(0, 0, -7))
	sum := audit.Summary(ev)
	var parts []string
	for _, a := range audit.Actions {
		if n := sum[a]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, a))
		}
	}
	detail = "no audited actions (claude-code doctor --audit)"
	if len(parts) > 0 {
		detail = strings.Join(parts, ", ") + " (claude-code doctor --audit)"
	}
	if err != nil {
		return false, "log unreadable, partial: " + detail + ": " + err.Error()
	}
	return true, detail
}

// browserDoctorDetail names the default browser and its extension state; advisory, never a failing check.
func browserDoctorDetail() string {
	rep, err := detectBrowsers()
	if err != nil {
		return "default browser not detected: " + err.Error() + " (claude-code network browser)"
	}
	for _, b := range rep.Browsers {
		if b.Default {
			return b.Name + " default, extension=" + b.Paths.Extension + " cdp=" + b.Paths.CDP + " (claude-code network browser)"
		}
	}
	return "no Chromium-family default (claude-code network browser)"
}

// rtkDoctorStatus reports the rtk compressor and fails when PATH's rtk is not that binary; a shadow is never executed.
func rtkDoctorStatus() (bool, string) {
	bin := paths.RTKBinary()
	if bin == "" {
		return true, "not on PATH — the rtk hook skips the rewrite"
	}
	found, same := paths.RTKOnPath(bin)
	switch {
	case same:
		return true, bin + " (rtk rewrite; PATH rtk is this binary)"
	case found == "":
		return false, bin + " is not on PATH — run make install (links ~/.local/bin/rtk)"
	}
	return false, "PATH rtk " + found + " shadows " + bin + " — run make install, or put ~/.local/bin first on PATH"
}

// setupDoctorDetail reports the last `setup adopt`; a missing record is a fresh clone, an unreadable one fails.
func setupDoctorDetail() (ok bool, detail string) {
	if st, found := setup.Read(); found {
		return true, fmt.Sprintf("adopted %s from %s; backup %s", st.Date.Format("2006-01-02"), st.Commit, st.BackupDir)
	}
	file := filepath.Join(paths.StateDir(), "setup.json") // setup.Read drops its error, so look at the file itself
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return true, "fresh checkout (no adopt record)"
	}
	return false, file + " exists but could not be read or decoded"
}

// opencodeStaticKeysDetail flags an opencode auth.json static key the vault already supplies; names only, no values.
func opencodeStaticKeysDetail(reg *providers.Registry) (bool, string) {
	ints, err := opencode.OpencodeAuth()
	if err != nil {
		return false, "could not read opencode logins: " + err.Error()
	}
	var stale []string
	for _, in := range ints {
		if p := opencodeProviderByID(reg, in.ID); p != nil && providers.CredentialPresent(p) && hasStaticKey(in) {
			stale = append(stale, in.ID) // a registry provider with a credential: auth.json duplicates it
		}
	}
	if len(stale) == 0 {
		return true, "clean"
	}
	slices.Sort(stale)
	notes := make([]string, len(stale))
	for i, id := range stale {
		notes[i] = fmt.Sprintf("auth.json still holds %s key; run `opencode auth logout %s`", id, id)
	}
	return false, strings.Join(notes, "; ")
}

// hasStaticKey reports whether in holds a static API key connection.
func hasStaticKey(in opencode.OpencodeIntegration) bool {
	return slices.ContainsFunc(in.Conns, func(c opencode.OpencodeConn) bool { return c.Kind == "credential" && c.Method == "key" })
}

// opencodeProviderByID finds the registry provider whose OpencodeID matches id, or nil.
func opencodeProviderByID(reg *providers.Registry, id string) *providers.Provider {
	for _, p := range reg.Providers {
		if p.OpencodeID == id {
			return p
		}
	}
	return nil
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
