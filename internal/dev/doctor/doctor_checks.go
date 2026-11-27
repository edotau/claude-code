package doctor

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/schemas"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/remote/network/browser"
	"github.com/edotau/claude-code/internal/routing/catalog"
	"github.com/edotau/claude-code/internal/routing/inflight"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/router"
)

// localRow is the last-resort row: the cached probe, machine-wide holders, the last fallback and the ranks.json stage.
func localRow() check {
	st := router.LocalStatus()
	at, kind, surface, _ := router.LastLocalFallback("")
	return localCheck(st, at, kind, surface, catalog.Local().Stage)
}

// localCheck renders localRow; down (no row included) is a soft ✗ that drops the fallback hint.
func localCheck(st router.LocalState, at time.Time, kind, surface, stage string) check {
	row := check{Name: "local", OK: st.Up, Detail: "down (" + st.Reason + ")"}
	if st.Up {
		row.Detail = fmt.Sprintf("up · %s · %d running", st.Model, st.Running)
		if !at.IsZero() {
			row.Detail += fmt.Sprintf(" · last fallback %s %s (%s)", at.Local().Format("2006-01-02 15:04"), kind, surface)
		}
	}
	row.Detail += " · stage " + cmp.Or(stage, "unset")
	return row
}

// detectBrowsers reads the host's browsers; a var so tests never wait on the real powershell.exe under WSL.
var detectBrowsers = func() (browser.Report, error) { return browser.DetectDefault(browser.NewEnv()) }

// modsLoadedDetail checks the template env every harness launch fills (mods, auto-compact the compact hooks rely on):
// settings.json must not drop a template mod, and inside a Claude session each template key must reach the env.
func modsLoadedDetail() (bool, string) {
	tmpl := settings.Template()
	want := pluginDirs(tmpl)
	var user []byte
	if b, err := os.ReadFile(settings.SettingsFile()); err == nil { // a missing file sets no dirs
		user = b
	}
	if have := pluginDirs(user); len(have) > 0 {
		var missing []string
		for _, d := range want {
			if !slices.Contains(have, d) {
				missing = append(missing, d)
			}
		}
		if len(missing) > 0 {
			return false, "settings.json env.CLAUDE_CODE_PLUGIN_DIRS lacks " + strings.Join(missing, ", ") +
				` (set it to "` + strings.Join(want, string(filepath.ListSeparator)) + `", then /reload-plugins)`
		}
	}
	if os.Getenv("CLAUDECODE") == "" { // outside a Claude session there is no launch env to check
		return true, "template mods and env keys are not dropped by settings.json"
	}
	var unset []string
	for _, k := range templateEnvKeys(tmpl) {
		if _, ok := os.LookupEnv(k); !ok {
			unset = append(unset, k)
		}
	}
	if len(unset) > 0 {
		return false, "this session lacks " + strings.Join(unset, ", ") + ": start it with the harness `claude` launcher"
	}
	return true, "this session has every template mod and env key"
}

// templateEnvKeys lists the template's env keys in order.
func templateEnvKeys(doc []byte) []string {
	var d struct {
		Env map[string]string `json:"env"`
	}
	_ = json.Unmarshal(doc, &d) // the embedded template always parses
	return slices.Sorted(maps.Keys(d.Env))
}

// pluginDirs reads env.CLAUDE_CODE_PLUGIN_DIRS from a settings document; none when it is absent or unparsable.
func pluginDirs(doc []byte) []string {
	var d struct {
		Env map[string]string `json:"env"`
	}
	_ = json.Unmarshal(doc, &d) // an unparsable file names no dirs
	return filepath.SplitList(d.Env["CLAUDE_CODE_PLUGIN_DIRS"])
}

// strayClaudeJSON flags ~/.claude/.claude.json, which claude reads instead of ~/.claude.json under CLAUDE_CONFIG_DIR.
func strayClaudeJSON() (ok bool, detail string) {
	if !paths.ConfigDirIsDefault() {
		return true, "none (CLAUDE_CONFIG_DIR holds the real .claude.json)"
	}
	stub := filepath.Join(paths.ConfigDir(), ".claude.json")
	if _, err := os.Stat(stub); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, "none (~/.claude.json is the only one)"
		}
		return false, "cannot stat " + stub + ": " + err.Error()
	}
	return false, "stray " + stub + "; move it aside (mv " + stub + "{,.bak})"
}

// hookBinaryStatus checks dir's .hookd exists and embeds the stamp in dir's .build-stamp (ldflags -X keeps it).
func hookBinaryStatus(dir string) (ok bool, detail string) {
	hook := filepath.Join(dir, paths.HookBinaryName)
	if paths.HookBinary(filepath.Join(dir, "claude-code")) != hook {
		return false, hook + " missing or not executable; hooks fall back to claude-code — run make install"
	}
	raw, err := os.ReadFile(filepath.Join(dir, paths.StampFile))
	stamp, _, _ := strings.Cut(strings.TrimSpace(string(raw)), " ") // the file is "<stamp> <size> <mtime>" of claude-code
	if err != nil || stamp == "" {
		return false, "no " + paths.StampFile + " in " + dir + "; hooks fall back to claude-code — run make install"
	}
	bin, err := os.ReadFile(hook)
	if err != nil {
		return false, "cannot read " + hook + ": " + err.Error()
	}
	if !bytes.Contains(bin, []byte(stamp)) {
		return false, hook + " was not built with stamp " + stamp + "; hooks fall back to claude-code — run make install"
	}
	return true, hook + " matches stamp " + stamp
}

var leftoverPatterns = []string{"*.test", "*.prof", "*.pprof", "cpu.out", "mem.out", "*.cpuprofile", "*.memprofile"}

// repoRootCheck warns about go test binaries and profiles in root itself (one directory read, never deletes).
func repoRootCheck(root string) check {
	row := check{Name: "repo root", OK: true, Detail: "no test binaries or profiles"}
	entries, err := os.ReadDir(root)
	if err != nil {
		return check{Name: row.Name, Detail: "warn cannot read " + root + ": " + err.Error()}
	}
	var found []string
	for _, e := range entries {
		if e.Type().IsRegular() && slices.ContainsFunc(leftoverPatterns, func(p string) bool {
			ok, _ := filepath.Match(p, e.Name())
			return ok
		}) {
			found = append(found, e.Name())
		}
	}
	if len(found) == 0 {
		return row
	}
	shown := strings.Join(found[:min(3, len(found))], ", ")
	if len(found) > 3 {
		shown += ", …"
	}
	row.OK = false
	row.Detail = fmt.Sprintf("warn %d leftover file(s): %s; delete them (go test -o <scratch>)", len(found), shown)
	return row
}

// inflightDoctorDetail is the live agent count against the machine cap, then each busy account against its own.
func inflightDoctorDetail() string {
	lim := router.CurrentLimits()
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

// fitDoctorDetail is when the daily learn step last ran and what fit.json has folded (the cached read of the file).
func fitDoctorDetail() string {
	f := catalog.LoadFit()
	if f == nil {
		return "no fit table yet; `claude-code models learn` or the daily catch-up job writes it"
	}
	ran := "never by the daily job"
	if fi, err := os.Stat(filepath.Join(paths.StateDir(), "preflight", "learn-daily")); err == nil {
		ran = "daily job ran " + fi.ModTime().Local().Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("%s; folded through %s, %d cells", ran, f.Folded.Local().Format("2006-01-02 15:04"), len(f.Cells))
}

const dreamStale = 48 * time.Hour
const dreamResultsScan = 200 // match the retained result history so skipped invocations cannot hide a real run

// memoryDreamRow fails when the newest dream failed or no ok/noop dream finished within 48 hours.
func memoryDreamRow(now time.Time) check {
	row := check{Name: "memory dream"}
	rs, err := memory.LastDreamResults(dreamResultsScan)
	if err != nil {
		row.Detail = textutil.TruncateCells("cannot read dream results: "+err.Error(), 80, "…")
		return row
	}
	if len(rs) == 0 {
		row.Detail = "no record yet (memory update --dream --force)"
		return row
	}
	at := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	for _, r := range rs {
		if !dreamCompleted(r) {
			continue
		}
		if r.Outcome == memory.DreamFailed {
			row.Detail = dreamFailureDetail(r)
			return row
		}
		if now.Sub(r.At) > dreamStale {
			row.Detail = "no ok run since " + at(r.At) + " (>48h)"
			return row
		}
		row.OK = true
		row.Detail = "last ok " + at(r.At) + " on " + dreamRoute(r) + " (" + r.Trigger + ")"
		if r.Local {
			row.Detail += ", local model"
		}
		row.Detail = textutil.TruncateCells(row.Detail, 80, "…")
		return row
	}
	row.Detail = "no ok run yet (memory update --dream --force)"
	return row
}

// dreamCompleted ignores skipped attempts; only runs that actually finished count toward health.
func dreamCompleted(r memory.DreamResult) bool {
	return r.Outcome == memory.DreamFailed || r.Outcome == memory.DreamOK ||
		(r.Outcome == memory.DreamNoop && r.Reason != "not due")
}

// dreamFailureDetail includes the recorded reason and the log name without printing an unbounded line.
func dreamFailureDetail(r memory.DreamResult) string {
	detail := "last run failed " + r.At.Local().Format("2006-01-02 15:04") + ": "
	suffix := ""
	if r.Log != "" {
		suffix = "; see " + textutil.TruncateCells(filepath.Base(r.Log), 22, "…")
	}
	if r.Local {
		suffix += ", local model"
	}
	reason, _, _ := strings.Cut(strings.TrimSpace(r.Reason), "\n")
	if reason == "" {
		reason = "error"
	}
	room := 80 - textutil.DisplayWidth(detail) - textutil.DisplayWidth(suffix)
	return detail + textutil.TruncateCells(reason, room, "…") + suffix
}

// dreamRoute names the recorded agent and profile without inventing a route for skipped dreams.
func dreamRoute(r memory.DreamResult) string {
	if r.Agent == "" {
		return "unknown route"
	}
	if r.Profile == "" {
		return r.Agent
	}
	return r.Agent + "·" + r.Profile
}

// quotaDoctorRows is one row per exhausted or low account in route's quota.json, with its reset and the manual fix.
func quotaDoctorRows(now time.Time) []check {
	st := router.LoadState()
	var rows []check
	for _, k := range slices.Sorted(maps.Keys(st)) {
		e := st[k]
		s := e.Status(now)
		if s != router.StateExhausted && s != router.StateLow {
			continue
		}
		d := fmt.Sprintf("%s at %.0f%%", s, e.UsedPct)
		if u := e.Until(now); !u.IsZero() {
			d += ", resets " + u.Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, check{Name: "quota " + k, Detail: d + "; " + quotaFix(k)})
	}
	rows = append(rows, check{Name: "quota gemini", OK: true,
		Detail: "failure-driven only (Antigravity's quota endpoint denies access)"})
	return rows
}

// quotaFix is the manual remedy; the router itself never changes accounts.
func quotaFix(key string) string {
	switch {
	case strings.HasPrefix(key, "github·"):
		return "routed around; only `claude-code configure agents --agent copilot` changes the Copilot account"
	case strings.HasPrefix(key, "opencode·"):
		return "routed around; `opencode auth login` changes opencode's login"
	}
	return "routed around until the reset; `claude-code gateway` shows the pick"
}

func formatsCheck() check {
	var stale []string
	current := 0
	for _, f := range schemas.All() {
		if f.Kind != schemas.Wire {
			current++
		}
		if f.Kind != schemas.Store || f.Probe == nil {
			continue
		}
		v, err := f.Probe()
		if err != nil {
			stale = append(stale, fmt.Sprintf("%s: unreadable (%v)", f.Name, err))
			continue
		}
		if v != f.Version && v != 0 {
			stale = append(stale, fmt.Sprintf("%s: schema %d (want %d)", f.Name, v, f.Version))
		}
	}
	if len(stale) > 0 {
		return check{Name: "formats", OK: false, Detail: strings.Join(stale, " · ")}
	}
	return check{Name: "formats", OK: true, Detail: fmt.Sprintf("%d formats current", current)}
}

func vaultDoctorChecks() []check {
	row := check{Name: "vault", OK: true}
	retired := 0
	_, err := os.Stat(vault.Path())
	if errors.Is(err, fs.ErrNotExist) {
		row.Detail = "not created yet"
	} else {
		if err == nil {
			err = vault.View(func(t *vault.Tx) error {
				kids, err := vault.Retired(t)
				if err != nil {
					return err
				}
				retired = len(kids)
				return nil
			})
		}
		count, source := 0, "not needed"
		if err == nil {
			var kerr error
			count, source, kerr = vault.MasterKeyStatus()
			err = kerr
		}
		row.Detail = fmt.Sprintf("%s · key %s · %d secret(s) · %d retired", vault.Path(), source, count, retired)
		if err != nil {
			row.OK = false
			row.Detail += ": " + err.Error()
		}
	}
	// "vault master key" repeats the "vault" row; drop it here but keep it in secretsDoctorChecks for printVault.
	checks, _ := secretsDoctorChecks()
	checks = slices.DeleteFunc(checks, func(c check) bool { return c.Name == "vault master key" })
	return append([]check{row}, checks...)
}

// secretsDoctorChecks adapts vaultChecks to soft doctor rows; gateOK is false when a gating row failed.
func secretsDoctorChecks() (checks []check, gateOK bool) {
	reg, _ := providers.Load() // a load error is reported by the provider registry row; nil skips name checks
	gateOK = true
	for _, c := range vaultChecks(reg) {
		checks = append(checks, check{Name: c.Name, OK: c.OK, Detail: c.Detail})
		if c.Gate && !c.OK {
			gateOK = false
		}
	}
	return checks, gateOK
}

// printVault prints the vault rows as ✓/✗ lines for doctor --secrets; false when a gating row failed.
func printVault(w io.Writer) bool {
	checks, ok := secretsDoctorChecks()
	fmt.Fprintln(w, "== vault checks ==")
	printCheckLines(w, checks)
	return ok
}

// memoryStoreRow reports refusing stamps without initialising or changing the store.
func memoryStoreRow() check {
	row := check{Name: "memory store", OK: true, Detail: "layout ok"}
	raw, err := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), ".layout"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return row
	case err != nil:
		return check{Name: row.Name, Detail: ".layout unreadable"}
	}
	v, err := memory.StoreLayoutVersion(string(raw))
	switch {
	case err != nil:
		return check{Name: row.Name, Detail: ".layout unparseable; writes refused"}
	case v > memory.StoreVersion():
		return check{Name: row.Name, Hard: true, Detail: fmt.Sprintf("v%d>%d; binary older than store; writes refused", v, memory.StoreVersion())}
	case v == memory.StoreVersion():
		row.Detail = fmt.Sprintf("flat %d matches code", v)
	}
	return row
}
