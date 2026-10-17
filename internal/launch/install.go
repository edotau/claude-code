package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
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
	if string(merged) == string(existing) {
		fmt.Fprintln(w, "  unchanged")
		syncVendors(w, dryRun)
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  would write %d bytes (was %d); --dry-run wrote nothing\n", len(merged), len(existing))
		syncVendors(w, dryRun)
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
	return nil
}

// syncVendors projects agents/ + skills/ into every vendor home so they track the repo; best-effort, like completion.
func syncVendors(w io.Writer, dryRun bool) {
	fmt.Fprintln(w, "agents   sync agents/ + skills/ into vendor homes (claude-code agents sync)")
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
			check(providers.CredentialPresent(p), session, "credential "+n, label)
		}
	}
	check(true, false, "router", routerStatus())
	check(true, false, "rtk compressor", rtkDoctorStatus())
	has, err := settingsHasHooks(SettingsFile())
	check(true, false, "audit (7d)", auditSummary())
	check(has, false, "settings.json harness hooks", orErr(map[bool]string{true: "present", false: "missing (run claude-code install)"}[has], err))
	return checks, ok
}

// Doctor prints DoctorChecks as ✓/✗ lines and reports whether every hard check passed.
func Doctor(w io.Writer) bool {
	checks, ok := DoctorChecks()
	for _, c := range checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, c.Detail)
	}
	return ok
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
		return "no audited actions (claude-code audit)"
	}
	return strings.Join(parts, ", ") + " (claude-code audit)"
}

// rtkDoctorStatus reports whether the rtk output compressor is on PATH; the PreToolUse hook no-ops without it.
func rtkDoctorStatus() string {
	if bin := hooks.RTKBinary(); bin != "" {
		return bin + " (" + hooks.RTKCommandList() + ")"
	}
	return "not on PATH — rtk-wrap no-ops"
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

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
