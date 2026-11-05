package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/opencode"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/network/vscode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// selfBinary is the running harness binary; a seam so install tests never link the test binary.
var selfBinary = agent.SelfBinary

// userBinDir is where install links claude-code onto PATH (~/.local/bin is on PATH by convention).
var userBinDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "" // caller's MkdirAll("") fails loudly rather than link under a bogus root path
	}
	return filepath.Join(home, ".local", "bin")
}

// SettingsFile is the live Claude Code settings.json under the config dir.
func SettingsFile() string { return filepath.Join(paths.ConfigDir(), "settings.json") }

// Check is one doctor line: Hard failures make the run fail; soft ones only report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Hard   bool   `json:"hard,omitempty"`
	Detail string `json:"detail"`
}

// DoctorChecks runs every read-only health check; ok is false when any hard check failed.
func DoctorChecks() (checks []Check, ok bool) {
	ok = true
	check := func(pass, hard bool, name, detail string) {
		checks = append(checks, Check{Name: name, OK: pass, Hard: hard, Detail: detail})
		if !pass && hard {
			ok = false
		}
	}
	bin, err := claude.ClaudeBinary()
	check(err == nil, true, "claude binary", orErr(bin, err))
	for _, c := range codex.CodexConfigChecks() {
		check(c.State == "ok", false, c.Name, c.Detail)
	}
	checkProviderHealth(check)
	checkToolHealth(check)
	checkHostHealth(check)
	checks = append(checks, vaultDoctorChecks()...)
	checks = append(checks, formatsDoctorChecks()...)
	return checks, ok
}

func checkProviderHealth(check func(bool, bool, string, string)) {
	reg, err := providers.Load()
	check(err == nil, true, "provider registry", orErr("vault overlay/providers", err))
	if reg != nil {
		sel, serr := providers.Select(reg, "")
		for _, c := range providerCredentialChecks(reg, sel, serr, reg.Names()) {
			check(c.OK, c.Hard, c.Name, c.Detail)
		}
		check(true, false, "opencode route", routeDetail("opencode"))
		staticOK, staticDetail := opencodeStaticKeysDetail(reg)
		check(staticOK, false, "opencode auth.json", staticDetail)
		check(true, false, "copilot route", routeDetail("copilot"))
		check(true, false, "codex route", routeDetail("codex"))
		check(true, false, "gemini route", routeDetail("gemini"))
		for _, pin := range preflight.Check(reg, time.Now()) {
			check(pin.Status == preflight.StatusOK, false, "pin "+pin.Agent, pin.Line())
		}
	}
}

func checkToolHealth(check func(bool, bool, string, string)) {
	for _, s := range tools.StatusAll() {
		check(toolDoctorOK(s), false, "tool "+s.Name, toolDoctorDetail(s))
	}
	for _, spec := range agent.Roster() {
		pass, detail := agentVersionDetail(spec)
		check(pass, false, "agent version "+spec.Name, detail)
	}
	for _, q := range quotaDoctorRows(time.Now()) {
		check(true, false, q.Name, q.Detail)
	}
}

func checkHostHealth(check func(bool, bool, string, string)) {
	check(true, false, "in-flight", inflightDoctorDetail())
	proxyDetail, proxyStale := proxyStatus()
	check(!proxyStale, false, "proxy", proxyDetail)
	check(true, false, "rtk compressor", rtkDoctorStatus())
	tsOK, tsDetail := tailscale.Doctor(context.Background())
	check(tsOK, false, "tailscale", tsDetail)
	check(true, false, "vscode serve", vscodeServeDoctorDetail())
	has, err := settingsHasHooks(SettingsFile())
	nested := clean.NestedConfig()
	check(nested.Dir == "" || nested.OnlyWorktrees(), false, "nested config", nestedDetail(nested))
	strayOK, strayDetail := strayClaudeJSON()
	check(strayOK, false, "claude.json", strayDetail)
	check(true, false, "audit (7d)", auditSummary())
	check(true, false, "setup", setupDoctorDetail())
	check(true, false, "browser", browserDoctorDetail())
	clipOK, clipDetail := clipboard.DoctorDetail()
	check(clipOK, false, "wl-paste proxy", clipDetail)
	check(has, false, "settings.json harness hooks", orErr(map[bool]string{true: "present", false: "missing (run claude-code install)"}[has], err))
}

// routeDetail is the "<agent> route" row; the caller loaded the registry already, so a miss is not a doctor failure.
func routeDetail(name string) string {
	detail, _ := agent.RouteDetail(name)
	return detail
}

// credentialProbeWorkers caps the concurrent live credential probes doctor runs.
const credentialProbeWorkers = 4

// probeCredential is the live credential probe; a var so tests can substitute a fake.
var probeCredential = providers.ProbeCredential

// providerCredentialChecks runs providerCredentialCheck for names concurrently and returns the rows in names order.
func providerCredentialChecks(reg *providers.Registry, sel providers.Selection, selectErr error, names []string) []Check {
	rows := make([]Check, len(names))
	sem := make(chan struct{}, credentialProbeWorkers)
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = providerCredentialCheck(reg, sel, selectErr, n)
		}()
	}
	wg.Wait()
	return rows
}

func providerCredentialCheck(reg *providers.Registry, sel providers.Selection, selectErr error, name string) Check {
	p := reg.Providers[name]
	session := selectErr == nil && sel.Provider == p
	label := p.Auth.Method()
	if p.Auth.Env != "" {
		label += " " + p.Auth.Env
	}
	if session {
		label += " (session profile)"
	}
	present := providers.CredentialPresent(p)
	if present {
		present, label = probeCredentialLabel(p, label)
	}
	if !present {
		present, label = alternativeCredentialLabel(reg, name, label)
	}
	if !present && !session && !profileInUse(reg, name) {
		present, label = true, label+" · unused"
	}
	return Check{Name: "credential " + name, OK: present, Hard: session, Detail: label}
}

func alternativeCredentialLabel(reg *providers.Registry, name, label string) (bool, string) {
	alt := altAuth(reg, name)
	if alt == "" {
		return false, label
	}
	if !strings.Contains(label, "REJECTED") {
		label += " · no key"
	}
	return true, label + " · " + alt
}

func probeCredentialLabel(p *providers.Provider, label string) (bool, string) {
	present := true
	pctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	status, detail := probeCredential(pctx, p)
	cancel()
	switch status {
	case "login":
		if p.Auth.Login == "" { // the Claude Max login: say whether unattended runs have a token too
			if providers.HeadlessOAuthToken() != "" {
				label += " · headless token set"
			} else {
				label += " · no headless token (claude-code configure → claude mints one)"
			}
		}
	case "verified":
		label += " · verified"
	case "rejected":
		present = false                           // stored, but the provider refuses it: as unusable as a missing key
		detail, _, _ = strings.Cut(detail, ": {") // drop the JSON error body the probe appends
		label += " · REJECTED: " + detail
	default:
		label += " · " + status + ": " + detail
	}
	return present, label
}

// profileInUse reports whether the registry default/fallback chain or any agent's profile pin names the profile.
func profileInUse(reg *providers.Registry, name string) bool {
	if name == reg.Default || slices.Contains(reg.Fallback, name) {
		return true
	}
	for _, rec := range providers.AgentRecords() {
		if rec.Profile == name {
			return true
		}
	}
	return false
}

// altAuth names the sign-in that covers a provider whose own key is absent or rejected, or "" when none does:
// claude runs on its OAuth login (or headless token) without ANTHROPIC_API_KEY.
func altAuth(reg *providers.Registry, name string) string {
	switch name {
	case "anthropic":
		if providers.HeadlessOAuthToken() != "" {
			return "claude uses the OAuth token instead"
		}
		if st := agent.AuthStatus("claude"); st.Present {
			plan := ""
			if st.PlanHint != "" {
				plan = " (" + st.PlanHint + ")"
			}
			return "claude uses the OAuth login" + plan + " instead"
		}
	}
	return ""
}

// PrintChecks writes checks as ✓/✗ lines, shared by the doctor checks so all print one format.
func PrintChecks(w io.Writer, checks []Check) {
	color := ansi.Enabled(w)
	for _, c := range checks {
		mark := ansi.Style(color, ansi.Green, "✓")
		if !c.OK {
			mark = ansi.Style(color, ansi.Red, "✗")
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, ansi.Status(color, c.Detail))
	}
}

// nestedDetail says whether <config dir>/.claude exists and what keeps clean from removing it.
func nestedDetail(n clean.Nested) string {
	switch {
	case n.Dir == "":
		return "none"
	case n.OnlyWorktrees():
		return n.Dir + " holds only linked worktrees"
	case len(n.Keep) > 0:
		keep := slices.DeleteFunc(slices.Clone(n.Keep), func(k string) bool { return k == clean.KeepWorktrees })
		return n.Dir + " layers over user settings; move " + strings.Join(keep, ", ") + " into the defaults first"
	default:
		return n.Dir + " is redundant (claude-code clean)"
	}
}

// auditSummary counts the last week's audited actions, e.g. "2 hook_blocked, 1 provider_switch".
func auditSummary() string {
	ev, err := audit.Read(time.Now().AddDate(0, 0, -7))
	if err != nil {
		return err.Error()
	}
	sum := audit.Summary(ev)
	var parts []string
	for _, a := range audit.Actions {
		if n := sum[a]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, a))
		}
	}
	if len(parts) == 0 {
		return "no audited actions (claude-code doctor --audit)"
	}
	return strings.Join(parts, ", ") + " (claude-code doctor --audit)"
}

// browserDoctorDetail names the default browser and its extension state; advisory, never a failing check.
func browserDoctorDetail() string {
	rep := detectBrowsers()
	for _, b := range rep.Browsers {
		if b.Default {
			return b.Name + " default, extension=" + b.Paths.Extension + " cdp=" + b.Paths.CDP + " (claude-code network browser)"
		}
	}
	return "no Chromium-family default (claude-code network browser)"
}

// rtkDoctorStatus reports whether the rtk output compressor is on PATH; the PreToolUse hook no-ops without it.
func rtkDoctorStatus() string {
	if bin := hooks.RTKBinary(); bin != "" {
		return bin + " (rtk rewrite)"
	}
	return "not on PATH — the rtk hook skips the rewrite"
}

// setupDoctorDetail reports the last `setup adopt`, never a failing check — a fresh clone has none.
func setupDoctorDetail() string {
	st, ok := setup.Read()
	if !ok {
		return "fresh checkout (no adopt record)"
	}
	return fmt.Sprintf("adopted %s from %s; backup %s", st.Date.Format("2006-01-02"), st.Commit, st.BackupDir)
}

// opencodeStaticKeysDetail flags an opencode auth.json static key for a provider the harness vault
// already supplies via the {env:} indirection — names only, never a credential value.
func opencodeStaticKeysDetail(reg *providers.Registry) (bool, string) {
	ints, err := opencode.OpencodeAuth()
	if err != nil {
		return true, "skipped: " + err.Error()
	}
	var stale []string
	for _, in := range ints {
		p := opencodeProviderByID(reg, in.ID)
		if p == nil || !providers.CredentialPresent(p) {
			continue // TEST-DISABLE
		}
		for _, c := range in.Conns {
			if c.Kind == "credential" && c.Method == "key" {
				stale = append(stale, in.ID)
				break
			}
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

// opencodeProviderByID finds the registry provider whose OpencodeID matches id, or nil.
func opencodeProviderByID(reg *providers.Registry, id string) *providers.Provider {
	if reg == nil {
		return nil
	}
	for _, p := range reg.Providers {
		if p.OpencodeID == id {
			return p
		}
	}
	return nil
}

// proxyStatus is the running proxy's port, pid and build; stale is true when it runs an older binary than this one.
func proxyStatus() (detail string, stale bool) {
	st, err := proxy.ReadState()
	if errors.Is(err, os.ErrNotExist) {
		return "not running", false
	}
	if err != nil || st.Port == 0 {
		return "proxy.json unreadable", false
	}
	h, ok := proxy.Holder(context.Background(), st.Port)
	if !ok {
		return "stale proxy.json (port " + strconv.Itoa(st.Port) + " not answering)", false
	}
	if h.Build != textutil.BuildVersion() {
		return fmt.Sprintf("pid %d runs build %s; restart when idle", h.PID, h.Build), true
	}
	return fmt.Sprintf("port %d pid %d, %d sessions", st.Port, h.PID, h.Sessions), false
}

// vscodeServeDoctorDetail reports the vscode-serve daemon and its tailscale mapping, token redacted.
func vscodeServeDoctorDetail() string {
	st, running := vscode.Running()
	if !running {
		return "stopped"
	}
	url, _, _ := strings.Cut(st.URL, "?")
	return fmt.Sprintf("running on :%d -> %s", st.Port, url)
}

func settingsHasHooks(file string) (bool, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	var doc struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return false, err
	}
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if settings.IsHarness(h.Command) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// toolDoctorOK is false only for a bin/<agent> entry that exists but cannot run (broken link, retired shim).
func toolDoctorOK(s tools.Status) bool {
	bin, ok := paths.AgentName(s.Name)
	if !ok {
		return true
	}
	if _, err := os.Lstat(filepath.Join(paths.BinDir(), bin)); err != nil {
		return true
	}
	_, err := paths.AgentBin(bin)
	return err == nil
}

// agentVersionDetail renders one agent's version row: ok <ver>, warn <ver> below|above <bound>: <reason>, or not installed.
func agentVersionDetail(spec agent.Schema) (pass bool, detail string) {
	v, err := tools.Version(spec.Name)
	switch {
	case errors.Is(err, tools.ErrAgentNotInstalled):
		return true, "not installed"
	case err != nil:
		return true, "version unknown (" + err.Error() + ")"
	}
	if p := tools.VersionProblem(spec.CLI, v); p != "" {
		return false, "warn " + v + " " + p
	}
	return true, "ok " + v
}

// toolDoctorDetail renders one tool's doctor line: an agent's bin/ entry, else path + version, then extra PATH copies.
func toolDoctorDetail(s tools.Status) string {
	if bin, ok := paths.AgentName(s.Name); ok {
		return agentDoctorDetail(s, bin)
	}
	if s.Path == "" {
		if s.Hint != "" {
			return "not found (" + s.Hint + ")"
		}
		return "not found (claude-code install --tools=" + s.Name + ")"
	}
	var detail strings.Builder
	detail.WriteString(s.Path)
	if s.Version != "" {
		detail.WriteString(" " + s.Version)
	}
	switch {
	case s.NPMGlobal:
		detail.WriteString(" (npm-global; claude-code install --tools retires this)")
	case s.LocalBin:
		detail.WriteString(" (~/.local/bin)")
	}
	detail.WriteString(extraPATHCopies(s.Name, s.Path))
	return detail.String()
}

// agentDoctorDetail renders BinDir/<bin>: `<dst> → <Target> <version>`, a direct install, or why it is unusable.
func agentDoctorDetail(s tools.Status, bin string) string {
	dst := filepath.Join(paths.BinDir(), bin)
	if _, err := paths.AgentBin(bin); err != nil {
		if s.Path != "" {
			return err.Error() + " · on PATH: " + s.Path
		}
		return err.Error()
	}
	var detail strings.Builder
	detail.WriteString(dst)
	if s.Target != "" {
		detail.WriteString(" → " + s.Target)
	}
	version := s.Version
	if s.Target == "" || s.Target != s.Path {
		version = toolPATHVersion(dst)
	}
	if version != "" {
		detail.WriteString(" " + version)
	}
	if s.Target == "" {
		detail.WriteString(" (direct install in bin/)")
	}
	if s.NPMGlobal && s.Target == s.Path {
		detail.WriteString(" (npm-global)")
	}
	detail.WriteString(extraPATHCopies(s.Name, dst))
	return detail.String()
}

// extraPATHCopies lists PATH copies of name that resolve somewhere other than primary, as " · also on PATH: …".
func extraPATHCopies(name, primary string) string {
	first, err := filepath.EvalSymlinks(primary)
	if err != nil { // broken/inaccessible symlink: fall back to the path itself so comparison still works
		first = primary
	}
	first, _ = filepath.Abs(first)
	var out strings.Builder
	for _, path := range toolPATHCopies(name) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			real = path
		}
		real, _ = filepath.Abs(real)
		if real == first {
			continue
		}
		extra := path
		if version := toolPATHVersion(path); version != "" {
			extra += " " + version
		}
		out.WriteString(" · also on PATH: " + extra)
	}
	return out.String()
}

// toolPATHCopies lists distinct executables named name on PATH, skipping BinDir and the harness binary.
func toolPATHCopies(name string) []string {
	binDir, _ := filepath.Abs(paths.BinDir())
	self, _ := agent.SelfBinary()
	seen := map[string]bool{}
	var copies []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if abs, _ := filepath.Abs(dir); abs == binDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		real := executableRealPath(candidate)
		if real == "" || real == self || filepath.Base(real) == "claude-code" || seen[real] {
			continue
		}
		seen[real] = true
		copies = append(copies, candidate)
	}
	return copies
}

func executableRealPath(candidate string) string {
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return ""
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return ""
	}
	return real
}

// toolPATHVersion is the first --version line of a PATH copy, cached by path+size+mtime like the tools version cache.
func toolPATHVersion(path string) string { return tools.ProbedVersionLine(path, spawnPATHVersion) }

func spawnPATHVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = 200 * time.Millisecond // lets stdout drain under load
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
