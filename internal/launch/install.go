package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/history"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/tools"
	"github.com/edotau/claude-code/internal/vendors"
)

// ShimNames are the bare names install links to the harness binary.
var ShimNames = []string{"claude", "codex", "gemini", "opencode", "copilot"}

// userBinDir is where install links claude-code onto PATH (~/.local/bin is on PATH by convention).
var userBinDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

// SettingsFile is the live Claude Code settings.json under the config dir.
func SettingsFile() string { return filepath.Join(paths.ConfigDir(), "settings.json") }

// Install copies the binary into BinDir, links the shims, and merges the harness settings (backing up first).
func Install(w io.Writer, dryRun bool) error {
	if !dryRun { // a vendor or hand edit about to be overwritten stays recoverable via `history diff`
		if r, err := history.SnapshotConfig("pre-install"); err != nil {
			fmt.Fprintf(w, "history  snapshot failed: %v\n", err)
		} else if r.Commit != "" {
			fmt.Fprintf(w, "history  %s (pre-install snapshot)\n", r.Commit)
		}
	}
	self, err := selfBinary()
	if err != nil {
		return err
	}
	bin := paths.BinDir()
	target := filepath.Join(bin, "claude-code")
	if !sameFile(self, target) {
		fmt.Fprintf(w, "copy     %s → %s\n", self, target)
		if !dryRun {
			if err := copyBinary(self, target); err != nil {
				return err
			}
		}
	}
	for _, name := range ShimNames {
		link := filepath.Join(bin, name)
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			fmt.Fprintf(w, "skip     %s (a real file is there)\n", link)
			continue
		}
		fmt.Fprintf(w, "link     %s → claude-code\n", link)
		if !dryRun {
			_ = os.Remove(link)
			if err := os.Symlink("claude-code", link); err != nil {
				return err
			}
		}
	}
	// Bash tab-completion: best-effort — an unwritable completions dir (no bash-completion package,
	// read-only $HOME) must never fail the install that got the binary and shims in place.
	switch {
	case dryRun:
		fmt.Fprintln(w, "completion would install bash tab-completion (skipped: --dry-run)")
	default:
		if path, err := completion.Install("claude-code", ShimNames); err != nil {
			fmt.Fprintf(w, "completion skip  %v\n", err)
		} else {
			fmt.Fprintf(w, "completion %s\n", path)
		}
	}
	seedTmuxConf(w, dryRun)
	moveProvidersFile(w, dryRun)
	if err := linkOnPath(w, target, dryRun); err != nil {
		return err
	}
	file := SettingsFile()
	existing, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	rendered, err := settings.Render()
	if err != nil {
		return err
	}
	merged, sum, err := settings.Merge(existing, rendered)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "settings %s\n  keep   %s\n  merge  %s\n  hooks  %d harness + %d foreign kept\n",
		file, strings.Join(sum.Kept, ", "), strings.Join(sum.Owned, ", "), sum.HarnessHooks, sum.ForeignHooks)
	if len(sum.Retired) > 0 {
		fmt.Fprintf(w, "  drop   %s (retired)\n", strings.Join(sum.Retired, ", "))
	}
	if string(merged) == string(existing) {
		fmt.Fprintln(w, "  unchanged")
		syncVendors(w, dryRun)
		installGitHooks(w, dryRun)
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  would write %d bytes (was %d); --dry-run wrote nothing\n", len(merged), len(existing))
		syncVendors(w, dryRun)
		installGitHooks(w, dryRun)
		return nil
	}
	backup, err := settings.WriteWithBackup(file, merged)
	if backup != "" {
		fmt.Fprintf(w, "  backup %s\n", backup)
	}
	if err != nil {
		return err
	}
	syncVendors(w, dryRun)
	installGitHooks(w, dryRun)
	return nil
}

// moveProvidersFile moves a pre-env.d ~/.claude/providers.json into env.d once; both present keeps both and warns.
func moveProvidersFile(w io.Writer, dryRun bool) {
	old, dst := filepath.Join(paths.ConfigDir(), "providers.json"), providers.UserFile()
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		fmt.Fprintf(w, "providers %s and %s both exist; only the env.d one is read — merge and delete the old one\n", old, dst)
		return
	}
	fmt.Fprintf(w, "move     %s → %s\n", old, dst)
	if dryRun {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err == nil {
		if err := os.Rename(old, dst); err != nil {
			fmt.Fprintf(w, "providers move failed: %v\n", err)
		}
	}
}

// installGitHooks points core.hooksPath at the repo's .githooks (pre-push runs the CI gate); best-effort.
func installGitHooks(w io.Writer, dryRun bool) {
	root := paths.ConfigDir()
	if _, err := os.Stat(filepath.Join(root, ".githooks")); err != nil {
		return
	}
	cur, _ := exec.Command("git", "-C", root, "config", "--get", "core.hooksPath").Output()
	if strings.TrimSpace(string(cur)) == ".githooks" {
		return
	}
	fmt.Fprintln(w, "githooks core.hooksPath → .githooks (pre-push runs make audit)")
	if dryRun {
		return
	}
	if out, err := exec.Command("git", "-C", root, "config", "core.hooksPath", ".githooks").CombinedOutput(); err != nil {
		fmt.Fprintf(w, "  githooks skip  %v %s\n", err, strings.TrimSpace(string(out)))
	}
}

// syncVendors projects agents/, skills/ and the vendor instruction files into every vendor home so they track the
// repo; best-effort, like completion.
func syncVendors(w io.Writer, dryRun bool) {
	fmt.Fprintln(w, "agents   sync agents/ + skills/ + instructions into vendor homes (claude-code agents sync)")
	if rc := crossgen.Sync(crossgen.SyncOptions{DryRun: dryRun, Out: io.Discard}); rc != 0 {
		fmt.Fprintf(w, "  agents sync exit %d — run `claude-code agents sync` for details\n", rc)
	}
}

// linkOnPath points userBinDir/claude-code at the installed binary; only claude-code, so the bare
// claude/codex/... shims never shadow the real CLIs. A real file there is left alone.
func linkOnPath(w io.Writer, target string, dryRun bool) error {
	dir := userBinDir()
	link := filepath.Join(dir, "claude-code")
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		fmt.Fprintf(w, "skip     %s (a real file is there)\n", link)
		return nil
	}
	fmt.Fprintf(w, "link     %s → %s\n", link, target)
	if !strings.Contains(string(os.PathListSeparator)+os.Getenv("PATH")+string(os.PathListSeparator),
		string(os.PathListSeparator)+dir+string(os.PathListSeparator)) {
		fmt.Fprintf(w, "warning  %s is not on PATH; add it in your shell profile\n", dir)
	}
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_ = os.Remove(link)
	return os.Symlink(target, link)
}

// sameFile is true when target already is, or holds the same bytes as, self.
func sameFile(self, target string) bool {
	if real, _ := filepath.EvalSymlinks(target); real == self {
		return true
	}
	a, err1 := os.ReadFile(self)
	b, err2 := os.ReadFile(target)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

func copyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(dst, data, 0o755)
}

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
	check(err == nil, true, "provider registry", orErr(providers.UserFile(), err))
	if reg != nil {
		sel, serr := providers.Select(reg, "")
		for _, n := range reg.Names() {
			p := reg.Providers[n]
			session := serr == nil && sel.Provider == p
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
				if alt := altAuth(reg, n); alt != "" {
					if !strings.Contains(label, "REJECTED") {
						label += " · no key"
					}
					present, label = true, label+" · "+alt
				}
			}
			check(present, session, "credential "+n, label)
		}
		check(true, false, "opencode route", opencodeRouteDetail(reg, sel, serr))
		check(true, false, "copilot route", copilotRouteDetail(reg, serr))
	}
	for _, s := range tools.StatusAll() {
		check(true, false, "tool "+s.Name, toolDoctorDetail(s))
	}
	check(true, false, "router", routerStatus())
	check(true, false, "rtk compressor", rtkDoctorStatus())
	tsOK, tsDetail := tailscale.Doctor(context.Background())
	check(tsOK, false, "tailscale", tsDetail)
	has, err := settingsHasHooks(SettingsFile())
	nested := clean.NestedConfig()
	check(nested.Dir == "" || nested.OnlyWorktrees(), false, "nested config", nestedDetail(nested))
	check(true, false, "audit (7d)", auditSummary())
	check(true, false, "setup", setupDoctorDetail())
	check(true, false, "browser", browserDoctorDetail())
	clipOK, clipDetail := clipboard.DoctorDetail()
	check(clipOK, false, "wl-paste proxy", clipDetail)
	check(has, false, "settings.json harness hooks", orErr(map[bool]string{true: "present", false: "missing (run claude-code install)"}[has], err))
	return checks, ok
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
	return "not on PATH — rtk-wrap no-ops"
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
	return fmt.Sprintf("provider=github-copilot model=github-copilot/%s small_model=github-copilot/%s file=%s opencode_login=%v copilot_cli_login=%v",
		mainID, smallID, file, opencodeLogin, copilotCLILogin)
}

// copilotRouteDetail reports the copilot CLI's resolved leg (DescribeRoute): its own GitHub login,
// BYOK with an anthropic or gemini key, or none — names/booleans/leg only, never a credential.
func copilotRouteDetail(reg *providers.Registry, selErr error) string {
	if selErr != nil {
		return fmt.Sprintf("(%v)", selErr)
	}
	ri := vendors.DescribeRoute(reg, "copilot")
	if !ri.OK {
		return ri.Reason
	}
	return fmt.Sprintf("leg=%s provider=%s model=%s (%s)", ri.Leg, ri.Provider, ri.Model, ri.Reason)
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
		return "stale router.json (port " + fmt.Sprint(st.Port) + " not answering)"
	}
	resp.Body.Close()
	return fmt.Sprintf("port %d %s", st.Port, resp.Status)
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

// toolDoctorDetail renders one tool's doctor line: path + version, flagged when it's an npm-global
// copy that `claude-code install --tools` would retire, or a report-only hint when none is found.
func toolDoctorDetail(s tools.Status) string {
	if s.Path == "" {
		if s.Hint != "" {
			return "not found (" + s.Hint + ")"
		}
		return "not found (claude-code install --tools=" + s.Name + ")"
	}
	detail := s.Path
	if s.Version != "" {
		detail += " " + s.Version
	}
	switch {
	case s.NPMGlobal:
		detail += " (npm-global; claude-code install --tools retires this)"
	case s.LocalBin:
		detail += " (~/.local/bin)"
	}
	return detail
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
