package launch

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/tools"
	"github.com/edotau/claude-code/internal/vendors"
	"github.com/edotau/claude-code/internal/vscodeserve"
)

// userBinDir is where install links claude-code onto PATH (~/.local/bin is on PATH by convention).
var userBinDir = func() string {
	home, _ := os.UserHomeDir()
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
	bin, err := ClaudeBinary()
	check(err == nil, true, "claude binary", orErr(bin, err))
	reg, err := providers.Load()
	check(err == nil, true, "provider registry", orErr("vault overlay/providers", err))
	if reg != nil {
		sel, serr := providers.Select(reg, "")
		for _, n := range reg.Names() {
			c := providerCredentialCheck(reg, sel, serr, n)
			check(c.OK, c.Hard, c.Name, c.Detail)
		}
		check(true, false, "opencode route", opencodeRouteDetail(reg, sel, serr))
		staticOK, staticDetail := opencodeStaticKeysDetail(reg)
		check(staticOK, false, "opencode auth.json", staticDetail)
		check(true, false, "copilot route", copilotRouteDetail(reg, serr))
		check(true, false, "codex route", codexRouteDetail())
		check(true, false, "gemini route", geminiRouteDetail())
	}
	for _, s := range tools.StatusAll() {
		check(toolDoctorOK(s), false, "tool "+s.Name, toolDoctorDetail(s))
	}
	check(true, false, "router", routerStatus())
	check(true, false, "rtk compressor", rtkDoctorStatus())
	tsOK, tsDetail := tailscale.Doctor(context.Background())
	check(tsOK, false, "tailscale", tsDetail)
	check(true, false, "vscode serve", vscodeServeDoctorDetail())
	has, err := settingsHasHooks(SettingsFile())
	nested := clean.NestedConfig()
	check(nested.Dir == "" || nested.OnlyWorktrees(), false, "nested config", nestedDetail(nested))
	check(true, false, "audit (7d)", auditSummary())
	check(true, false, "setup", setupDoctorDetail())
	check(true, false, "browser", browserDoctorDetail())
	clipOK, clipDetail := clipboard.DoctorDetail()
	check(clipOK, false, "wl-paste proxy", clipDetail)
	check(has, false, "settings.json harness hooks", orErr(map[bool]string{true: "present", false: "missing (run claude-code install)"}[has], err))
	drift, driftErr := CheckSettingsDrift()
	check(driftErr == nil && !drift.Drifted(), false, "settings drift", settingsDriftDetail(drift, driftErr))
	if wdrift, wok, werr := CheckWindowsSettingsDrift(); wok {
		check(werr == nil && !wdrift.Drifted(), false, "settings (windows)", settingsDriftDetailLabeled(wdrift, werr, "settings (windows)"))
	}
	checks = append(checks, vaultDoctorChecks()...)
	return checks, ok
}

// RouteDetail returns the "<vendor> route" detail string DoctorChecks would emit for vendor, without
// running the rest of the doctor sweep; ok is false for an unknown vendor or a registry load error.
func RouteDetail(vendor string) (detail string, ok bool) {
	switch vendor {
	case "codex":
		return codexRouteDetail(), true
	case "gemini":
		return geminiRouteDetail(), true
	case "opencode":
		reg, err := providers.Load()
		if err != nil || reg == nil {
			return "", false
		}
		sel, selErr := providers.Select(reg, "")
		return opencodeRouteDetail(reg, sel, selErr), true
	case "copilot":
		reg, err := providers.Load()
		if err != nil || reg == nil {
			return "", false
		}
		_, selErr := providers.Select(reg, "")
		return copilotRouteDetail(reg, selErr), true
	default:
		return "", false
	}
}

func providerCredentialCheck(reg *providers.Registry, sel providers.Selection, selectErr error, name string) Check {
	p := reg.Providers[name]
	session := selectErr == nil && sel.Provider == p
	label := p.Auth.Type
	if p.Auth.Env != "" {
		label += " " + p.Auth.Env
	}
	if session {
		label += " (session provider)"
	}
	present := providers.CredentialPresent(p)
	if present {
		pctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		status, detail := providers.ProbeCredential(pctx, p)
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
	}
	if !present {
		if alt := altAuth(reg, name); alt != "" {
			if !strings.Contains(label, "REJECTED") {
				label += " · no key"
			}
			present, label = true, label+" · "+alt
		}
	}
	return Check{Name: "credential " + name, OK: present, Hard: session, Detail: label}
}

// altAuth names the sign-in that covers a provider whose own key is absent or rejected, or "" when none does:
// claude runs on its OAuth login (or headless token) without ANTHROPIC_API_KEY.
func altAuth(reg *providers.Registry, name string) string {
	switch name {
	case "anthropic":
		if providers.HeadlessOAuthToken() != "" {
			return "claude uses the OAuth token instead"
		}
		if ok, plan := providers.ClaudeLoginStatus(); ok {
			return "claude uses the OAuth login" + plan + " instead"
		}
	}
	return ""
}

// Doctor prints DoctorChecks as ✓/✗ lines and reports whether every hard check passed.
func Doctor(w io.Writer) bool {
	checks, ok := DoctorChecks()
	color := ansi.Enabled(w)
	for _, c := range checks {
		mark := ansi.Style(color, ansi.Green, "✓")
		if !c.OK {
			mark = ansi.Style(color, ansi.Red, "✗")
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, ansi.Status(color, c.Detail))
	}
	return ok
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
		return n.Dir + " layers over user settings; move " + strings.Join(keep, ", ") + " into the template first"
	default:
		return n.Dir + " is redundant (claude-code clean)"
	}
}

// settingsDriftDetail is the doctor detail text for the "settings drift" check.
func settingsDriftDetail(d SettingsDrift, err error) string {
	return settingsDriftDetailLabeled(d, err, "settings")
}

// settingsDriftDetailLabeled strips label+": " off each drift line, for the doctor detail column.
func settingsDriftDetailLabeled(d SettingsDrift, err error, label string) string {
	if err != nil {
		return err.Error()
	}
	lines := d.LinesLabeled(label)
	if len(lines) == 0 {
		return "clean"
	}
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, label+": ") // LinesLabeled already caps each line at 80
	}
	return strings.Join(lines, "; ")
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
	rep := browser.Detect(browser.NewEnv())
	for _, b := range rep.Browsers {
		if b.Default {
			return b.Name + " default, extension=" + b.Paths.Extension + " cdp=" + b.Paths.CDP + " (claude-code web browsers)"
		}
	}
	return "no Chromium-family default (claude-code web browsers)"
}

// rtkDoctorStatus reports whether the rtk output compressor is on PATH; the PreToolUse hook no-ops without it.
func rtkDoctorStatus() string {
	if bin := hooks.RTKBinary(); bin != "" {
		return bin + " (" + hooks.RTKCommandList() + ")"
	}
	return "not on PATH — bash-guard skips the rtk rewrite"
}

// setupDoctorDetail reports the last `setup adopt`, never a failing check — a fresh clone has none.
func setupDoctorDetail() string {
	st, ok := setup.Read()
	if !ok {
		return "fresh checkout (no adopt record)"
	}
	return fmt.Sprintf("adopted %s from %s; backup %s", st.Date.Format("2006-01-02"), st.Commit, st.BackupDir)
}

// opencodeRouteDetail reports the opencode route `claude-code generate` would render: provider,
// model/small_model (the github-copilot route only), the file it writes, and — names/booleans only,
// never a credential — whether opencode has its own GitHub Copilot login and whether the copilot CLI has one.
func opencodeRouteDetail(reg *providers.Registry, sel providers.Selection, selErr error) string {
	opencodeLogin := vendors.OpencodeCopilotLoginPresent()
	copilotCLILogin := vendors.CopilotLoginPresent()
	if selErr != nil {
		return fmt.Sprintf("opencode_login=%v copilot_cli_login=%v (%v)", opencodeLogin, copilotCLILogin, selErr)
	}
	target, _, err := vendors.ResolveTarget(reg, "opencode", sel, false)
	if err != nil || target.Provider == nil {
		return fmt.Sprintf("opencode_login=%v copilot_cli_login=%v (%v)", opencodeLogin, copilotCLILogin, orErr("", err))
	}
	home, _ := os.UserHomeDir()
	file := filepath.Join(home, ".config", "opencode", "opencode.json")
	if target.Provider.Name != "github-copilot" {
		return fmt.Sprintf("provider=%s file=%s opencode_login=%v copilot_cli_login=%v", target.Provider.Name, file, opencodeLogin, copilotCLILogin)
	}
	mainID, smallID, ok := vendors.OpencodeCopilotModelIDs(reg)
	if !ok {
		return fmt.Sprintf("provider=github-copilot file=%s (no sonnet/haiku model configured) opencode_login=%v copilot_cli_login=%v", file, opencodeLogin, copilotCLILogin)
	}
	// opus/sonnet/haiku here are github-copilot's own table (configure github), separate from COPILOT_MODEL.
	opusID := reg.Providers["github-copilot"].Models[models.Opus]
	return fmt.Sprintf("provider=github-copilot model=github-copilot/%s small_model=github-copilot/%s opus/sonnet/haiku=%s/%s/%s file=%s opencode_login=%v copilot_cli_login=%v",
		mainID, smallID, opusID, mainID, smallID, file, opencodeLogin, copilotCLILogin)
}

// opencodeStaticKeysDetail flags an opencode auth.json static key for a provider the harness vault
// already supplies via the {env:} indirection — names only, never a credential value.
func opencodeStaticKeysDetail(reg *providers.Registry) (bool, string) {
	ints, err := vendors.OpencodeAuth()
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

// copilotRouteDetail reports the copilot CLI's resolved leg (DescribeRoute): its own GitHub login,
// BYOK with an anthropic or gemini key, or none — names/booleans/leg only, never a credential. On the
// GitHub login leg it also names COPILOT_MODEL's source and the effort/context pins every launch reads.
func copilotRouteDetail(reg *providers.Registry, selErr error) string {
	if selErr != nil {
		return fmt.Sprintf("(%v)", selErr)
	}
	ri := vendors.DescribeRoute(reg, "copilot")
	if !ri.OK {
		return ri.Reason
	}
	detail := fmt.Sprintf("leg=%s provider=%s model=%s (%s)", ri.Leg, ri.Provider, ri.Model, ri.Reason)
	if ri.Leg == vendors.LegCopilotLogin {
		_, source := providers.CopilotModel(reg)
		effort := cmp.Or(providers.Pin(providers.PinCopilotEffort), "default")
		context := cmp.Or(providers.Pin(providers.PinCopilotContext), "default")
		detail += fmt.Sprintf(" source=%s effort=%s context=%s", source, effort, context)
	}
	return detail
}

// codexRouteDetail reports codex's own CODEX_MODEL/CODEX_REASONING_EFFORT pins — names/booleans only,
// never a credential; unset means codex's own default (config.toml, then its ChatGPT-login catalog).
func codexRouteDetail() string {
	model, source := providers.CodexModel()
	effort, _ := providers.CodexEffort()
	return fmt.Sprintf("model=%s source=%s effort=%s", cmp.Or(model, "(default)"), source, cmp.Or(effort, "default"))
}

// geminiRouteDetail reports agy's own GEMINI_MODEL pin (effort rides the id's own suffix) — unset
// means agy's own default (agent.AgyDefaultModel).
func geminiRouteDetail() string {
	model, source := providers.GeminiModel()
	return fmt.Sprintf("model=%s source=%s", cmp.Or(model, "(default)"), source)
}

func routerStatus() string {
	b, err := os.ReadFile(filepath.Join(paths.StateDir(), "router.json"))
	if err != nil {
		return "not running"
	}
	var st struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(b, &st) != nil || st.Port == 0 {
		return "router.json unreadable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", st.Port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "stale router.json (port " + strconv.Itoa(st.Port) + " not answering)"
	}
	_ = resp.Body.Close()
	return fmt.Sprintf("port %d %s", st.Port, resp.Status)
}

// vscodeServeDoctorDetail reports the vscode-serve daemon and its tailscale mapping, token redacted.
func vscodeServeDoctorDetail() string {
	st, running := vscodeserve.Running()
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
		Hooks map[string][]hookspec.Group `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return false, err
	}
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if hookspec.IsHarness(h.Command) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// toolDoctorOK is false only for a bin/<vendor> entry that exists but cannot run (broken link, retired shim).
func toolDoctorOK(s tools.Status) bool {
	bin, ok := paths.VendorName(s.Name)
	if !ok {
		return true
	}
	if _, err := os.Lstat(filepath.Join(paths.BinDir(), bin)); err != nil {
		return true
	}
	_, err := paths.VendorBin(bin)
	return err == nil
}

// toolDoctorDetail renders one tool's doctor line: a vendor's bin/ entry, else path + version, then extra PATH copies.
func toolDoctorDetail(s tools.Status) string {
	if bin, ok := paths.VendorName(s.Name); ok {
		return vendorDoctorDetail(s, bin)
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

// vendorDoctorDetail renders BinDir/<bin>: `<dst> → <Target> <version>`, a direct install, or why it is unusable.
func vendorDoctorDetail(s tools.Status, bin string) string {
	dst := filepath.Join(paths.BinDir(), bin)
	if _, err := paths.VendorBin(bin); err != nil {
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
	first, _ := filepath.EvalSymlinks(primary)
	first, _ = filepath.Abs(first)
	var out strings.Builder
	for _, path := range toolPATHCopies(name) {
		real, _ := filepath.EvalSymlinks(path)
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
	self, _ := SelfBinary()
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
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		real, err = filepath.Abs(real)
		if err != nil || real == self || filepath.Base(real) == "claude-code" || seen[real] {
			continue
		}
		seen[real] = true
		copies = append(copies, candidate)
	}
	return copies
}

func toolPATHVersion(path string) string {
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
