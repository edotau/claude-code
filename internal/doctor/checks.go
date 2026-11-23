package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/tailscale"
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
	func(context.Context) ([]check, bool) { return hookTelemetryChecks(time.Now()), true },
}

const doctorWorkers = 4

type sectionRows struct {
	rows []check
	ok   bool
}

// runChecks runs every read-only health check; ok is false when any hard check failed.
func runChecks(ctx context.Context) (checks []check, ok bool) {
	ok = true
	results, err := parallel.Map(context.Background(), doctorSections, doctorWorkers,
		func(_ context.Context, section doctorSection) (sectionRows, error) {
			rows, sok := section(ctx)
			return sectionRows{rows, sok}, nil
		})
	parallel.Rethrow(err)
	for _, r := range results {
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
	launcherOK, launcherDetail := claudeLauncherDetail(bin)
	check(launcherOK, false, "claude launcher", launcherDetail)
	modsOK, modsDetail := modsLoadedDetail()
	check(modsOK, false, "mods", modsDetail)
	for _, c := range codex.CodexConfigChecks() {
		check(c.State == "ok", false, c.Name, c.Detail)
	}
}

// claudeLauncherDetail reports whether bin/claude is the launcher script that execs `claude --launcher`.
func claudeLauncherDetail(native string) (bool, string) {
	path := filepath.Join(paths.BinDir(), "claude")
	fi, err := os.Lstat(path)
	if err != nil {
		return false, path + " is missing (make install writes the launcher)"
	}
	body, err := os.ReadFile(path)
	if !fi.Mode().IsRegular() || err != nil || !bytes.Contains(body, []byte("claude --launcher")) {
		return false, path + " is not the harness launcher (make install writes it); real claude: " + orDash(native)
	}
	return true, path + " (real claude: " + orDash(native) + ")"
}

func orDash(s string) string {
	if s == "" {
		return "not found"
	}
	return s
}

func checkProviderHealth(ctx context.Context, check func(bool, bool, string, string)) {
	reg, err := providers.Load()
	check(err == nil, true, "provider registry", orErr("vault overlay/providers", err))
	checkModelStrings(reg, check)
	driftOK, driftDetail := overlayDriftDetail()
	check(driftOK, false, "provider overlay drift", driftDetail)
	cost := costReportCheck(reg)
	check(cost.OK, cost.Hard, cost.Name, cost.Detail)
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
		check(true, false, "pi route", routeDetail(ctx, "pi"))
		piOK, piDetail := piPackagesDetail()
		check(piOK, false, "pi packages", piDetail)
		for _, pin := range preflight.Check(ctx, reg, time.Now()) {
			check(pin.Status == preflight.StatusOK, false, "pin "+pin.Agent, pin.Line())
		}
	}
}

func checkToolHealth(ctx context.Context, check func(bool, bool, string, string)) {
	flagRows := startFlagProbes(ctx)
	for _, s := range tools.StatusAll() {
		check(toolDoctorOK(s), false, "tool "+s.Name, toolDoctorDetail(ctx, s))
	}
	for _, s := range tools.DepStatusAll() {
		pass, detail := depDoctorDetail(s)
		check(pass, false, "dep "+s.Name, detail)
	}
	for _, spec := range agent.Roster() {
		pass, detail := agentVersionDetail(spec)
		check(pass, false, "agent version "+spec.Name, detail)
	}
	flagRows(check)
	for _, q := range quotaDoctorRows(time.Now()) {
		check(q.OK, false, q.Name, q.Detail)
	}
}

func checkHostHealth(ctx context.Context, check func(bool, bool, string, string)) {
	check(true, false, "in-flight", inflightDoctorDetail())
	check(true, false, "routing fit", fitDoctorDetail())
	for _, r := range memorySyncRows() {
		check(r.OK, false, r.Name, r.Detail)
	}
	proxyDetail, proxyBad := proxyStatus(ctx)
	check(!proxyBad, false, "proxy", proxyDetail)
	rtkOK, rtkDetail := rtkDoctorStatus()
	check(rtkOK, false, "rtk compressor", rtkDetail)
	for _, r := range rtkHookRows() {
		check(r.OK, false, r.Name, r.Detail)
	}
	ccOK, ccDetail := pathBinaryStatus()
	check(ccOK, false, "claude-code on PATH", ccDetail)
	hookOK, hookDetail := hookBinaryStatus(paths.BinDir())
	check(hookOK, false, "hook binary", hookDetail)
	tsOK, tsDetail := tailscale.Doctor(ctx)
	check(tsOK, false, "tailscale", tsDetail)
	for _, r := range upstreamRows(ctx) {
		check(r.OK, false, r.Name, r.Detail)
	}
	settingsErr := settings.CheckFile(settings.SettingsFile())
	nested := setup.NestedConfig()
	check(nested.Dir == "" || onlyWorktrees(nested), false, "nested config", nestedDetail(nested))
	strayOK, strayDetail := strayClaudeJSON()
	check(strayOK, false, "claude.json", strayDetail)
	root := repoRootCheck(paths.ConfigDir())
	check(root.OK, false, root.Name, root.Detail)
	auditOK, auditDetail := auditSummary()
	check(auditOK, false, "audit (7d)", auditDetail)
	setupOK, setupDetail := setupDoctorDetail()
	check(setupOK, false, "setup", setupDetail)
	check(true, false, "browser", browserDoctorDetail())
	clipOK, clipDetail := clipboard.DoctorDetail()
	check(clipOK, false, "clipboard", clipDetail)
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

// auditSummary counts the last week's audited actions ("2 blocked (plus 1 test/probe), 1 provider_switch").
func auditSummary() (ok bool, detail string) {
	ev, err := audit.Read(time.Now().AddDate(0, 0, -7))
	sum := audit.Summary(ev)
	var parts []string
	syn := audit.Synthetic(ev)
	for _, a := range audit.Actions {
		n := sum[a]
		switch {
		case n == 0:
		case a == audit.HookBlocked && syn > 0:
			parts = append(parts, fmt.Sprintf("%d blocked (plus %d test/probe)", n-syn, syn))
		default:
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

// pathBinaryStatus fails when PATH's claude-code is not the installed ~/.claude/bin one; a shadow is never executed.
func pathBinaryStatus() (bool, string) {
	want := filepath.Join(paths.BinDir(), "claude-code")
	if _, err := os.Stat(want); err != nil {
		return true, want + " not installed — run make install"
	}
	found, err := exec.LookPath("claude-code")
	if err != nil {
		return false, want + " is not on PATH — put " + paths.BinDir() + " first on PATH"
	}
	got, gerr := filepath.EvalSymlinks(found)
	real, werr := filepath.EvalSymlinks(want)
	if gerr == nil && werr == nil && got == real {
		return true, found + " (PATH claude-code is the installed binary)"
	}
	return false, "PATH claude-code " + found + " shadows " + want + " — remove the stale copy, or put " + paths.BinDir() + " first on PATH"
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

// overlayDriftDetail names overlay rows whose snapshot no longer matches the built-in; key names only, no values.
func overlayDriftDetail() (bool, string) {
	drift, err := providers.OverlayDrift()
	if err != nil {
		return false, "could not compare the overlay with defaults.json: " + err.Error()
	}
	if len(drift) == 0 {
		return true, "overlay rows match the built-ins"
	}
	notes := make([]string, 0, len(drift))
	for _, name := range slices.Sorted(maps.Keys(drift)) {
		notes = append(notes, fmt.Sprintf("%s differs in %s", name, strings.Join(drift[name], ", ")))
	}
	return false, strings.Join(notes, "; ") + " (a row replaces its built-in whole, so new defaults do not reach it)"
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
